//go:build linux

// This file holds the iptables applier for the mesh firewall. It depends on
// github.com/coreos/go-iptables, which is Linux-only (it uses syscall.Flock and
// friends and does not compile on Windows). The agent only ever runs on Linux,
// so the applier is gated to linux; the pure MeshNetworkPolicy compiler in
// plan.go/interpret.go stays cross-platform so the read-only CLIs (dwxctl,
// dwx-mcp) — which import meshfw only for that compiler — build on darwin and
// windows too.
package meshfw

import (
	"fmt"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"github.com/go-logr/logr"
)

// hookChains are the built-in filter chains we jump FWChain from. FORWARD
// covers pod → pod (routed) traffic; INPUT covers traffic terminating on the node
// itself (e.g. hostNetwork pods, the node's own services).
var hookChains = []string{"FORWARD", "INPUT"}

// Manager programs the mesh firewall (FWChain) into the kernel's iptables
// filter table, matched on packets arriving via the WireGuard device. Like the
// NAT manager, every Sync is a full-state rebuild — idempotent and self-healing
// against drift. Needs root + the iptables binary; exercised by data-plane
// tests.
//
// IPv4 only for now the planner skips non-IPv4 inputs. The structure mirrors
// nat.Manager so an ip6tables handle can be added alongside dual stack.
type Manager struct {
	// mu serializes SyncFirewall so concurrent reconciles can never interleave a
	// chain flush with another's repopulate (which would corrupt FWChain).
	mu      sync.Mutex
	ipt     *iptables.IPTables
	wgIface string
	log     logr.Logger
}

// NewManager opens the iptables handle and installs FWChain and the interface
// hooks. wgIface is the mesh device name (e.g. dwx-mesh0) whose ingress the
// firewall guards.
func NewManager(wgIface string, log logr.Logger) (*Manager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("meshfw: opening iptables: %w", err)
	}
	m := &Manager{ipt: ipt, wgIface: wgIface, log: log.WithName("meshfw")}
	if err := m.ensureInfra(); err != nil {
		return nil, err
	}
	return m, nil
}

// ensureInfra makes sure FWChain and the rebuild-guard chain exist and that
// FWChain is jumped to from the hook chains for traffic arriving on the mesh
// device.
func (m *Manager) ensureInfra() error {
	if err := m.ensureChain(FWChain); err != nil {
		return err
	}
	if err := m.ensureGuardChain(); err != nil {
		return err
	}
	for _, hook := range hookChains {
		if err := m.ensureHook(hook); err != nil {
			return err
		}
	}
	return nil
}

// ensureGuardChain makes sure FWChainGuard exists and holds exactly a blanket
// DROP, so diverting mesh ingress to it during a rebuild fails closed.
func (m *Manager) ensureGuardChain() error {
	if err := m.ensureChain(FWChainGuard); err != nil {
		return err
	}
	exists, err := m.ipt.Exists(TableFilter, FWChainGuard, "-j", "DROP")
	if err != nil {
		return fmt.Errorf("meshfw: checking guard DROP: %w", err)
	}
	if !exists {
		if err := m.ipt.Append(TableFilter, FWChainGuard, "-j", "DROP"); err != nil {
			return fmt.Errorf("meshfw: installing guard DROP: %w", err)
		}
	}
	return nil
}

func (m *Manager) ensureChain(chain string) error {
	exists, err := m.ipt.ChainExists(TableFilter, chain)
	if err != nil {
		return fmt.Errorf("meshfw: checking chain %s: %w", chain, err)
	}
	if !exists {
		if err := m.ipt.NewChain(TableFilter, chain); err != nil {
			return fmt.Errorf("meshfw: creating chain %s: %w", chain, err)
		}
	}
	return nil
}

// ensureHook inserts `-i <wgIface> -j FWChain` at the top of a built-in chain
// if it isn't already present.
func (m *Manager) ensureHook(builtin string) error {
	spec := []string{"-i", m.wgIface, "-j", FWChain}
	exists, err := m.ipt.Exists(TableFilter, builtin, spec...)
	if err != nil {
		return fmt.Errorf("meshfw: checking %s hook: %w", builtin, err)
	}
	if !exists {
		if err := m.ipt.Insert(TableFilter, builtin, 1, spec...); err != nil {
			return fmt.Errorf("meshfw: installing %s hook: %w", builtin, err)
		}
	}
	return nil
}

// SyncFirewall rebuilds FWChain to exactly realize rs.Rules.
//
// go-iptables exposes no atomic flush+repopulate (no iptables-restore), so a
// naive ClearChain-then-Append would leave FWChain momentarily empty — and for a
// default-deny firewall an empty chain means mesh ingress falls through and is
// ACCEPTed, i.e. it fails OPEN on every reconcile. To fail CLOSED instead, mesh
// ingress is diverted to the blanket-DROP guard chain for the duration of the
// rebuild and only restored once FWChain is fully repopulated. If the rebuild
// errors, the guard is deliberately left engaged (traffic stays denied) and the
// controller retries, rather than exposing a half-built chain.
func (m *Manager) SyncFirewall(rs Ruleset) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureInfra(); err != nil {
		return err
	}
	if err := m.engageGuard(); err != nil {
		return err
	}
	if err := m.ipt.ClearChain(TableFilter, FWChain); err != nil {
		return fmt.Errorf("meshfw: flushing %s: %w", FWChain, err)
	}
	for _, r := range rs.Rules {
		if err := m.ipt.Append(TableFilter, r.Chain, r.Args...); err != nil {
			return fmt.Errorf("meshfw: appending to %s %v: %w", r.Chain, r.Args, err)
		}
	}
	if err := m.disengageGuard(); err != nil {
		return fmt.Errorf("meshfw: lifting rebuild guard: %w", err)
	}
	// V(1): full-state rebuild on every reconcile; debug-level (see pkg/logging).
	m.log.V(1).Info("mesh firewall synced", "iface", m.wgIface, "rules", len(rs.Rules), "skipped", len(rs.Skipped))
	return nil
}

// engageGuard inserts `-i <wgIface> -j FWChainGuard` above the FWChain jump in
// each hook chain, so mesh ingress is dropped while FWChain is rebuilt. It is
// idempotent: a guard left over from a failed prior sync is reused, not stacked.
func (m *Manager) engageGuard() error {
	spec := []string{"-i", m.wgIface, "-j", FWChainGuard}
	for _, hook := range hookChains {
		exists, err := m.ipt.Exists(TableFilter, hook, spec...)
		if err != nil {
			return fmt.Errorf("meshfw: checking %s guard hook: %w", hook, err)
		}
		if !exists {
			if err := m.ipt.Insert(TableFilter, hook, 1, spec...); err != nil {
				return fmt.Errorf("meshfw: engaging %s guard: %w", hook, err)
			}
		}
	}
	return nil
}

// disengageGuard removes the guard jump from each hook chain, restoring normal
// FWChain enforcement. Idempotent.
func (m *Manager) disengageGuard() error {
	spec := []string{"-i", m.wgIface, "-j", FWChainGuard}
	for _, hook := range hookChains {
		exists, err := m.ipt.Exists(TableFilter, hook, spec...)
		if err != nil {
			return fmt.Errorf("meshfw: checking %s guard hook: %w", hook, err)
		}
		if exists {
			if err := m.ipt.Delete(TableFilter, hook, spec...); err != nil {
				return fmt.Errorf("meshfw: disengaging %s guard: %w", hook, err)
			}
		}
	}
	return nil
}

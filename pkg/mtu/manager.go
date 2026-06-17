package mtu

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/go-logr/logr"
)

// Manager programs the TCP-MSS clamp into the kernel's mangle table for both
// address families. Like the nat manager it performs a full-state rebuild of its
// managed chain (idempotent, self-healing against drift) and needs root + the
// iptables binaries; it is exercised by a dataplane-tagged integration test.
type Manager struct {
	mu   sync.Mutex
	ipt  *iptables.IPTables // IPv4
	ipt6 *iptables.IPTables // IPv6; nil when ip6tables is unavailable
	log  logr.Logger
}

// NewManager opens the iptables handles and ensures the managed chain is hooked
// from POSTROUTING. The IPv4 handle is required; IPv6 is best-effort.
func NewManager(log logr.Logger) (*Manager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("mtu: opening iptables (is the iptables binary present?): %w", err)
	}
	m := &Manager{ipt: ipt, log: log.WithName("mtu")}

	if ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6); err != nil {
		m.log.Info("ip6tables unavailable; IPv6 MSS clamp disabled", "err", err.Error())
	} else {
		m.ipt6 = ipt6
	}
	return m, nil
}

// EnsureClamp reconciles the MSS-clamp chain to clamp TCP MSS on traffic
// egressing iface, on both available address families. It is idempotent.
func (m *Manager) EnsureClamp(iface string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.sync(m.ipt, iface); err != nil {
		return err
	}
	if m.ipt6 != nil {
		if err := m.sync(m.ipt6, iface); err != nil {
			return err
		}
	}
	return nil
}

// sync rebuilds MSSChain on one handle and ensures it is hooked from POSTROUTING.
func (m *Manager) sync(ipt *iptables.IPTables, iface string) error {
	if err := m.ensureChain(ipt, MSSChain); err != nil {
		return err
	}
	if err := m.ensureHook(ipt, "POSTROUTING", MSSChain); err != nil {
		return err
	}
	if err := ipt.ClearChain(TableMangle, MSSChain); err != nil {
		return fmt.Errorf("mtu: flushing %s: %w", MSSChain, err)
	}
	for _, r := range BuildClampRules(iface) {
		if err := ipt.Append(TableMangle, r.Chain, r.Args...); err != nil {
			return fmt.Errorf("mtu: appending to %s %v: %w", r.Chain, r.Args, err)
		}
	}
	// V(1): re-applied on every ensure pass; debug-level (see pkg/logging).
	m.log.V(1).Info("mesh MSS clamp synced", "family", familyName(ipt), "iface", iface)
	return nil
}

func (m *Manager) ensureChain(ipt *iptables.IPTables, chain string) error {
	exists, err := ipt.ChainExists(TableMangle, chain)
	if err != nil {
		return fmt.Errorf("mtu: checking chain %s: %w", chain, err)
	}
	if !exists {
		if err := ipt.NewChain(TableMangle, chain); err != nil {
			return fmt.Errorf("mtu: creating chain %s: %w", chain, err)
		}
	}
	return nil
}

func (m *Manager) ensureHook(ipt *iptables.IPTables, builtin, target string) error {
	exists, err := ipt.Exists(TableMangle, builtin, "-j", target)
	if err != nil {
		return fmt.Errorf("mtu: checking %s hook: %w", builtin, err)
	}
	if !exists {
		if err := ipt.Append(TableMangle, builtin, "-j", target); err != nil {
			return fmt.Errorf("mtu: installing %s hook: %w", builtin, err)
		}
	}
	return nil
}

func familyName(ipt *iptables.IPTables) string {
	if ipt.Proto() == iptables.ProtocolIPv6 {
		return "ipv6"
	}
	return "ipv4"
}

// ClampPlane is the seam the Ensurer depends on, so its scheduling logic is
// unit-testable with a fake. *Manager satisfies it.
type ClampPlane interface {
	EnsureClamp(iface string) error
}

// Ensurer is a manager.Runnable that keeps the MSS clamp in place for the mesh
// interface. It re-ensures on an interval so a flush by the CNI or a host
// firewall reload self-heals without needing to watch kernel state.
type Ensurer struct {
	// Iface is the mesh interface whose egress TCP MSS is clamped.
	Iface string
	// Plane applies the clamp.
	Plane ClampPlane
	// Interval is the re-ensure cadence; defaults to 60s when <= 0.
	Interval time.Duration
	// Log is optional.
	Log logr.Logger
}

// NeedLeaderElection makes the Ensurer run on every node: each node clamps its
// own mesh egress.
func (e *Ensurer) NeedLeaderElection() bool { return false }

// Start ensures the clamp immediately, then re-ensures on the interval until the
// context is cancelled. A transient failure is logged, not fatal.
func (e *Ensurer) Start(ctx context.Context) error {
	if e.Iface == "" {
		return fmt.Errorf("mtu: no mesh interface configured")
	}
	if e.Plane == nil {
		return fmt.Errorf("mtu: no clamp plane configured")
	}
	interval := e.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}

	e.ensure()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			e.ensure()
		}
	}
}

func (e *Ensurer) ensure() {
	if err := e.Plane.EnsureClamp(e.Iface); err != nil {
		e.Log.Error(err, "ensuring mesh MSS clamp", "iface", e.Iface)
		return
	}
	e.Log.V(1).Info("mesh MSS clamp ensured", "iface", e.Iface)
}

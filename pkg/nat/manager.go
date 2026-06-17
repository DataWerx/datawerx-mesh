package nat

import (
	"fmt"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"github.com/go-logr/logr"
)

// hookChains are the built-in nat chains we hook our root chain into so that
// both pod-originated (PREROUTING) and node/host-originated (OUTPUT) traffic to
// a ClusterSetIP is intercepted before routing.
var hookChains = []string{"PREROUTING", "OUTPUT"}

// errFlushChain is the wrap format used when clearing one of our managed chains.
const errFlushChain = "nat: flushing %s: %w"

// iptablesRules is the subset of *iptables.IPTables the manager uses. Depending
// on it (rather than the concrete type) lets the apply layer be unit-tested with
// a recording fake, without root or the iptables binary. *iptables.IPTables
// satisfies it directly.
type iptablesRules interface {
	ChainExists(table, chain string) (bool, error)
	NewChain(table, chain string) error
	Exists(table, chain string, rulespec ...string) (bool, error)
	Insert(table, chain string, pos int, rulespec ...string) error
	ClearChain(table, chain string) error
	Append(table, chain string, rulespec ...string) error
	ListChains(table string) ([]string, error)
	DeleteChain(table, chain string) error
	Proto() iptables.Protocol
}

// Manager programs the ClusterSetIP DNAT/load-balancing and overlap NETMAP rules
// into the kernel's iptables nat table. It drives both an IPv4 (`iptables`) and,
// when available, an IPv6 (`ip6tables`) handle, dispatching each rule to the
// table matching its address family. All rule computation happens in the pure
// planners (BuildRuleset / BuildRemapRules); this type only applies the result.
//
// Each Sync performs a full-state rebuild of our managed chains: idempotent and
// self-healing against drift — the right bias for a data plane that must never
// silently misroute. Needs root + the iptables binaries; exercised by
// integration tests.
type Manager struct {
	mu   sync.Mutex
	ipt  iptablesRules // IPv4
	ipt6 iptablesRules // IPv6; nil when ip6tables is unavailable
	log  logr.Logger
}

// NewManager opens the iptables handles and installs the root chain + hooks. The
// IPv4 handle is required; the IPv6 handle is best-effort.  It's logged and skipped if
// ip6tables is unavailable, so an IPv4-only host still works.
func NewManager(log logr.Logger) (*Manager, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, fmt.Errorf("nat: opening iptables (is the iptables binary present?): %w", err)
	}
	m := &Manager{ipt: ipt, log: log.WithName("nat")}

	if ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6); err != nil {
		m.log.Info("ip6tables unavailable; IPv6 NAT disabled", "err", err.Error())
	} else {
		m.ipt6 = ipt6
	}

	if err := m.ensureInfra(m.ipt); err != nil {
		return nil, err
	}
	if m.ipt6 != nil {
		if err := m.ensureInfra(m.ipt6); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// handleFor returns the iptables handle for an address family, or nil if that
// family is unavailable - as in IPv6 without ip6tables.
func (m *Manager) handleFor(ipv6 bool) iptablesRules {
	if ipv6 {
		return m.ipt6
	}
	return m.ipt
}

// ensureInfra makes sure RootChain exists and is jumped to from the hook chains.
func (m *Manager) ensureInfra(ipt iptablesRules) error {
	if err := m.ensureChain(ipt, RootChain); err != nil {
		return err
	}
	for _, hook := range hookChains {
		if err := m.ensureHook(ipt, hook, RootChain); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) ensureChain(ipt iptablesRules, chain string) error {
	exists, err := ipt.ChainExists(TableNAT, chain)
	if err != nil {
		return fmt.Errorf("nat: checking chain %s: %w", chain, err)
	}
	if !exists {
		if err := ipt.NewChain(TableNAT, chain); err != nil {
			return fmt.Errorf("nat: creating chain %s: %w", chain, err)
		}
	}
	return nil
}

func (m *Manager) ensureHook(ipt iptablesRules, builtin, target string) error {
	exists, err := ipt.Exists(TableNAT, builtin, "-j", target)
	if err != nil {
		return fmt.Errorf("nat: checking %s hook: %w", builtin, err)
	}
	if !exists {
		if err := ipt.Insert(TableNAT, builtin, 1, "-j", target); err != nil {
			return fmt.Errorf("nat: installing %s hook: %w", builtin, err)
		}
	}
	return nil
}

// SyncClusterSetNAT reconciles the kernel nat table(s) to exactly realize
// services, dispatching each service to the IPv4 or IPv6 table by its VIP.
func (m *Manager) SyncClusterSetNAT(services []ServiceDNAT) error {
	var v4, v6 []ServiceDNAT
	for _, s := range services {
		if isIPv6(s.VIP) {
			v6 = append(v6, s)
		} else {
			v4 = append(v4, s)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.syncClusterSet(m.ipt, v4); err != nil {
		return err
	}
	if len(v6) > 0 && m.ipt6 == nil {
		m.log.Info("dropping IPv6 ClusterSetIP services: ip6tables unavailable", "count", len(v6))
	} else if m.ipt6 != nil {
		if err := m.syncClusterSet(m.ipt6, v6); err != nil {
			return err
		}
	}
	return nil
}

// syncClusterSet rebuilds the DWX-CLUSTERSET / DWX-SVC- / DWX-SEP- chains on one
// handle from the (single-family) services.
func (m *Manager) syncClusterSet(ipt iptablesRules, services []ServiceDNAT) error {
	rs := BuildRuleset(services)

	if err := m.ensureInfra(ipt); err != nil {
		return err
	}
	if err := ipt.ClearChain(TableNAT, RootChain); err != nil {
		return fmt.Errorf(errFlushChain, RootChain, err)
	}
	if err := m.deleteManagedChains(ipt); err != nil {
		return err
	}
	if err := applyRuleset(ipt, rs); err != nil {
		return err
	}

	// Full-state rebuild on every reconcile; debug-level keeps Info for
	// state changes (see pkg/logging verbosity convention).
	m.log.V(1).Info("clusterset NAT synced", "family", familyName(ipt), "services", len(services), "rules", len(rs.Rules))
	return nil
}

// deleteManagedChains flushes then removes every DWX-SVC-/DWX-SEP- chain left
// over from a previous sync, so the rebuild always starts from a clean slate.
func (m *Manager) deleteManagedChains(ipt iptablesRules) error {
	existing, err := ipt.ListChains(TableNAT)
	if err != nil {
		return fmt.Errorf("nat: listing chains: %w", err)
	}
	var managed []string
	for _, c := range existing {
		if strings.HasPrefix(c, svcChainPrefix) || strings.HasPrefix(c, sepChainPrefix) {
			managed = append(managed, c)
		}
	}
	for _, c := range managed {
		if err := ipt.ClearChain(TableNAT, c); err != nil {
			return fmt.Errorf("nat: flushing chain %s: %w", c, err)
		}
	}
	for _, c := range managed {
		if err := ipt.DeleteChain(TableNAT, c); err != nil {
			return fmt.Errorf("nat: deleting chain %s: %w", c, err)
		}
	}
	return nil
}

// applyRuleset creates the per-service chains and appends every computed rule.
func applyRuleset(ipt iptablesRules, rs Ruleset) error {
	for _, c := range rs.Chains {
		if err := ipt.NewChain(TableNAT, c); err != nil {
			return fmt.Errorf("nat: creating chain %s: %w", c, err)
		}
	}
	for _, r := range rs.Rules {
		if err := ipt.Append(TableNAT, r.Chain, r.Args...); err != nil {
			return fmt.Errorf("nat: appending to %s %v: %w", r.Chain, r.Args, err)
		}
	}
	return nil
}

// SyncRemap reconciles the overlapping-CIDR NETMAP rules to exactly realize the
// given local real⇄virtual entries, dispatching each by address family.
func (m *Manager) SyncRemap(entries []RemapEntry) error {
	var v4, v6 []RemapEntry
	for _, e := range entries {
		if isIPv6(e.Virtual) {
			v6 = append(v6, e)
		} else {
			v4 = append(v4, e)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.syncRemap(m.ipt, v4); err != nil {
		return err
	}
	if len(v6) > 0 && m.ipt6 == nil {
		m.log.Info("dropping IPv6 remap entries: ip6tables unavailable", "count", len(v6))
	} else if m.ipt6 != nil {
		if err := m.syncRemap(m.ipt6, v6); err != nil {
			return err
		}
	}
	return nil
}

// syncRemap rebuilds the two NETMAP chains on one handle from the (single-family)
// entries.
func (m *Manager) syncRemap(ipt iptablesRules, entries []RemapEntry) error {
	rules := BuildRemapRules(entries)

	if err := m.ensureChain(ipt, RemapPreChain); err != nil {
		return err
	}
	if err := m.ensureHook(ipt, "PREROUTING", RemapPreChain); err != nil {
		return err
	}
	if err := m.ensureChain(ipt, RemapPostChain); err != nil {
		return err
	}
	if err := m.ensureHook(ipt, "POSTROUTING", RemapPostChain); err != nil {
		return err
	}

	if err := ipt.ClearChain(TableNAT, RemapPreChain); err != nil {
		return fmt.Errorf(errFlushChain, RemapPreChain, err)
	}
	if err := ipt.ClearChain(TableNAT, RemapPostChain); err != nil {
		return fmt.Errorf(errFlushChain, RemapPostChain, err)
	}
	for _, r := range rules {
		if err := ipt.Append(TableNAT, r.Chain, r.Args...); err != nil {
			return fmt.Errorf("nat: appending to %s %v: %w", r.Chain, r.Args, err)
		}
	}

	m.log.V(1).Info("overlap NETMAP synced", "family", familyName(ipt), "entries", len(entries), "rules", len(rules))
	return nil
}

// SyncMeshNoMasq reconciles the masquerade-exemption chain to exactly realize
// "do not masquerade traffic from these local CIDRs to these remote mesh CIDRs",
// dispatching each rule to the IPv4 or IPv6 table by address family. Like the
// other syncs it is a full-state rebuild: idempotent and self-healing.
func (m *Manager) SyncMeshNoMasq(local, remote []string) error {
	var l4, l6, r4, r6 []string
	for _, c := range local {
		if isIPv6(c) {
			l6 = append(l6, c)
		} else {
			l4 = append(l4, c)
		}
	}
	for _, c := range remote {
		if isIPv6(c) {
			r6 = append(r6, c)
		} else {
			r4 = append(r4, c)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.syncNoMasq(m.ipt, l4, r4); err != nil {
		return err
	}
	if len(r6) > 0 && m.ipt6 == nil {
		m.log.Info("dropping IPv6 masquerade-exemption rules: ip6tables unavailable", "count", len(r6))
	} else if m.ipt6 != nil {
		if err := m.syncNoMasq(m.ipt6, l6, r6); err != nil {
			return err
		}
	}
	return nil
}

// syncNoMasq rebuilds NoMasqChain on one handle from the (single-family) CIDRs.
func (m *Manager) syncNoMasq(ipt iptablesRules, local, remote []string) error {
	rules := BuildNoMasqRules(local, remote)

	if err := m.ensureChain(ipt, NoMasqChain); err != nil {
		return err
	}
	if err := m.ensureHook(ipt, "POSTROUTING", NoMasqChain); err != nil {
		return err
	}
	if err := ipt.ClearChain(TableNAT, NoMasqChain); err != nil {
		return fmt.Errorf(errFlushChain, NoMasqChain, err)
	}
	for _, r := range rules {
		if err := ipt.Append(TableNAT, r.Chain, r.Args...); err != nil {
			return fmt.Errorf("nat: appending to %s %v: %w", r.Chain, r.Args, err)
		}
	}

	m.log.V(1).Info("mesh masquerade-exemption synced", "family", familyName(ipt), "rules", len(rules))
	return nil
}

// SyncGatewayMasq reconciles the remote-access gateway's masquerade chain to
// exactly realize "MASQUERADE traffic from these client source CIDRs to these
// mesh destination CIDRs", dispatching each rule to the IPv4 or IPv6 table by
// address family. Like the other syncs it is a full-state rebuild: idempotent
// and self-healing against drift.
func (m *Manager) SyncGatewayMasq(clientCIDRs, destCIDRs []string) error {
	var c4, c6, d4, d6 []string
	for _, c := range clientCIDRs {
		if isIPv6(c) {
			c6 = append(c6, c)
		} else {
			c4 = append(c4, c)
		}
	}
	for _, d := range destCIDRs {
		if isIPv6(d) {
			d6 = append(d6, d)
		} else {
			d4 = append(d4, d)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.syncGatewayMasq(m.ipt, c4, d4); err != nil {
		return err
	}
	if len(d6) > 0 && m.ipt6 == nil {
		m.log.Info("dropping IPv6 gateway masquerade rules: ip6tables unavailable", "count", len(d6))
	} else if m.ipt6 != nil {
		if err := m.syncGatewayMasq(m.ipt6, c6, d6); err != nil {
			return err
		}
	}
	return nil
}

// syncGatewayMasq rebuilds GatewayMasqChain on one handle from the
// (single-family) client and destination CIDRs.
func (m *Manager) syncGatewayMasq(ipt iptablesRules, clientCIDRs, destCIDRs []string) error {
	rules := BuildGatewayMasqRules(clientCIDRs, destCIDRs)

	if err := m.ensureChain(ipt, GatewayMasqChain); err != nil {
		return err
	}
	if err := m.ensureHook(ipt, "POSTROUTING", GatewayMasqChain); err != nil {
		return err
	}
	if err := ipt.ClearChain(TableNAT, GatewayMasqChain); err != nil {
		return fmt.Errorf(errFlushChain, GatewayMasqChain, err)
	}
	for _, r := range rules {
		if err := ipt.Append(TableNAT, r.Chain, r.Args...); err != nil {
			return fmt.Errorf("nat: appending to %s %v: %w", r.Chain, r.Args, err)
		}
	}

	m.log.V(1).Info("gateway masquerade synced", "family", familyName(ipt), "rules", len(rules))
	return nil
}

func familyName(ipt iptablesRules) string {
	if ipt.Proto() == iptables.ProtocolIPv6 {
		return "ipv6"
	}
	return "ipv4"
}

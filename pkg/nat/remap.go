package nat

import "sort"

const (
	// RemapPreChain holds the inbound destination NETMAP rules, hooked from
	// PREROUTING.  A peer addresses one of our ranges by its virtual CIDR which
	// we translate back to the real local CIDR before routing delivers it.
	RemapPreChain = "DWX-REMAP-PRE"
	// RemapPostChain holds the outbound source NETMAP rules, hooked from
	// POSTROUTING. Our real source CIDR is presented to the mesh as our virtual
	// CIDR so it never collides with an overlapping remote range.
	RemapPostChain = "DWX-REMAP-POST"
)

// RemapEntry is one local real⇄virtual CIDR pair to program as a stateless 1:1
// NETMAP in both directions.
type RemapEntry struct {
	Real    string
	Virtual string
}

// BuildRemapRules expands the local remap entries into the (deterministic,
// de-duplicated, sorted) NETMAP rules. The result is pure and comparable, so
// the planning is unit-testable; the manager only applies it.
//
// For each entry:
//
//	PRE  : -d <virtual> -j NETMAP --to <real>     (inbound dst: virtual → real)
//	POST : -s <real>    -j NETMAP --to <virtual>  (outbound src: real → virtual)
//
// NETMAP is stateless and order-preserving, so a /16 maps 1:1 onto a /16 with no
// conntrack — the two rules compose symmetrically with the peer's mirror rules.
func BuildRemapRules(entries []RemapEntry) []Rule {
	seen := map[RemapEntry]struct{}{}
	uniq := make([]RemapEntry, 0, len(entries))
	for _, e := range entries {
		if e.Real == "" || e.Virtual == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		uniq = append(uniq, e)
	}
	sort.Slice(uniq, func(i, j int) bool {
		if uniq[i].Virtual != uniq[j].Virtual {
			return uniq[i].Virtual < uniq[j].Virtual
		}
		return uniq[i].Real < uniq[j].Real
	})

	rules := make([]Rule, 0, len(uniq)*2)
	for _, e := range uniq {
		rules = append(rules, Rule{
			Chain: RemapPreChain,
			Args:  []string{"-d", e.Virtual, "-j", "NETMAP", "--to", e.Real},
		})
	}
	for _, e := range uniq {
		rules = append(rules, Rule{
			Chain: RemapPostChain,
			Args:  []string{"-s", e.Real, "-j", "NETMAP", "--to", e.Virtual},
		})
	}
	return rules
}

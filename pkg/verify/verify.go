// Package verify contains the pure logic behind `dwxctl verify`: given a
// read-only snapshot of a cluster's DataWerx state, it produces a health Report.
// The CLI (cmd/dwxctl) fetches the snapshot via the Kubernetes API and hands it
// here, so all the decision logic is unit-testable without a cluster.
package verify

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Status is the outcome of a single check.
type Status int

const (
	// StatusPass means the check is healthy.
	StatusPass Status = iota
	// StatusWarn means the check is non-fatal but worth attention.
	StatusWarn
	// StatusFail means the check failed; the mesh is not healthy.
	StatusFail
)

func (s Status) Symbol() string {
	switch s {
	case StatusPass:
		return "✓"
	case StatusWarn:
		return "!"
	default:
		return "✗"
	}
}

func (s Status) String() string {
	switch s {
	case StatusPass:
		return "PASS"
	case StatusWarn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// MarshalText renders the status as its symbolic name ("PASS"/"WARN"/"FAIL")
// rather than the raw iota when a Check is serialized to JSON, so the snapshot
// contract stays readable and stable across reorderings of the enum.
func (s Status) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// UnmarshalText parses the symbolic name back into a Status, so a snapshot
// emitted by `dwxctl snapshot` / `dwx-mcp` round-trips into verify.Snapshot. The
// contract is meant to be re-ingested by a higher layer (e.g. dwx-signal), so
// the text form MarshalText writes must read back symmetrically. An unknown name
// is an error rather than a silent StatusPass, so corrupt input never masquerades
// as healthy.
func (s *Status) UnmarshalText(text []byte) error {
	switch string(text) {
	case "PASS":
		*s = StatusPass
	case "WARN":
		*s = StatusWarn
	case "FAIL":
		*s = StatusFail
	default:
		return fmt.Errorf("unknown verify.Status %q", text)
	}
	return nil
}

// Check is one named result.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report is the ordered set of checks produced by Build.
type Report struct {
	Checks []Check `json:"checks"`
}

func (r *Report) add(name string, st Status, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: st, Detail: detail})
}

// Failed reports whether any check failed (the CLI exits non-zero when true).
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// Write renders the report to w and a one-line summary.
func (r Report) Write(w io.Writer) {
	for _, c := range r.Checks {
		fmt.Fprintf(w, "  %s  %-22s %s\n", c.Status.Symbol(), c.Name, c.Detail)
	}
	var pass, warn, fail int
	for _, c := range r.Checks {
		switch c.Status {
		case StatusPass:
			pass++
		case StatusWarn:
			warn++
		case StatusFail:
			fail++
		}
	}
	fmt.Fprintf(w, "\n%d passed, %d warning(s), %d failed\n", pass, warn, fail)
}

// PeerInfo is a MeshPeer's relevant state.
type PeerInfo struct {
	ClusterID string
	Phase     string
	// LastHandshake is the Unix-epoch-seconds of the peer's most recent WireGuard
	// handshake (0 = never). Used with Inputs.Now to flag stale tunnels.
	LastHandshake int64
}

// ExportInfo is a ServiceExport's relevant state.
type ExportInfo struct {
	Namespace string
	Name      string
	Valid     bool
}

// Inputs is the read-only snapshot the CLI gathers from the cluster.
type Inputs struct {
	RequiredCRDs []string
	PresentCRDs  map[string]bool

	AgentFound   bool
	AgentDesired int
	AgentReady   int

	Peers   []PeerInfo
	Exports []ExportInfo

	ImportsClusterSetIP int
	ImportsHeadless     int

	// Now is the current Unix time (seconds); when > 0 the handshake-liveness
	// check runs. Left 0 by tests that don't exercise staleness.
	Now int64
}

// StaleHandshakeSeconds is how long since a Connected peer's last WireGuard
// handshake before we flag it as stale. WireGuard rekeys roughly every 2 minutes
// while traffic flows, so a Connected peer with no handshake well past that is
// suspect — though an idle tunnel can also legitimately go quiet, which is why
// this is a warning, not a failure.
const StaleHandshakeSeconds int64 = 300

// RequiredCRDs is the canonical set the agent needs.
func RequiredCRDs() []string {
	return []string{
		"meshpeers.networking.datawerx.io",
		"endpointexports.networking.datawerx.io",
		"serviceexports.multicluster.x-k8s.io",
		"serviceimports.multicluster.x-k8s.io",
	}
}

// Check names. Kept as constants so the same label is reused across every
// branch of a check and to keep the rendered report consistent.
const (
	checkCRDs       = "CRDs installed"
	checkAgent      = "Agent DaemonSet"
	checkPeers      = "Mesh peers"
	checkHandshakes = "Tunnel handshakes"
	checkExports    = "Service exports"
	checkImports    = "Service imports"
)

// Build evaluates the snapshot into a Report. Each check is a small helper so
// the per-check decision logic stays independently readable and testable.
func Build(in Inputs) Report {
	var r Report
	r.checkCRDs(in)
	r.checkAgent(in)
	r.checkPeers(in)
	r.checkHandshakes(in)
	r.checkExports(in)
	r.add(checkImports, StatusPass, fmt.Sprintf("%d ClusterSetIP, %d headless", in.ImportsClusterSetIP, in.ImportsHeadless))
	return r
}

func (r *Report) checkCRDs(in Inputs) {
	var missing []string
	for _, c := range in.RequiredCRDs {
		if !in.PresentCRDs[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	if len(missing) == 0 {
		r.add(checkCRDs, StatusPass, fmt.Sprintf("%d/%d present", len(in.RequiredCRDs), len(in.RequiredCRDs)))
		return
	}
	r.add(checkCRDs, StatusFail, "missing: "+strings.Join(missing, ", "))
}

func (r *Report) checkAgent(in Inputs) {
	switch {
	case !in.AgentFound:
		r.add(checkAgent, StatusFail, "not found")
	case in.AgentDesired == 0:
		r.add(checkAgent, StatusWarn, "0 scheduled pods (no matching nodes?)")
	case in.AgentReady == in.AgentDesired:
		r.add(checkAgent, StatusPass, fmt.Sprintf("%d/%d pods ready", in.AgentReady, in.AgentDesired))
	default:
		r.add(checkAgent, StatusFail, fmt.Sprintf("%d/%d pods ready", in.AgentReady, in.AgentDesired))
	}
}

func (r *Report) checkPeers(in Inputs) {
	if len(in.Peers) == 0 {
		r.add(checkPeers, StatusWarn, "no MeshPeers configured")
		return
	}
	var connected, pending, errored int
	for _, p := range in.Peers {
		switch p.Phase {
		case "Connected":
			connected++
		case "Error":
			errored++
		default:
			pending++
		}
	}
	detail := fmt.Sprintf("%d connected, %d pending, %d error", connected, pending, errored)
	st := StatusPass
	if errored > 0 {
		st = StatusFail
	} else if pending > 0 {
		st = StatusWarn
	}
	r.add(checkPeers, st, detail)
}

// checkHandshakes flags Connected peers whose WireGuard tunnel looks dead: no
// handshake ever, or none within StaleHandshakeSeconds. It runs only when the
// snapshot carries a clock (in.Now > 0) and there are Connected peers, so it
// doesn't fire on a freshly created or handshake-less cluster. It is a warning,
// since an idle tunnel can legitimately go quiet.
func (r *Report) checkHandshakes(in Inputs) {
	if in.Now <= 0 {
		return
	}
	var connected int
	var stale []string
	for _, p := range in.Peers {
		if p.Phase != "Connected" {
			continue
		}
		connected++
		if p.LastHandshake <= 0 || in.Now-p.LastHandshake > StaleHandshakeSeconds {
			stale = append(stale, p.ClusterID)
		}
	}
	if connected == 0 {
		return // nothing to assess; checkPeers already covered the set
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		r.add(checkHandshakes, StatusWarn,
			fmt.Sprintf("%d/%d stale (idle or unreachable): %s", len(stale), connected, strings.Join(stale, ", ")))
		return
	}
	r.add(checkHandshakes, StatusPass, fmt.Sprintf("%d/%d recent", connected, connected))
}

func (r *Report) checkExports(in Inputs) {
	if len(in.Exports) == 0 {
		r.add(checkExports, StatusPass, "none")
		return
	}
	var invalid []string
	for _, e := range in.Exports {
		if !e.Valid {
			invalid = append(invalid, e.Namespace+"/"+e.Name)
		}
	}
	sort.Strings(invalid)
	if len(invalid) > 0 {
		r.add(checkExports, StatusWarn, fmt.Sprintf("%d invalid: %s", len(invalid), strings.Join(invalid, ", ")))
		return
	}
	r.add(checkExports, StatusPass, fmt.Sprintf("%d valid", len(in.Exports)))
}

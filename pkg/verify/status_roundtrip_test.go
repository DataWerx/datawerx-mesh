package verify

import (
	"encoding/json"
	"testing"
)

// TestStatusRoundTrip locks the snapshot contract: a status serialized by
// MarshalText must read back through UnmarshalText to the same value, so a
// snapshot emitted by `dwx mesh`/`dwx mcp` can be re-ingested (e.g. by `dwx signal`).
func TestStatusRoundTrip(t *testing.T) {
	for _, s := range []Status{StatusPass, StatusWarn, StatusFail} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %v: %v", s, err)
		}
		var got Status
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got != s {
			t.Errorf("round-trip %v -> %s -> %v", s, b, got)
		}
	}
}

func TestStatusUnmarshalRejectsUnknown(t *testing.T) {
	var s Status
	if err := s.UnmarshalText([]byte("BOGUS")); err == nil {
		t.Fatal("expected an error for an unknown status name")
	}
}

// TestSnapshotJSONRoundTrip ensures a full snapshot survives a marshal/unmarshal
// cycle — the exact path dwx signal uses when reading a snapshot file.
func TestSnapshotJSONRoundTrip(t *testing.T) {
	snap := BuildSnapshot(SnapshotInputs{
		Now:          1000,
		RequiredCRDs: []string{"meshpeers.networking.datawerx.io"},
		PresentCRDs:  map[string]bool{"meshpeers.networking.datawerx.io": true},
		AgentFound:   true,
		AgentDesired: 1,
		AgentReady:   1,
		Peers:        []PeerSnapshot{{ClusterID: "a", Phase: "Connected", LastHandshake: 990}},
	})
	b, err := snap.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var back Snapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("snapshot does not round-trip: %v", err)
	}
	if len(back.Health.Checks) == 0 {
		t.Fatal("expected health checks to survive the round-trip")
	}
}

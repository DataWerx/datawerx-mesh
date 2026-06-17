package topology_test

import (
	"net"
	"reflect"
	"strings"
	"testing"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/topology"
)

func TestPlanPeer(t *testing.T) {
	tests := []struct {
		name       string
		spec       networkingv1alpha1.MeshPeerSpec
		localCIDRs []string
		want       topology.Plan
	}{
		{
			name: "no conflicts -> Connected with all routed",
			spec: networkingv1alpha1.MeshPeerSpec{
				PublicKey:    "key-frankfurt",
				Endpoint:     "203.0.113.10:51820",
				PodCIDRs:     []string{"10.50.0.0/16"},
				ServiceCIDRs: []string{"10.96.0.0/12"},
			},
			localCIDRs: []string{"10.244.0.0/16", "10.0.0.0/12"},
			want: topology.Plan{
				PublicKey:        "key-frankfurt",
				Endpoint:         "203.0.113.10:51820",
				DesiredCIDRs:     []string{"10.50.0.0/16", "10.96.0.0/12"},
				RoutableCIDRs:    []string{"10.50.0.0/16", "10.96.0.0/12"},
				ConflictingCIDRs: nil,
				Phase:            networkingv1alpha1.MeshPeerPhaseConnected,
				Message:          "peer programmed; 2/2 CIDRs routed",
			},
		},
		{
			name: "full overlap -> Error, nothing routable",
			spec: networkingv1alpha1.MeshPeerSpec{
				PublicKey: "key-dup",
				PodCIDRs:  []string{"10.244.0.0/16"},
			},
			localCIDRs: []string{"10.244.0.0/16"},
			want: topology.Plan{
				PublicKey:        "key-dup",
				DesiredCIDRs:     []string{"10.244.0.0/16"},
				RoutableCIDRs:    nil,
				ConflictingCIDRs: []string{"10.244.0.0/16"},
				Phase:            networkingv1alpha1.MeshPeerPhaseError,
				Message:          "CIDR overlap with local cluster requires NAT remap; unrouted: [10.244.0.0/16]",
			},
		},
		{
			name: "partial overlap -> Error, only non-conflicting routed",
			spec: networkingv1alpha1.MeshPeerSpec{
				PublicKey:    "key-partial",
				PodCIDRs:     []string{"10.244.0.0/16", "10.60.0.0/16"},
				ServiceCIDRs: []string{"172.20.0.0/16"},
			},
			localCIDRs: []string{"10.244.0.0/16"},
			want: topology.Plan{
				PublicKey:        "key-partial",
				DesiredCIDRs:     []string{"10.244.0.0/16", "10.60.0.0/16", "172.20.0.0/16"},
				RoutableCIDRs:    []string{"10.60.0.0/16", "172.20.0.0/16"},
				ConflictingCIDRs: []string{"10.244.0.0/16"},
				Phase:            networkingv1alpha1.MeshPeerPhaseError,
				Message:          "CIDR overlap with local cluster requires NAT remap; unrouted: [10.244.0.0/16]",
			},
		},
		{
			name: "supernet/subnet overlap is detected in both directions",
			spec: networkingv1alpha1.MeshPeerSpec{
				PublicKey: "key-super",
				// remote /24 sits inside local /16, and a remote /8 contains local /16.
				PodCIDRs: []string{"10.244.5.0/24", "10.0.0.0/8"},
			},
			localCIDRs: []string{"10.244.0.0/16"},
			want: topology.Plan{
				PublicKey:        "key-super",
				DesiredCIDRs:     []string{"10.244.5.0/24", "10.0.0.0/8"},
				RoutableCIDRs:    nil,
				ConflictingCIDRs: []string{"10.244.5.0/24", "10.0.0.0/8"},
				Phase:            networkingv1alpha1.MeshPeerPhaseError,
				Message:          "CIDR overlap with local cluster requires NAT remap; unrouted: [10.244.5.0/24 10.0.0.0/8]",
			},
		},
		{
			name: "malformed CIDR is surfaced as a conflict",
			spec: networkingv1alpha1.MeshPeerSpec{
				PublicKey: "key-bad",
				PodCIDRs:  []string{"not-a-cidr", "10.70.0.0/16"},
			},
			localCIDRs: nil,
			want: topology.Plan{
				PublicKey:        "key-bad",
				DesiredCIDRs:     []string{"not-a-cidr", "10.70.0.0/16"},
				RoutableCIDRs:    []string{"10.70.0.0/16"},
				ConflictingCIDRs: []string{"not-a-cidr"},
				Phase:            networkingv1alpha1.MeshPeerPhaseError,
				Message:          "CIDR overlap with local cluster requires NAT remap; unrouted: [not-a-cidr]",
			},
		},
		{
			name: "no public key -> non-programmable Error",
			spec: networkingv1alpha1.MeshPeerSpec{
				Endpoint: "203.0.113.10:51820",
				PodCIDRs: []string{"10.80.0.0/16"},
			},
			localCIDRs: nil,
			want: topology.Plan{
				PublicKey:    "",
				Endpoint:     "203.0.113.10:51820",
				DesiredCIDRs: []string{"10.80.0.0/16"},
				Phase:        networkingv1alpha1.MeshPeerPhaseError,
				Message:      "spec.publicKey is required",
			},
		},
		{
			name: "no CIDRs -> Connected with 0/0 routed",
			spec: networkingv1alpha1.MeshPeerSpec{
				PublicKey: "key-empty",
			},
			localCIDRs: []string{"10.244.0.0/16"},
			want: topology.Plan{
				PublicKey:    "key-empty",
				DesiredCIDRs: []string{},
				Phase:        networkingv1alpha1.MeshPeerPhaseConnected,
				Message:      "peer programmed; 0/0 CIDRs routed",
			},
		},
		{
			name: "malformed local CIDR is ignored, remote still routes",
			spec: networkingv1alpha1.MeshPeerSpec{
				PublicKey: "key-localbad",
				PodCIDRs:  []string{"10.90.0.0/16"},
			},
			localCIDRs: []string{"garbage", "10.244.0.0/16"},
			want: topology.Plan{
				PublicKey:     "key-localbad",
				DesiredCIDRs:  []string{"10.90.0.0/16"},
				RoutableCIDRs: []string{"10.90.0.0/16"},
				Phase:         networkingv1alpha1.MeshPeerPhaseConnected,
				Message:       "peer programmed; 1/1 CIDRs routed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := topology.PlanPeer(tt.spec, tt.localCIDRs)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PlanPeer() mismatch\n got: %#v\nwant: %#v", got, tt.want)
			}
		})
	}
}

func TestPlanProgrammableAndConflicts(t *testing.T) {
	programmable := topology.PlanPeer(networkingv1alpha1.MeshPeerSpec{
		PublicKey: "k", PodCIDRs: []string{"10.5.0.0/16"},
	}, nil)
	if !programmable.Programmable() {
		t.Error("expected plan with public key to be programmable")
	}
	if programmable.HasConflicts() {
		t.Error("did not expect conflicts")
	}

	invalid := topology.PlanPeer(networkingv1alpha1.MeshPeerSpec{}, nil)
	if invalid.Programmable() {
		t.Error("expected plan without public key to be non-programmable")
	}
}

func TestPartition(t *testing.T) {
	tests := []struct {
		name          string
		desired       []string
		local         []string
		wantRoutable  []string
		wantConflicts []string
	}{
		{"empty desired", nil, []string{"10.0.0.0/8"}, nil, nil},
		{"all routable", []string{"192.168.1.0/24"}, []string{"10.0.0.0/8"}, []string{"192.168.1.0/24"}, nil},
		{"all conflict", []string{"10.1.0.0/16"}, []string{"10.0.0.0/8"}, nil, []string{"10.1.0.0/16"}},
		{
			name:          "order preserved",
			desired:       []string{"10.1.0.0/16", "192.168.0.0/16", "10.2.0.0/16"},
			local:         []string{"10.0.0.0/8"},
			wantRoutable:  []string{"192.168.0.0/16"},
			wantConflicts: []string{"10.1.0.0/16", "10.2.0.0/16"},
		},
		{
			// SECURITY: a default route must be withheld even with no local CIDRs
			// configured (the egress-hijack guard).
			name:          "default route v4 withheld without locals",
			desired:       []string{"0.0.0.0/0", "10.5.0.0/16"},
			local:         nil,
			wantRoutable:  []string{"10.5.0.0/16"},
			wantConflicts: []string{"0.0.0.0/0"},
		},
		{
			name:          "default route v6 withheld",
			desired:       []string{"::/0"},
			local:         nil,
			wantRoutable:  nil,
			wantConflicts: []string{"::/0"},
		},
		{
			name:          "loopback/link-local/multicast withheld",
			desired:       []string{"127.0.0.1/32", "169.254.0.0/16", "224.0.0.0/4", "10.6.0.0/16"},
			local:         nil,
			wantRoutable:  []string{"10.6.0.0/16"},
			wantConflicts: []string{"127.0.0.1/32", "169.254.0.0/16", "224.0.0.0/4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routable, conflicts := topology.Partition(tt.desired, tt.local)
			if !reflect.DeepEqual(routable, tt.wantRoutable) {
				t.Errorf("routable = %v, want %v", routable, tt.wantRoutable)
			}
			if !reflect.DeepEqual(conflicts, tt.wantConflicts) {
				t.Errorf("conflicts = %v, want %v", conflicts, tt.wantConflicts)
			}
		})
	}
}

func TestIsDangerousCIDR(t *testing.T) {
	dangerous := []string{"0.0.0.0/0", "::/0", "127.0.0.0/8", "127.0.0.1/32", "::1/128", "169.254.0.0/16", "fe80::/10", "224.0.0.0/4", "239.1.2.3/32", "ff00::/8"}
	for _, c := range dangerous {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("parse %s: %v", c, err)
		}
		if !topology.IsDangerousCIDR(n) {
			t.Errorf("IsDangerousCIDR(%s) = false, want true", c)
		}
	}
	safe := []string{"10.244.0.0/16", "192.168.0.0/16", "10.96.0.0/16", "172.16.0.0/12", "fd00::/48"}
	for _, c := range safe {
		_, n, _ := net.ParseCIDR(c)
		if topology.IsDangerousCIDR(n) {
			t.Errorf("IsDangerousCIDR(%s) = true, want false", c)
		}
	}
	if topology.IsDangerousCIDR(nil) {
		t.Error("IsDangerousCIDR(nil) should be false")
	}
}

func TestDangerousCIDRs(t *testing.T) {
	got := topology.DangerousCIDRs([]string{"0.0.0.0/0", "10.5.0.0/16", "garbage", "224.0.0.0/4"})
	want := []string{"0.0.0.0/0", "224.0.0.0/4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DangerousCIDRs = %v, want %v", got, want)
	}
}

// A peer advertising a default route must be Error and route nothing dangerous,
// even when no local CIDRs are configured.
func TestPlanPeer_RefusesDefaultRoute(t *testing.T) {
	plan := topology.PlanPeer(networkingv1alpha1.MeshPeerSpec{
		ClusterID: "evil", PublicKey: "k",
		PodCIDRs: []string{"0.0.0.0/0", "10.7.0.0/16"},
	}, nil)
	if plan.Phase != networkingv1alpha1.MeshPeerPhaseError {
		t.Errorf("phase = %q, want Error", plan.Phase)
	}
	for _, c := range plan.RoutableCIDRs {
		if c == "0.0.0.0/0" {
			t.Fatal("default route must never be routable")
		}
	}
	if !strings.Contains(plan.Message, "dangerous") {
		t.Errorf("message should flag the dangerous range: %q", plan.Message)
	}
}

func TestOverlapsAny(t *testing.T) {
	locals := topology.ParseCIDRList([]string{"10.244.0.0/16", "172.16.0.0/12"})
	tests := []struct {
		cidr string
		want bool
	}{
		{"10.244.5.0/24", true}, // subnet of a local
		{"10.0.0.0/8", true},    // supernet of a local
		{"172.20.0.0/16", true}, // inside 172.16/12
		{"192.168.0.0/16", false},
		{"10.245.0.0/16", false},
	}
	for _, tt := range tests {
		t.Run(tt.cidr, func(t *testing.T) {
			candidate := topology.ParseCIDRList([]string{tt.cidr})
			if len(candidate) != 1 {
				t.Fatalf("failed to parse %q", tt.cidr)
			}
			if got := topology.OverlapsAny(candidate[0], locals); got != tt.want {
				t.Errorf("OverlapsAny(%q) = %v, want %v", tt.cidr, got, tt.want)
			}
		})
	}
}

func TestParseCIDRList_SkipsMalformed(t *testing.T) {
	got := topology.ParseCIDRList([]string{"10.0.0.0/8", "bogus", "", "192.168.0.0/16"})
	if len(got) != 2 {
		t.Fatalf("expected 2 valid CIDRs, got %d (%v)", len(got), got)
	}
}

func TestSanitizeName(t *testing.T) {
	// Already DNS-1123-safe (lowercase alphanumeric + '-') IDs are returned
	// unchanged — no hash suffix.
	t.Run("already safe unchanged", func(t *testing.T) {
		for _, in := range []string{"cluster-frankfurt", "123-cluster", "prod-us-east-1"} {
			if got := topology.SanitizeName(in); got != in {
				t.Errorf("SanitizeName(%q) = %q, want unchanged", in, got)
			}
		}
	})

	// Inputs requiring sanitization get a deterministic <base>-<8 hex> suffix so
	// the lossy mapping stays injective.
	t.Run("sanitized gets hash suffix", func(t *testing.T) {
		cases := []struct{ in, base string }{
			{"Cluster_Frankfurt", "cluster-frankfurt"},
			{"prod.us-east-1", "prod-us-east-1"},
			{"  spaces  ", "spaces"},
			{"UPPER", "upper"},
			{"a/b\\c", "a-b-c"},
			{"--leading-trailing--", "leading-trailing"},
		}
		for _, c := range cases {
			t.Run(c.in, func(t *testing.T) {
				got := topology.SanitizeName(c.in)
				prefix := c.base + "-"
				if !strings.HasPrefix(got, prefix) || len(got) != len(prefix)+8 {
					t.Errorf("SanitizeName(%q) = %q, want %q + 8-hex suffix", c.in, got, prefix)
				}
				if again := topology.SanitizeName(c.in); again != got {
					t.Errorf("SanitizeName(%q) not deterministic: %q vs %q", c.in, got, again)
				}
			})
		}
	})

	// Inputs with nothing usable fall back to a fixed "peer".
	t.Run("empty fallback", func(t *testing.T) {
		for _, in := range []string{"", "!!!"} {
			if got := topology.SanitizeName(in); got != "peer" {
				t.Errorf("SanitizeName(%q) = %q, want %q", in, got, "peer")
			}
		}
	})

	// Distinct IDs that collapse to the same base must NOT clobber each other.
	t.Run("no collision across distinct ids", func(t *testing.T) {
		a := topology.SanitizeName("cluster_a")
		b := topology.SanitizeName("cluster-a") // already safe -> "cluster-a"
		c := topology.SanitizeName("Cluster.A")
		if a == b || a == c || b == c {
			t.Errorf("collision: cluster_a=%q cluster-a=%q Cluster.A=%q", a, b, c)
		}
	})
}

package wg

import (
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
)

// These are white-box tests for the pure helpers in the wg package. The
// kernel-touching methods (SyncInterface/ConfigurePeer/RemovePeer) require root
// and a real netlink socket and are exercised by the integration suite instead.

func TestParseCIDRs(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		nets, err := parseCIDRs([]string{"10.0.0.0/8", "192.168.1.0/24"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(nets) != 2 {
			t.Fatalf("expected 2 nets, got %d", len(nets))
		}
		if nets[0].String() != "10.0.0.0/8" {
			t.Errorf("nets[0] = %s, want 10.0.0.0/8", nets[0].String())
		}
	})

	t.Run("empty yields non-nil empty", func(t *testing.T) {
		nets, err := parseCIDRs(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if nets == nil {
			t.Error("expected non-nil slice")
		}
		if len(nets) != 0 {
			t.Errorf("expected empty, got %v", nets)
		}
	})

	t.Run("malformed errors", func(t *testing.T) {
		if _, err := parseCIDRs([]string{"10.0.0.0/8", "garbage"}); err == nil {
			t.Fatal("expected error for malformed CIDR")
		}
	})
}

func TestStaleRoutes(t *testing.T) {
	tests := []struct {
		name    string
		old     []string
		current []string
		want    []string
	}{
		{"no prior routes", nil, []string{"10.0.0.0/8"}, nil},
		{"unchanged", []string{"10.0.0.0/8"}, []string{"10.0.0.0/8"}, nil},
		{
			name:    "cidr removed leaks without withdrawal",
			old:     []string{"10.0.0.0/8", "192.168.0.0/16"},
			current: []string{"10.0.0.0/8"},
			want:    []string{"192.168.0.0/16"},
		},
		{
			name:    "cidr changed",
			old:     []string{"10.244.0.0/16"},
			current: []string{"10.245.0.0/16"},
			want:    []string{"10.244.0.0/16"},
		},
		{
			name:    "all removed",
			old:     []string{"10.0.0.0/8", "192.168.0.0/16"},
			current: nil,
			want:    []string{"10.0.0.0/8", "192.168.0.0/16"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := staleRoutes(tt.old, tt.current)
			if len(got) != len(tt.want) {
				t.Fatalf("staleRoutes(%v, %v) = %v, want %v", tt.old, tt.current, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("staleRoutes[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestWithAddress(t *testing.T) {
	m := &WireGuardManager{}

	// Empty and blank entries are ignored so an unset config value is a no-op.
	WithAddress("")(m)
	WithAddress("   ")(m)
	if len(m.addrs) != 0 {
		t.Fatalf("blank addresses should be ignored, got %v", m.addrs)
	}

	// Real entries are trimmed and appended; multiple calls accumulate.
	WithAddress(" 10.244.255.254/32 ")(m)
	WithAddress("fd00::1/128", "")(m)
	want := []string{"10.244.255.254/32", "fd00::1/128"}
	if len(m.addrs) != len(want) {
		t.Fatalf("addrs = %v, want %v", m.addrs, want)
	}
	for i := range want {
		if m.addrs[i] != want[i] {
			t.Errorf("addrs[%d] = %q, want %q", i, m.addrs[i], want[i])
		}
	}
}

func TestParseAddr(t *testing.T) {
	t.Run("valid /32", func(t *testing.T) {
		addr, err := parseAddr("10.244.255.254/32")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := addr.IPNet.String(); got != "10.244.255.254/32" {
			t.Errorf("parsed = %s, want 10.244.255.254/32", got)
		}
	})

	t.Run("bare IP without mask errors", func(t *testing.T) {
		if _, err := parseAddr("10.244.255.254"); err == nil {
			t.Fatal("expected error for a non-CIDR address")
		}
	})

	t.Run("garbage errors", func(t *testing.T) {
		if _, err := parseAddr("not-an-address"); err == nil {
			t.Fatal("expected error for malformed address")
		}
	})
}

func TestShortKey(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"short", "short"},
		{"12345678", "12345678"},
		{"123456789", "12345678…"},
		{"AbCdEfGhIjKl", "AbCdEfGh…"},
	}
	for _, tt := range tests {
		if got := shortKey(tt.in); got != tt.want {
			t.Errorf("shortKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsLinkNotFound(t *testing.T) {
	if isLinkNotFound(nil) {
		t.Error("nil should not be link-not-found")
	}
	if isLinkNotFound(errors.New("some other error")) {
		t.Error("generic error should not be link-not-found")
	}
	if !isLinkNotFound(netlink.LinkNotFoundError{}) {
		t.Error("LinkNotFoundError should be detected")
	}
}

func TestIsNoSuchRoute(t *testing.T) {
	if isNoSuchRoute(nil) {
		t.Error("nil is not 'no such process'")
	}
	if !isNoSuchRoute(errors.New("no such process")) {
		t.Error("expected 'no such process' to match")
	}
	if isNoSuchRoute(errors.New("permission denied")) {
		t.Error("unrelated error should not match")
	}
}

// TestNewWireGuardManager_DefaultsInterface verifies name defaulting without
// requiring a working wgctrl socket: we only assert the error path is sane when
// the environment lacks WireGuard. On hosts where wgctrl opens successfully the
// manager is closed immediately.
func TestNewWireGuardManager_DefaultsInterface(t *testing.T) {
	m, err := NewWireGuardManager("", logr.Discard())
	if err != nil {
		// Acceptable in CI without the wireguard kernel module / netlink perms.
		t.Skipf("wgctrl unavailable in this environment: %v", err)
	}
	defer func() { _ = m.Close() }()
	if m.ifaceName != DefaultInterfaceName {
		t.Errorf("ifaceName = %q, want %q", m.ifaceName, DefaultInterfaceName)
	}
	if m.listenPort != DefaultListenPort {
		t.Errorf("listenPort = %d, want %d", m.listenPort, DefaultListenPort)
	}
}

func TestOptions(t *testing.T) {
	m := &WireGuardManager{listenPort: DefaultListenPort, keepalive: persistentKeepalive}

	// Zero/negative values are ignored (keep defaults).
	WithListenPort(0)(m)
	WithKeepalive(0)(m)
	if m.listenPort != DefaultListenPort || m.keepalive != persistentKeepalive {
		t.Fatalf("zero options should not change defaults: port=%d keepalive=%s", m.listenPort, m.keepalive)
	}

	WithListenPort(53820)(m)
	WithKeepalive(10 * time.Second)(m)
	if m.listenPort != 53820 {
		t.Errorf("listenPort = %d, want 53820", m.listenPort)
	}
	if m.keepalive != 10*time.Second {
		t.Errorf("keepalive = %s, want 10s", m.keepalive)
	}
}

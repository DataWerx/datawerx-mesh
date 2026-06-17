package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

// enableIPForward must write "1" to both the IPv4 and IPv6 forwarding sysctls
// when both exist, simulating a dual-stack host.
func TestEnableIPForward_WritesBothSysctls(t *testing.T) {
	root := t.TempDir()
	v4 := filepath.Join(root, ipv4ForwardSysctl)
	v6 := filepath.Join(root, ipv6ForwardSysctl)
	seedSysctl(t, v4, "0")
	seedSysctl(t, v6, "0")

	if err := enableIPForward(root); err != nil {
		t.Fatalf("enableIPForward: %v", err)
	}
	assertContent(t, v4, "1")
	assertContent(t, v6, "1")
}

// A host with IPv6 disabled legitimately lacks the v6 sysctl; that must be
// tolerated as long as IPv4 forwarding is enabled.
func TestEnableIPForward_ToleratesMissingIPv6(t *testing.T) {
	root := t.TempDir()
	v4 := filepath.Join(root, ipv4ForwardSysctl)
	seedSysctl(t, v4, "0")
	// Deliberately do not create the IPv6 sysctl.

	if err := enableIPForward(root); err != nil {
		t.Fatalf("enableIPForward should tolerate a missing IPv6 sysctl: %v", err)
	}
	assertContent(t, v4, "1")
}

// A missing IPv4 sysctl is fatal: a gateway that cannot forward IPv4 is
// non-functional, so the error must surface.
func TestEnableIPForward_MissingIPv4IsError(t *testing.T) {
	root := t.TempDir() // no sysctls seeded
	if err := enableIPForward(root); err == nil {
		t.Fatal("expected an error when the IPv4 forwarding sysctl is absent")
	}
}

func seedSysctl(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Errorf("%s = %q, want %q", path, string(b), want)
	}
}

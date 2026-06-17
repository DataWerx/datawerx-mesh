package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// IP-forwarding sysctls, relative to the sysctl root (/proc/sys). A gateway
// forwards packets between the overlay and the mesh data plane, so the kernel
// must be told to route rather than drop them.
const (
	ipv4ForwardSysctl = "net/ipv4/ip_forward"
	ipv6ForwardSysctl = "net/ipv6/conf/all/forwarding"
)

// EnableIPForward turns on IPv4 and best-effort IPv6 packet forwarding via the
// host's /proc/sys, so the gateway can route client traffic into the mesh. It is
// idempotent. Setting the sysctl from the agent requires the gateway pod to run
// with the relevant securityContext - privileged or an unsafe-sysctl allowance.
// When that is provided out-of-band the write here is a no-op.
func EnableIPForward() error {
	return enableIPForward("/proc/sys")
}

// enableIPForward is the testable core: it writes "1" to the forwarding sysctls
// under root. The IPv4 sysctl is required because a gateway that cannot forward
// IPv4 is non-functional. The IPv6 sysctl is best-effort, since IPv6-disabled
// hosts legitimately lack the file.
func enableIPForward(root string) error {
	if err := writeSysctl(filepath.Join(root, ipv4ForwardSysctl), "1"); err != nil {
		return fmt.Errorf("gateway: enabling IPv4 forwarding: %w", err)
	}
	if err := writeSysctl(filepath.Join(root, ipv6ForwardSysctl), "1"); err != nil {
		// A missing v6 sysctl means IPv6 is disabled on the host; that is fine.
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("gateway: enabling IPv6 forwarding: %w", err)
		}
	}
	return nil
}

// writeSysctl writes value to a sysctl file. The file already exists for any
// real sysctl, so we open it for writing rather than creating it, matching how
// the kernel exposes /proc/sys.
func writeSysctl(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o644)
}

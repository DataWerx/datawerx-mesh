//go:build e2e

// End-to-end tests for the edge device connector.
//
// TestEdgeDeviceContract exercises the open-core contract surface end-to-end
// against a real API server: `dwxctl edge enroll` authors the EdgeDevice CRD,
// `dwxctl edge list` reports it, and `dwxctl edge profile` renders a valid
// device-side wg-quick config. It needs only the EdgeDevice CRD installed and a
// reachable cluster — no premium terminator.
//
// TestEdgeDeviceReachability is the full path: an out-of-cluster WireGuard client
// dials the managed edge terminator and reaches a *.clusterset.local service
// across the mesh. It requires the premium terminator + gateway deployed and the
// harness to export the terminator endpoint/pubkey and a target service URL, so
// it skips unless those env vars are set. The kind harness runs it once with the
// native data plane and once with routed, to prove data-plane independence.
package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
)

const edgeDeviceName = "e2e-edge-device"

func TestEdgeDeviceContract(t *testing.T) {
	ctx := context.Background()
	a, _, err := clusters()
	if err != nil {
		t.Fatalf("connecting to clusters: %v", err)
	}
	dwxctl := buildDwxctl(t)

	devKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	termKey, _ := wgtypes.GeneratePrivateKey()
	devPub := devKey.PublicKey().String()

	t.Cleanup(func() {
		_ = a.c.Delete(context.Background(), &networkingv1alpha1.EdgeDevice{
			ObjectMeta: metav1.ObjectMeta{Name: edgeDeviceName},
		})
	})

	// Enroll: authors the EdgeDevice CRD.
	run(t, exec.Command(dwxctl, "edge", "enroll",
		"--context", a.ctxName,
		"--device-id", edgeDeviceName,
		"--public-key", devPub,
		"--allowed-services", "payments.prod,telemetry.*"))

	// The object exists, is tagged, and carries the spec.
	if err := eventually(ctx, time.Minute, func(ctx context.Context) (bool, error) {
		var dev networkingv1alpha1.EdgeDevice
		if err := a.c.Get(ctx, types.NamespacedName{Name: edgeDeviceName}, &dev); err != nil {
			return false, nil
		}
		return dev.Spec.PublicKey == devPub && len(dev.Spec.AllowedServices) == 2, nil
	}); err != nil {
		t.Fatalf("EdgeDevice was not authored by `dwxctl edge enroll`: %v", err)
	}

	// List reports it.
	if out := run(t, exec.Command(dwxctl, "edge", "list", "--context", a.ctxName)); !strings.Contains(out, edgeDeviceName) {
		t.Errorf("`dwxctl edge list` did not show the device:\n%s", out)
	}

	// Profile renders a usable wg-quick config - address overridden, since no
	// terminator assigns status in this contract-only test.
	cfg := run(t, exec.Command(dwxctl, "edge", "profile",
		"--context", a.ctxName,
		"--device-id", edgeDeviceName,
		"--address", "100.71.0.5",
		"--endpoint", "gw.example.com:51821",
		"--peer-public-key", termKey.PublicKey().String(),
		"--route-cidrs", "241.0.0.0/8",
		"--private-key", devKey.String()))
	for _, want := range []string{
		"[Interface]", "Address = 100.71.0.5/32",
		"[Peer]", "Endpoint = gw.example.com:51821",
		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("rendered wg-quick config missing %q:\n%s", want, cfg)
		}
	}
}

func TestEdgeDeviceReachability(t *testing.T) {
	endpoint := os.Getenv("E2E_EDGE_ENDPOINT")
	peerPub := os.Getenv("E2E_EDGE_PEER_PUBKEY")
	serviceURL := os.Getenv("E2E_EDGE_SERVICE_URL")
	address := os.Getenv("E2E_EDGE_ADDRESS")
	dnsAddr := os.Getenv("E2E_EDGE_DNS")
	if endpoint == "" || peerPub == "" || serviceURL == "" || address == "" {
		t.Skip("set E2E_EDGE_ENDPOINT, E2E_EDGE_PEER_PUBKEY, E2E_EDGE_ADDRESS and E2E_EDGE_SERVICE_URL to run the out-of-cluster reachability test")
	}
	if os.Geteuid() != 0 {
		t.Skip("out-of-cluster wg-quick client requires root")
	}

	ctx := context.Background()
	a, _, err := clusters()
	if err != nil {
		t.Fatalf("connecting to clusters: %v", err)
	}
	dwxctl := buildDwxctl(t)

	devKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	devPub := devKey.PublicKey().String()

	t.Cleanup(func() {
		_ = a.c.Delete(context.Background(), &networkingv1alpha1.EdgeDevice{
			ObjectMeta: metav1.ObjectMeta{Name: edgeDeviceName},
		})
	})
	run(t, exec.Command(dwxctl, "edge", "enroll",
		"--context", a.ctxName, "--device-id", edgeDeviceName, "--public-key", devPub))

	// Render the device's wg-quick config against the real terminator. The route
	// set includes the clusterset VIP range and the device's address; DNS points
	// at the clusterset responder for name resolution.
	args := []string{"edge", "profile",
		"--context", a.ctxName, "--device-id", edgeDeviceName,
		"--address", address, "--endpoint", endpoint, "--peer-public-key", peerPub,
		"--route-cidrs", os.Getenv("E2E_EDGE_ROUTE_CIDRS"),
		"--private-key", devKey.String()}
	if dnsAddr != "" {
		args = append(args, "--dns", dnsAddr)
	}
	cfg := run(t, exec.Command(dwxctl, args...))

	confPath := filepath.Join(t.TempDir(), "dwx-e2e-edge.conf")
	if err := os.WriteFile(confPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Bring the tunnel up, then reach the clusterset service by name.
	run(t, exec.Command("wg-quick", "up", confPath))
	t.Cleanup(func() { _ = exec.Command("wg-quick", "down", confPath).Run() })

	if err := eventually(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		out, err := exec.CommandContext(ctx, "curl", "-fsS", "--max-time", "5", serviceURL).CombinedOutput()
		if err != nil {
			t.Logf("curl %s not ready: %v (%s)", serviceURL, err, strings.TrimSpace(string(out)))
			return false, nil
		}
		return true, nil
	}); err != nil {
		t.Fatalf("out-of-cluster client could not reach %s over the edge tunnel: %v", serviceURL, err)
	}
}

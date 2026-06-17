//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
)

// TestJoinFormsMesh proves the zero-friction join (design 0006) end to end: it
// wipes the MeshPeers on both clusters, then re-forms the mesh using only
// `dwxctl join export | dwxctl join import` — no hand-authored CRDs — and asserts
// every peer reaches Connected, exactly as the hand-wired suite does.
//
// It needs each cluster's WireGuard public key, endpoint, and advertised CIDRs,
// which the harness exports (see hack/e2e/join.sh). When they are absent the
// test skips, so it never fails a run that did not opt into the join path.
func TestJoinFormsMesh(t *testing.T) {
	ctx := context.Background()
	a, b, err := clusters()
	if err != nil {
		t.Fatalf("connecting to clusters: %v", err)
	}

	ja := joinParams(t, "A")
	jb := joinParams(t, "B")
	dwxctl := buildDwxctl(t)

	// Clean slate: remove any existing peers so a green result can only come from
	// the join flow re-authoring them.
	deleteAllPeers(ctx, t, a)
	deleteAllPeers(ctx, t, b)

	// Each cluster imports the other's bundle.
	joinExportImport(t, dwxctl, jb, a.ctxName) // B's bundle -> cluster A
	joinExportImport(t, dwxctl, ja, b.ctxName) // A's bundle -> cluster B

	for _, cl := range []*cluster{a, b} {
		cl := cl
		if err := eventually(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
			return allPeersConnected(ctx, cl)
		}); err != nil {
			dumpMeshDiagnostics(ctx, t, a, b)
			t.Fatalf("%s: MeshPeers authored by `dwxctl join` did not reach Connected: %v", cl.name, err)
		}
	}
}

// joinInputs is one cluster's self-description, the inputs to `join export`.
type joinInputs struct {
	id, pub, endpoint, podCIDR, svcCIDR string
}

// joinParams reads a cluster's join inputs from E2E_*_<suffix>, skipping the
// whole test if the required key or endpoint is absent.
func joinParams(t *testing.T, suffix string) joinInputs {
	t.Helper()
	pub := os.Getenv("E2E_PUB_" + suffix)
	endpoint := os.Getenv("E2E_EP_" + suffix)
	if pub == "" || endpoint == "" {
		t.Skipf("set E2E_PUB_%s and E2E_EP_%s (and optionally E2E_POD_%s/E2E_SVC_%s) to run the join e2e", suffix, suffix, suffix, suffix)
	}
	id := os.Getenv("E2E_ID_" + suffix)
	if id == "" {
		id = "cluster-" + strings.ToLower(suffix)
	}
	return joinInputs{
		id:       id,
		pub:      pub,
		endpoint: endpoint,
		podCIDR:  os.Getenv("E2E_POD_" + suffix),
		svcCIDR:  os.Getenv("E2E_SVC_" + suffix),
	}
}

// buildDwxctl compiles the CLI once and returns its path.
func buildDwxctl(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dwxctl")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/dwxctl")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building dwxctl: %v\n%s", err, out)
	}
	return bin
}

// joinExportImport mints exporter's bundle and applies it as a MeshPeer in the
// importer cluster's context, the two halves of a zero-friction peering.
func joinExportImport(t *testing.T, dwxctl string, in joinInputs, importerCtx string) {
	t.Helper()
	args := []string{"join", "export", "--cluster-id", in.id, "--public-key", in.pub, "--endpoint", in.endpoint}
	if in.podCIDR != "" {
		args = append(args, "--pod-cidrs", in.podCIDR)
	}
	if in.svcCIDR != "" {
		args = append(args, "--service-cidrs", in.svcCIDR)
	}
	token := strings.TrimSpace(run(t, exec.Command(dwxctl, args...)))
	if !strings.HasPrefix(token, "dwxmesh.v1.") {
		t.Fatalf("join export did not mint a bundle token: %q", token)
	}
	run(t, exec.Command(dwxctl, "join", "import", "--context", importerCtx, "--bundle", token))
}

// run executes a command, failing the test with its output on error.
func run(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("%s: %v\n%s", strings.Join(cmd.Args, " "), err, stderr)
	}
	return string(out)
}

// deleteAllPeers removes every MeshPeer on a cluster so the join flow forms the
// mesh from nothing.
func deleteAllPeers(ctx context.Context, t *testing.T, cl *cluster) {
	t.Helper()
	var peers networkingv1alpha1.MeshPeerList
	if err := cl.c.List(ctx, &peers); err != nil {
		t.Fatalf("%s: listing peers: %v", cl.name, err)
	}
	for i := range peers.Items {
		if err := cl.deleteIfExists(ctx, &peers.Items[i]); err != nil {
			t.Fatalf("%s: deleting peer %s: %v", cl.name, peers.Items[i].Name, err)
		}
	}
}

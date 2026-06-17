//go:build integration

// Package integration exercises the controllers against a real Kubernetes API
// server provided by envtest — validating things the in-memory fake client
// cannot: CRD schema enforcement of required fields, enums, etc., the status
// subresource, finalizers, and real create/update/get semantics.
//
// It is gated behind the `integration` build tag so the default `go test ./...`
// stays hermetic. envtest needs the kube-apiserver + etcd test binaries; provide
// them via `setup-envtest` and KUBEBUILDER_ASSETS (see
// .github/workflows/integration.yml). When the binaries are absent the suite
// skips rather than failing, so a developer without them is not blocked.
//
//	KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.30.0) \
//	  go test -tags integration ./test/integration/...
package integration

import (
	"path/filepath"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/nat"
)

// integrationScheme is the scheme used by integration clients.
func integrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("core scheme: %v", err)
	}
	if err := networkingv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("networking scheme: %v", err)
	}
	if err := mcsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("mcs scheme: %v", err)
	}
	return s
}

// startEnv boots an envtest API server with the project CRDs installed and
// returns a client plus a stop function. It skips the test if the test binaries
// are unavailable.
func startEnv(t *testing.T) (client.Client, *runtime.Scheme, func()) {
	t.Helper()
	scheme := integrationScheme(t)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Skipf("envtest unavailable (install test binaries via setup-envtest / set KUBEBUILDER_ASSETS): %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = env.Stop()
		t.Fatalf("building client: %v", err)
	}
	return c, scheme, func() { _ = env.Stop() }
}

// --- fakes (the controllers' data-plane seams; integration validates the
// control-plane/API behavior, not the kernel side effects) ---

type fakePeerDataPlane struct {
	mu         sync.Mutex
	configured map[string]bool
	removed    map[string]bool
}

func newFakePeerDataPlane() *fakePeerDataPlane {
	return &fakePeerDataPlane{configured: map[string]bool{}, removed: map[string]bool{}}
}

func (f *fakePeerDataPlane) ConfigurePeer(key, _ string, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configured[key] = true
	return nil
}

func (f *fakePeerDataPlane) RemovePeer(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed[key] = true
	return nil
}

func (f *fakePeerDataPlane) PeerHandshake(string) (int64, error) { return 0, nil }

func (f *fakePeerDataPlane) wasConfigured(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configured[key]
}

type fakeNATDataPlane struct {
	mu   sync.Mutex
	last []nat.ServiceDNAT
}

func (f *fakeNATDataPlane) SyncClusterSetNAT(s []nat.ServiceDNAT) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = append([]nat.ServiceDNAT(nil), s...)
	return nil
}

func (f *fakeNATDataPlane) synced() []nat.ServiceDNAT {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

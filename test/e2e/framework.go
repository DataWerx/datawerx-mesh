//go:build e2e

// Package e2e contains the multi-cluster end-to-end suite for DataWerx Mesh.
//
// It is gated behind the `e2e` build tag so the default `go test ./...` stays
// hermetic and root-free; run it explicitly against two live clusters:
//
//	go test -tags e2e -timeout 30m ./test/e2e/...
//
// The suite assumes two reachable clusters already running the agent with a
// reciprocal MeshPeer pair wired up (see hack/e2e/kind-up.sh, which builds the
// image, loads it into both kind clusters, installs CRDs, deploys the agent,
// configures CoreDNS, and exchanges WireGuard keys/endpoints). Configuration is
// via environment variables:
//
//	E2E_KUBECONFIG_A / E2E_CONTEXT_A   cluster A (default: $KUBECONFIG, context kind-dwx-a)
//	E2E_KUBECONFIG_B / E2E_CONTEXT_B   cluster B (default: $KUBECONFIG, context kind-dwx-b)
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
)

// cluster bundles a named controller-runtime client for one member cluster.
type cluster struct {
	name string
	// ctxName is the resolved kube context (e.g. kind-dwx-a), retained so
	// failure diagnostics can shell out to `kubectl --context` for agent logs.
	ctxName string
	c       client.Client
}

// e2eScheme is the scheme used by every e2e client: core/batch/etc. plus the
// DataWerx and MCS API groups.
func e2eScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := networkingv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := mcsv1alpha1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// newCluster builds a client for the cluster identified by the given env vars,
// falling back to the ambient kubeconfig and the supplied default context.
func newCluster(name, kubeconfigEnv, contextEnv, defaultContext string) (*cluster, error) {
	scheme, err := e2eScheme()
	if err != nil {
		return nil, err
	}

	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	if kc := os.Getenv(kubeconfigEnv); kc != "" {
		loading.ExplicitPath = kc
	}
	overrides := &clientcmd.ConfigOverrides{}
	ctxName := defaultContext
	if cctx := os.Getenv(contextEnv); cctx != "" {
		ctxName = cctx
	}
	if ctxName != "" {
		overrides.CurrentContext = ctxName
	}

	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building rest config for %s: %w", name, err)
	}
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building client for %s: %w", name, err)
	}
	return &cluster{name: name, ctxName: ctxName, c: c}, nil
}

// dumpMeshDiagnostics logs, for each cluster, every MeshPeer's phase/message and
// the agent DaemonSet's recent logs. It is best-effort — every failure is logged
// rather than fatal — and is meant to be deferred from a test's failure path:
// the nightly tears the clusters down on exit, so a peer that never reaches
// Connected would otherwise leave no trace of *why*. The reconciler records the
// wrapped data-plane error in MeshPeer .Status.Message (see r.fail in
// meshpeer_controller.go), so that field is the primary signal here.
func dumpMeshDiagnostics(ctx context.Context, t *testing.T, cls ...*cluster) {
	t.Helper()
	for _, cl := range cls {
		t.Logf("===== diagnostics: cluster %s (context %s) =====", cl.name, cl.ctxName)

		var peers networkingv1alpha1.MeshPeerList
		if err := cl.c.List(ctx, &peers); err != nil {
			t.Logf("%s: listing MeshPeers: %v", cl.name, err)
		} else if len(peers.Items) == 0 {
			t.Logf("%s: no MeshPeers found", cl.name)
		} else {
			for i := range peers.Items {
				p := &peers.Items[i]
				t.Logf("%s: MeshPeer %q phase=%q observedGen=%d lastHandshake=%d message=%q",
					cl.name, p.Name, p.Status.Phase, p.Status.ObservedGeneration,
					p.Status.LastHandshakeTime, p.Status.Message)
			}
		}

		if cl.ctxName == "" {
			continue
		}
		out, err := exec.CommandContext(ctx, "kubectl", "--context", cl.ctxName,
			"-n", "datawerx-system", "logs", "ds/dwx-mesh-agent",
			"--all-containers", "--tail=200").CombinedOutput()
		if err != nil {
			t.Logf("%s: fetching agent logs: %v", cl.name, err)
		}
		if len(out) > 0 {
			t.Logf("%s: agent logs (tail):\n%s", cl.name, out)
		}
	}
}

// nodeFor maps a kube context (e.g. kind-dwx-a) to its kind node container name
// (dwx-a-control-plane), the docker container the data-path diagnostics exec
// into. Returns "" if the context isn't a kind context.
func nodeFor(ctxName string) string {
	name := strings.TrimPrefix(ctxName, "kind-")
	if name == "" || name == ctxName {
		return ""
	}
	return name + "-control-plane"
}

// dumpDataPathDiagnostics captures, on a cross-cluster connectivity failure, the
// evidence needed to tell a broken tunnel from a forwarding/firewall drop: the
// curl Job's pod logs (a DNS failure looks different from a connection timeout),
// and on each node the WireGuard device counters (did encrypted bytes flow?),
// the host routes, ip_forward, the FORWARD chain, and the mesh NAT/firewall
// rules. Best-effort: it shells out to kubectl + docker on the runner and logs,
// never fails. jobCluster is where the curl Job runs; nodes are the clusters
// whose kind nodes to inspect.
func dumpDataPathDiagnostics(ctx context.Context, t *testing.T, jobCluster *cluster, jobName, jobNamespace string, nodes ...*cluster) {
	t.Helper()

	if jobCluster.ctxName != "" {
		out, err := exec.CommandContext(ctx, "kubectl", "--context", jobCluster.ctxName,
			"-n", jobNamespace, "logs", "job/"+jobName, "--tail=50").CombinedOutput()
		t.Logf("curl job %q logs (cluster %s, err=%v):\n%s", jobName, jobCluster.name, err, out)
	}

	cmds := [][]string{
		{"wg", "show", "dwx-mesh0"},
		{"ip", "-s", "link", "show", "dwx-mesh0"},
		{"ip", "route"},
		{"sysctl", "net.ipv4.ip_forward"},
		{"iptables", "-S", "FORWARD"},
		{"iptables", "-S", "DWX-MESH-FW"},
		{"iptables", "-t", "nat", "-S"},
		// conntrack (if present) shows whether the reply was seen / NATed —
		// distinguishes a dropped return path from one that never started.
		{"conntrack", "-L", "-p", "tcp", "--dport", "8080"},
	}
	for _, cl := range nodes {
		node := nodeFor(cl.ctxName)
		if node == "" {
			continue
		}
		t.Logf("----- node %s (cluster %s) data path -----", node, cl.name)
		for _, c := range cmds {
			out, err := exec.CommandContext(ctx, "docker", append([]string{"exec", node}, c...)...).CombinedOutput()
			suffix := ""
			if err != nil {
				suffix = fmt.Sprintf(" (err: %v)", err)
			}
			t.Logf("$ docker exec %s %s%s\n%s", node, strings.Join(c, " "), suffix, out)
		}
	}
}

// clusters connects to both member clusters.
func clusters() (*cluster, *cluster, error) {
	a, err := newCluster("A", "E2E_KUBECONFIG_A", "E2E_CONTEXT_A", "kind-dwx-a")
	if err != nil {
		return nil, nil, err
	}
	b, err := newCluster("B", "E2E_KUBECONFIG_B", "E2E_CONTEXT_B", "kind-dwx-b")
	if err != nil {
		return nil, nil, err
	}
	return a, b, nil
}

// eventually polls fn until it returns true or the timeout elapses.
func eventually(ctx context.Context, timeout time.Duration, fn func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := fn(ctx)
		if err == nil && ok {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("condition not met within %s: %w", timeout, lastErr)
	}
	return fmt.Errorf("condition not met within %s", timeout)
}

// ensureNamespace creates ns in the cluster if it does not already exist.
func (cl *cluster) ensureNamespace(ctx context.Context, ns string) error {
	obj := &corev1.Namespace{}
	obj.Name = ns
	if err := cl.c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("%s: creating namespace %s: %w", cl.name, ns, err)
	}
	return nil
}

// mirrorEndpointExports copies every EndpointExport in namespace ns from src to
// dst, simulating the GitOps pipeline that propagates exports between clusters
// in the free tier. (In the premium tier the SaaS syncer does this.)
func mirrorEndpointExports(ctx context.Context, src, dst *cluster, ns string) (int, error) {
	var list networkingv1alpha1.EndpointExportList
	if err := src.c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return 0, fmt.Errorf("listing EndpointExports on %s: %w", src.name, err)
	}
	for i := range list.Items {
		in := &list.Items[i]
		out := &networkingv1alpha1.EndpointExport{}
		out.Namespace = in.Namespace
		out.Name = in.Name
		_, err := controllerutil.CreateOrUpdate(ctx, dst.c, out, func() error {
			out.Spec = in.Spec
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("mirroring EndpointExport %s/%s to %s: %w", in.Namespace, in.Name, dst.name, err)
		}
	}
	return len(list.Items), nil
}

// deleteIfExists deletes obj, ignoring NotFound.
func (cl *cluster) deleteIfExists(ctx context.Context, obj client.Object) error {
	if err := cl.c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// getServiceImport fetches a ServiceImport, reporting existence.
func (cl *cluster) getServiceImport(ctx context.Context, ns, name string) (*mcsv1alpha1.ServiceImport, bool, error) {
	var si mcsv1alpha1.ServiceImport
	err := cl.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &si)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &si, true, nil
}

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/DataWerx/datawerx-mesh/pkg/topology"
)

const (
	ovlNamespace = "dwx-ovl"
	ovlName      = "echo-ovl"
	// Must match hack/e2e/kind-up.sh OVERLAP mode. RFC-6598 shared space, chosen
	// so the carved virtual /16s never collide with the docker "kind" bridge
	// (allocated from 172.16.0.0/12) that the clusters' underlay rides on.
	ovlRemapPool      = "100.64.0.0/10"
	ovlClusterAID     = "cluster-a"
	ovlPodCIDR        = "10.244.0.0/16" // POD_A == POD_B in OVERLAP mode
	ovlConnectTimeout = 4 * time.Minute
)

// TestOverlapRemapConnectivity is the M3 gate: both clusters share the same
// pod/service CIDRs (set up by `OVERLAP=1 hack/e2e/kind-up.sh`), so cluster B
// reaches cluster A's backend only through the bidirectional NETMAP remap — by
// the pod's *virtual* IP. A success here proves the stateless 1:1 NAT directions
// compose end to end across the overlapping mesh.
//
// It deliberately targets a pod IP, not a Service ClusterIP. Remap is a pure L3
// address translation (stateless NETMAP, no conntrack), so pod-to-pod over the
// virtual range is exactly what the feature provides and is correct on any
// number of nodes. Reaching a Service cross-cluster is a separate concern proven
// by TestCrossClusterClusterSetIP, whose ClusterSetIP data plane DNATs straight
// to pod endpoints. Curling a bare ClusterIP *through* remap would instead lean
// on the destination's kube-proxy to DNAT a non-local source, which kube-proxy
// masquerades to the ingress node — a single-node-only path that breaks the
// return route on a real multi-node cluster.
//
// Gated by E2E_OVERLAP=1 because it requires the overlapping cluster setup
// (distinct-CIDR runs would route directly and not exercise remap).
func TestOverlapRemapConnectivity(t *testing.T) {
	if os.Getenv("E2E_OVERLAP") != "1" {
		t.Skip("set E2E_OVERLAP=1 and bring clusters up with OVERLAP=1 hack/e2e/kind-up.sh")
	}

	ctx := context.Background()
	a, b, err := clusters()
	if err != nil {
		t.Fatalf("connecting to clusters: %v", err)
	}
	if err := a.ensureNamespace(ctx, ovlNamespace); err != nil {
		t.Fatal(err)
	}
	// The curl Job runs in cluster B, so B needs the namespace too — creating it
	// only in A left the Job create failing with "namespaces dwx-ovl not found".
	if err := b.ensureNamespace(ctx, ovlNamespace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_ = a.deleteIfExists(c, ovlDeployment())
		_ = b.deleteIfExists(c, ovlJob(""))
	})

	// Backend pod in cluster A.
	mustCreate(t, ctx, a, ovlDeployment())

	// Wait for A's pod to get a real pod IP, then map it to its virtual IP.
	podIP, err := awaitPodIP(ctx, a)
	if err != nil {
		t.Fatalf("backend pod never got an IP: %v", err)
	}

	virtualIP, err := remapHostIP(ovlRemapPool, ovlClusterAID, ovlPodCIDR, podIP)
	if err != nil {
		t.Fatalf("computing virtual IP: %v", err)
	}
	t.Logf("cluster A pod IP %s -> virtual %s (reachable from B)", podIP, virtualIP)

	// A Job in B that curls the virtual pod IP. Success proves the remap works
	// end-to-end across the overlapping mesh.
	job := ovlJob(virtualIP)
	mustCreate(t, ctx, b, job)
	if err := eventually(ctx, ovlConnectTimeout, func(ctx context.Context) (bool, error) {
		return jobSucceeded(ctx, b, ovlNamespace, job.Name)
	}); err != nil {
		dumpMeshDiagnostics(ctx, t, a, b)
		dumpDataPathDiagnostics(ctx, t, b, job.Name, ovlNamespace, a, b)
		t.Fatalf("overlap remap connectivity failed (B could not reach A's pod via its virtual IP): %v", err)
	}
}

// awaitPodIP waits until cluster A's overlap backend pod is Running with a pod IP
// assigned and returns it.
func awaitPodIP(ctx context.Context, a *cluster) (string, error) {
	var podIP string
	err := eventually(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		var pods corev1.PodList
		if err := a.c.List(ctx, &pods, client.InNamespace(ovlNamespace), client.MatchingLabels(ovlLabels())); err != nil {
			return false, err
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
				podIP = p.Status.PodIP
				return true, nil
			}
		}
		return false, nil
	})
	return podIP, err
}

// remapHostIP maps a real IP within realCIDR to the corresponding address in the
// deterministic virtual range for (clusterID, realCIDR): it swaps the network
// prefix while preserving the host bits, exactly as the 1:1 NETMAP does.
func remapHostIP(pool, clusterID, realCIDR, realIP string) (string, error) {
	virtualCIDR, err := topology.VirtualCIDR(pool, clusterID, realCIDR)
	if err != nil {
		return "", err
	}
	_, vNet, err := net.ParseCIDR(virtualCIDR)
	if err != nil {
		return "", err
	}
	_, rNet, err := net.ParseCIDR(realCIDR)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(realIP).To4()
	vBase := vNet.IP.To4()
	mask := rNet.Mask
	if ip == nil || vBase == nil || len(mask) != 4 {
		return "", fmt.Errorf("IPv4 expected (ip=%q virtual=%q real=%q)", realIP, virtualCIDR, realCIDR)
	}
	out := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		out[i] = (vBase[i] & mask[i]) | (ip[i] &^ mask[i])
	}
	return out.String(), nil
}

// --- fixtures ---

func ovlLabels() map[string]string { return map[string]string{"app": ovlName} }

func ovlDeployment() *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: ovlName, Namespace: ovlNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: ovlLabels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: ovlLabels()},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "echo",
						Image: "hashicorp/http-echo:1.0",
						Args:  []string{"-listen=:8080", "-text=hello-over-remap"},
						Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
					}},
				},
			},
		},
	}
}

func ovlJob(virtualIP string) *batchv1.Job {
	backoff := int32(6)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "dwx-ovl-curl", Namespace: ovlNamespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "curl",
						Image:   "curlimages/curl:8.7.1",
						Command: []string{"sh", "-c", fmt.Sprintf("curl -v -f --max-time 5 http://%s:8080; rc=$?; echo CURL_EXIT=$rc; exit $rc", virtualIP)},
					}},
				},
			},
		},
	}
}

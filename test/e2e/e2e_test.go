//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
)

const (
	e2eNamespace = "dwx-e2e"
	echoName     = "echo"
	echoPort     = 8080
	csName       = "echo-cs" // a normal ClusterIP service, for the ClusterSetIP path
)

// TestMeshPeersConnected asserts every MeshPeer on both clusters reaches the
// Connected phase — i.e. the WireGuard tunnels came up.
func TestMeshPeersConnected(t *testing.T) {
	ctx := context.Background()
	a, b, err := clusters()
	if err != nil {
		t.Fatalf("connecting to clusters: %v", err)
	}

	for _, cl := range []*cluster{a, b} {
		cl := cl
		if err := eventually(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
			return allPeersConnected(ctx, cl)
		}); err != nil {
			// Surface why before teardown destroys the evidence: each MeshPeer's
			// .Status.Message carries the reconciler's data-plane error, and the
			// agent logs show the WireGuard programming failure first-hand.
			dumpMeshDiagnostics(ctx, t, a, b)
			t.Fatalf("%s: MeshPeers did not all reach Connected: %v", cl.name, err)
		}
	}
}

// allPeersConnected reports whether the cluster has at least one MeshPeer and
// every MeshPeer has reached the Connected phase.
func allPeersConnected(ctx context.Context, cl *cluster) (bool, error) {
	var peers networkingv1alpha1.MeshPeerList
	if err := cl.c.List(ctx, &peers); err != nil {
		return false, err
	}
	if len(peers.Items) == 0 {
		return false, nil // peers not wired yet
	}
	for i := range peers.Items {
		if peers.Items[i].Status.Phase != networkingv1alpha1.MeshPeerPhaseConnected {
			return false, nil
		}
	}
	return true, nil
}

// TestCrossClusterHeadlessDNS exports a headless service in cluster A and
// verifies that a pod in cluster B can resolve and reach it by its
// clusterset.local name — the full export → mirror → import → DNS → connect path.
func TestCrossClusterHeadlessDNS(t *testing.T) {
	ctx := context.Background()
	a, b, err := clusters()
	if err != nil {
		t.Fatalf("connecting to clusters: %v", err)
	}

	if err := a.ensureNamespace(ctx, e2eNamespace); err != nil {
		t.Fatal(err)
	}
	if err := b.ensureNamespace(ctx, e2eNamespace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanup(context.Background(), a, b) })

	// 1. Backend + headless Service + ServiceExport in cluster A.
	mustCreate(t, ctx, a, echoDeployment(), echoHeadlessService(), echoServiceExport())

	// 2. The export controller publishes EndpointExport(s) in A once endpoints
	//    are ready; mirror them into B (the GitOps pipeline's job in the free
	//    tier). Retry until at least one export carries endpoint IPs.
	if err := eventually(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		return endpointExportsMirrored(ctx, a, b)
	}); err != nil {
		t.Fatalf("EndpointExports never carried endpoints: %v", err)
	}

	// 3. The import controller in B should converge a headless ServiceImport.
	if err := eventually(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		return headlessImportConverged(ctx, b, echoName)
	}); err != nil {
		t.Fatalf("ServiceImport did not converge in cluster B: %v", err)
	}

	// 4. A Job in B that curls the clusterset.local name proves DNS + L3
	//    connectivity across the mesh in one shot.
	job := curlJob()
	mustCreate(t, ctx, b, job)
	if err := eventually(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		return jobSucceeded(ctx, b, e2eNamespace, job.Name)
	}); err != nil {
		dumpMeshDiagnostics(ctx, t, a, b)
		dumpDataPathDiagnostics(ctx, t, b, job.Name, e2eNamespace, a, b)
		t.Fatalf("cross-cluster connectivity job did not succeed: %v", err)
	}
}

// TestCrossClusterClusterSetIP exports a normal (ClusterIP) Service in A and
// verifies a pod in B can reach it by its clusterset.local name — exercising
// the ClusterSetIP path: DNS returns the virtual IP, and the pkg/nat data plane
// DNATs it to A's real service IP across the mesh.
func TestCrossClusterClusterSetIP(t *testing.T) {
	ctx := context.Background()
	a, b, err := clusters()
	if err != nil {
		t.Fatalf("connecting to clusters: %v", err)
	}
	if err := a.ensureNamespace(ctx, e2eNamespace); err != nil {
		t.Fatal(err)
	}
	if err := b.ensureNamespace(ctx, e2eNamespace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_ = a.deleteIfExists(c, csServiceExport())
		_ = a.deleteIfExists(c, csService())
		_ = a.deleteIfExists(c, csDeployment())
		_ = b.deleteIfExists(c, csCurlJob())
	})

	mustCreate(t, ctx, a, csDeployment(), csService(), csServiceExport())

	// Mirror exports A->B until present.
	if err := eventually(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		n, err := mirrorEndpointExports(ctx, a, b, e2eNamespace)
		return n > 0, err
	}); err != nil {
		t.Fatalf("EndpointExports not mirrored: %v", err)
	}

	// B's import controller should converge a ClusterSetIP ServiceImport with a VIP.
	if err := eventually(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		return clusterSetImportReady(ctx, b, csName)
	}); err != nil {
		t.Fatalf("ClusterSetIP ServiceImport did not converge in B: %v", err)
	}

	// Connectivity by name: DNS -> VIP -> NAT DNAT -> A's service IP across the mesh.
	job := csCurlJob()
	mustCreate(t, ctx, b, job)
	if err := eventually(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		return jobSucceeded(ctx, b, e2eNamespace, job.Name)
	}); err != nil {
		dumpMeshDiagnostics(ctx, t, a, b)
		dumpDataPathDiagnostics(ctx, t, b, job.Name, e2eNamespace, a, b)
		t.Fatalf("ClusterSetIP connectivity job did not succeed: %v", err)
	}
}

// TestServiceImportTornDownOnUnexport verifies that removing the export (and its
// mirrored EndpointExport) removes the ServiceImport in cluster B.
func TestServiceImportTornDownOnUnexport(t *testing.T) {
	ctx := context.Background()
	a, b, err := clusters()
	if err != nil {
		t.Fatalf("connecting to clusters: %v", err)
	}

	// Remove the export in A and the mirrored export in B.
	_ = a.deleteIfExists(ctx, echoServiceExport())
	var mirrored networkingv1alpha1.EndpointExportList
	if err := b.c.List(ctx, &mirrored, client.InNamespace(e2eNamespace)); err == nil {
		for i := range mirrored.Items {
			_ = b.deleteIfExists(ctx, &mirrored.Items[i])
		}
	}

	if err := eventually(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		_, ok, err := b.getServiceImport(ctx, e2eNamespace, echoName)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}); err != nil {
		t.Fatalf("ServiceImport was not torn down after unexport: %v", err)
	}
}

func cleanup(ctx context.Context, a, b *cluster) {
	_ = a.deleteIfExists(ctx, echoServiceExport())
	_ = a.deleteIfExists(ctx, echoHeadlessService())
	_ = a.deleteIfExists(ctx, echoDeployment())
	_ = b.deleteIfExists(ctx, curlJob())
}

// --- shared assertions / actions ---

// mustCreate creates each object, treating AlreadyExists as success and failing
// the test on any other error.
func mustCreate(t *testing.T, ctx context.Context, cl *cluster, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		if err := cl.c.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("creating %T: %v", o, err)
		}
	}
}

// endpointExportsMirrored mirrors A's exports into B and reports whether at least
// one mirrored export carries endpoint IPs.
func endpointExportsMirrored(ctx context.Context, a, b *cluster) (bool, error) {
	n, err := mirrorEndpointExports(ctx, a, b, e2eNamespace)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	var list networkingv1alpha1.EndpointExportList
	if err := b.c.List(ctx, &list, client.InNamespace(e2eNamespace)); err != nil {
		return false, err
	}
	for i := range list.Items {
		if len(list.Items[i].Spec.IPs) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// headlessImportConverged reports whether cl has a headless ServiceImport named name.
func headlessImportConverged(ctx context.Context, cl *cluster, name string) (bool, error) {
	si, ok, err := cl.getServiceImport(ctx, e2eNamespace, name)
	if err != nil || !ok {
		return false, err
	}
	return si.Spec.Type == mcsv1alpha1.Headless, nil
}

// clusterSetImportReady reports whether cl has a ClusterSetIP ServiceImport named
// name that has been assigned at least one virtual IP.
func clusterSetImportReady(ctx context.Context, cl *cluster, name string) (bool, error) {
	si, ok, err := cl.getServiceImport(ctx, e2eNamespace, name)
	if err != nil || !ok {
		return false, err
	}
	return si.Spec.Type == mcsv1alpha1.ClusterSetIP && len(si.Spec.IPs) > 0, nil
}

// jobSucceeded reports whether the named Job in ns has at least one successful pod.
func jobSucceeded(ctx context.Context, cl *cluster, ns, name string) (bool, error) {
	var got batchv1.Job
	if err := cl.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
		return false, err
	}
	return got.Status.Succeeded > 0, nil
}

// --- fixtures ---

func echoLabels() map[string]string { return map[string]string{"app": echoName} }

func echoDeployment() *appsv1.Deployment {
	replicas := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: echoName, Namespace: e2eNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: echoLabels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: echoLabels()},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "echo",
						Image: "hashicorp/http-echo:1.0",
						Args:  []string{"-listen=:8080", "-text=hello-from-cluster-a"},
						Ports: []corev1.ContainerPort{{ContainerPort: echoPort}},
					}},
				},
			},
		},
	}
}

func echoHeadlessService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: echoName, Namespace: e2eNamespace},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone, // headless
			Selector:  echoLabels(),
			Ports:     []corev1.ServicePort{{Port: echoPort, TargetPort: intstr.FromInt(echoPort), Protocol: corev1.ProtocolTCP}},
		},
	}
}

func echoServiceExport() *mcsv1alpha1.ServiceExport {
	return &mcsv1alpha1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{Name: echoName, Namespace: e2eNamespace},
	}
}

func curlJob() *batchv1.Job {
	backoff := int32(6)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "dwx-e2e-curl", Namespace: e2eNamespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "curl",
						Image: "curlimages/curl:8.7.1",
						// Resolve + connect by the clusterset.local name; -f fails
						// the command (and thus the Job pod) on any error. -v makes
						// the pod log show DNS resolution + the connection attempt so
						// a failure can be diagnosed (DNS vs timeout vs refused).
						Command: []string{"sh", "-c",
							"curl -v -f --max-time 5 http://echo.dwx-e2e.svc.clusterset.local:8080; rc=$?; echo CURL_EXIT=$rc; exit $rc"},
					}},
				},
			},
		},
	}
}

// --- ClusterSetIP fixtures (a normal ClusterIP service) ---

func csLabels() map[string]string { return map[string]string{"app": csName} }

func csDeployment() *appsv1.Deployment {
	replicas := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: csName, Namespace: e2eNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: csLabels()},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: csLabels()},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "echo",
						Image: "hashicorp/http-echo:1.0",
						Args:  []string{"-listen=:8080", "-text=hello-clusterset"},
						Ports: []corev1.ContainerPort{{ContainerPort: echoPort}},
					}},
				},
			},
		},
	}
}

func csService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: csName, Namespace: e2eNamespace},
		Spec: corev1.ServiceSpec{
			Selector: csLabels(), // normal ClusterIP service (gets a ClusterIP)
			Ports:    []corev1.ServicePort{{Port: echoPort, TargetPort: intstr.FromInt(echoPort), Protocol: corev1.ProtocolTCP}},
		},
	}
}

func csServiceExport() *mcsv1alpha1.ServiceExport {
	return &mcsv1alpha1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{Name: csName, Namespace: e2eNamespace},
	}
}

func csCurlJob() *batchv1.Job {
	backoff := int32(6)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "dwx-e2e-curl-cs", Namespace: e2eNamespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "curl",
						Image:   "curlimages/curl:8.7.1",
						Command: []string{"sh", "-c", "curl -v -f --max-time 5 http://echo-cs.dwx-e2e.svc.clusterset.local:8080; rc=$?; echo CURL_EXIT=$rc; exit $rc"},
					}},
				},
			},
		},
	}
}

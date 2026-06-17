package conformance

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/dns"
)

// KEP-1645 §"ServiceExport": exporting is a marker on a Service of the same
// name and namespace — the ServiceExport carries no spec, and its acceptance is
// reported via status conditions.
func TestServiceExportIsAMarker(t *testing.T) {
	typ := reflect.TypeOf(mcsv1alpha1.ServiceExport{})
	if _, hasSpec := typ.FieldByName("Spec"); hasSpec {
		t.Error("ServiceExport must be a marker with no Spec (KEP-1645)")
	}
	if _, hasStatus := typ.FieldByName("Status"); !hasStatus {
		t.Error("ServiceExport must report acceptance via Status")
	}
}

// KEP-1645 defines the ServiceExport condition types Valid and Conflict; the
// MCS-aware tooling and our own readers key off these exact strings.
func TestServiceExportConditionTypes(t *testing.T) {
	if mcsv1alpha1.ServiceExportValid != "Valid" {
		t.Errorf("Valid condition type must be %q, got %q", "Valid", mcsv1alpha1.ServiceExportValid)
	}
	if mcsv1alpha1.ServiceExportConflict != "Conflict" {
		t.Errorf("Conflict condition type must be %q, got %q", "Conflict", mcsv1alpha1.ServiceExportConflict)
	}
}

// KEP-1645 §"ServiceImport": Type is exactly ClusterSetIP or Headless.
func TestServiceImportTypeEnum(t *testing.T) {
	if mcsv1alpha1.ClusterSetIP != "ClusterSetIP" {
		t.Errorf("ClusterSetIP type must be %q, got %q", "ClusterSetIP", mcsv1alpha1.ClusterSetIP)
	}
	if mcsv1alpha1.Headless != "Headless" {
		t.Errorf("Headless type must be %q, got %q", "Headless", mcsv1alpha1.Headless)
	}
}

// KEP-1645 ServicePort is the cross-cluster subset of core/v1 ServicePort:
// name, protocol, appProtocol, port.
func TestServicePortShape(t *testing.T) {
	typ := reflect.TypeOf(mcsv1alpha1.ServicePort{})
	for _, field := range []string{"Name", "Protocol", "AppProtocol", "Port"} {
		if _, ok := typ.FieldByName(field); !ok {
			t.Errorf("ServicePort must carry %q (KEP-1645)", field)
		}
	}
}

// KEP-1645 §"DNS": a ClusterSetIP/Headless service is resolvable at
// <service>.<namespace>.svc.clusterset.local. The name must build and parse back
// to the same service identity.
func TestClusterSetDNSName(t *testing.T) {
	if dns.ClusterSetDomain != "clusterset.local" {
		t.Errorf("the clusterset zone must be clusterset.local, got %q", dns.ClusterSetDomain)
	}
	fqdn := dns.FQDN("payments", "prod")
	if want := "payments.prod.svc.clusterset.local."; fqdn != want {
		t.Fatalf("FQDN = %q, want %q", fqdn, want)
	}
	if !dns.InClusterSetZone(fqdn) {
		t.Errorf("%q should be recognized as in the clusterset zone", fqdn)
	}
	name, ns, ok := dns.ParseClusterSetName(fqdn)
	if !ok || name != "payments" || ns != "prod" {
		t.Errorf("round-trip parse of %q = (%q, %q, %v), want (payments, prod, true)", fqdn, name, ns, ok)
	}
}

// DataWerx delta (documented): ClusterSetIPs are allocated deterministically by
// hashing into the managed range, with no central allocator — so every node
// computes the same VIP. The address must be stable across runs and within the
// CIDR.
func TestClusterSetIPAllocationIsDeterministicAndInRange(t *testing.T) {
	const cidr = "241.0.0.0/8"
	keys := []dns.ServiceKey{
		{Namespace: "prod", Name: "payments"},
		{Namespace: "prod", Name: "ledger"},
		{Namespace: "data", Name: "warehouse"},
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("parsing cidr: %v", err)
	}

	first, err := dns.AllocateClusterSetIPs(cidr, keys)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	second, err := dns.AllocateClusterSetIPs(cidr, keys)
	if err != nil {
		t.Fatalf("allocate (second run): %v", err)
	}

	if len(first) != len(keys) {
		t.Fatalf("expected a VIP per service, got %d", len(first))
	}
	for k, ip := range first {
		if second[k] != ip {
			t.Errorf("%v: allocation not deterministic: %q vs %q", k, ip, second[k])
		}
		if parsed := net.ParseIP(ip); parsed == nil || !ipnet.Contains(parsed) {
			t.Errorf("%v: VIP %q is not within %s", k, ip, cidr)
		}
	}
}

// KEP-1645 §"Constraints": ServiceExport and ServiceImport are namespaced, so a
// service's identity is its (namespace, name) across the clusterset. Assert the
// shipped CRDs declare Namespaced scope.
func TestMCSObjectsAreNamespaced(t *testing.T) {
	for _, file := range []string{
		"multicluster.x-k8s.io_serviceexports.yaml",
		"multicluster.x-k8s.io_serviceimports.yaml",
	} {
		crd := readCRD(t, file)
		if crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
			t.Errorf("%s: scope is %q, want Namespaced (KEP-1645)", file, crd.Spec.Scope)
		}
	}
}

// PlanServiceImport is the pure aggregation behind a ServiceImport. A single
// exporter must yield an import that exists, carries the declared type, and
// merges the ports — the export → import contract with no cluster.
func TestServiceImportAggregation(t *testing.T) {
	plan := dns.PlanServiceImport([]dns.ExportedEndpoint{
		{
			Cluster: "east",
			Type:    mcsv1alpha1.ClusterSetIP,
			Ports:   []mcsv1alpha1.ServicePort{{Name: "http", Protocol: "TCP", Port: 80}},
			IPs:     []string{"10.0.0.10"},
		},
	})
	if !plan.Exists {
		t.Fatal("a single exporter should produce an import")
	}
	if plan.Type != mcsv1alpha1.ClusterSetIP {
		t.Errorf("import type = %q, want ClusterSetIP", plan.Type)
	}
	if len(plan.Ports) != 1 || plan.Ports[0].Port != 80 {
		t.Errorf("import ports not carried: %+v", plan.Ports)
	}
	if len(plan.Clusters) != 1 || plan.Clusters[0] != "east" {
		t.Errorf("contributing clusters not recorded: %+v", plan.Clusters)
	}
}

// KEP-1645 conflict resolution: a type disagreement is resolved in favor of the
// oldest ServiceExport, and the losing export is reported as a conflict. Here
// the older "west" export wins despite "east" sorting first by cluster ID.
func TestServiceImportTypeConflictResolvedByOldest(t *testing.T) {
	plan := dns.PlanServiceImport([]dns.ExportedEndpoint{
		{Cluster: "east", Type: mcsv1alpha1.ClusterSetIP, ExportTime: 200, IPs: []string{"10.0.0.10"}},
		{Cluster: "west", Type: mcsv1alpha1.Headless, ExportTime: 100, IPs: []string{"10.1.0.10"}},
	})
	if !plan.Exists {
		t.Fatal("the import should still exist on a conflict")
	}
	if plan.Type != mcsv1alpha1.Headless {
		t.Errorf("the oldest export should win the type: want Headless, got %q", plan.Type)
	}
	if !plan.HasConflicts() {
		t.Error("a type disagreement between exporters must be reported as a conflict")
	}
}

// KEP-1645 §"EndpointSlices": a consuming cluster materializes an imported
// service's endpoints as discovery.k8s.io EndpointSlices labeled with the
// multicluster service name and source cluster, so native consumers discover
// them through the standard surface. DataWerx does this for headless imports.
func TestHeadlessImportMirrorsLabeledEndpointSlices(t *testing.T) {
	slices := dns.PlanEndpointSlices("payments", "prod", []dns.ExportedEndpoint{
		{Cluster: "east", Type: mcsv1alpha1.Headless, IPs: []string{"10.0.0.1"}},
		{Cluster: "west", Type: mcsv1alpha1.Headless, IPs: []string{"10.1.0.1"}},
	})
	if len(slices) == 0 {
		t.Fatal("a headless import must mirror EndpointSlices into the consuming cluster")
	}
	for _, s := range slices {
		if s.Labels[mcsv1alpha1.LabelServiceName] != "payments" {
			t.Errorf("mirrored slice must carry the multicluster service-name label, got %q", s.Labels[mcsv1alpha1.LabelServiceName])
		}
		if s.Labels[mcsv1alpha1.LabelSourceCluster] == "" {
			t.Error("mirrored slice must record its source cluster")
		}
		if len(s.Endpoints) == 0 {
			t.Error("mirrored slice must carry endpoints")
		}
	}
}

func readCRD(t *testing.T, file string) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", file))
	if err != nil {
		t.Fatalf("reading CRD %s: %v", file, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parsing CRD %s: %v", file, err)
	}
	return crd
}

package dns

import (
	"strings"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
)

// clusterSetZoneSuffix is the DNS suffix every imported service name ends with:
// "<svc>.<ns>.svc.clusterset.local". We answer this zone authoritatively.
const clusterSetZoneSuffix = "svc." + ClusterSetDomain

// ResolvedService is what the clusterset.local zone knows about an imported
// service: its consumption type and the IP(s) the name should resolve to
// (the ClusterSetIP for a ClusterSetIP service, or the union of backing
// endpoint IPs for a headless one).
type ResolvedService struct {
	Type mcsv1alpha1.ServiceImportType
	IPs  []string
}

// InClusterSetZone reports whether qname falls within the clusterset.local zone
// we are authoritative for. The trailing dot and case are normalized.
func InClusterSetZone(qname string) bool {
	n := normalizeName(qname)
	return n == clusterSetZoneSuffix || strings.HasSuffix(n, "."+clusterSetZoneSuffix)
}

// ParseClusterSetName extracts the service name and namespace from a
// clusterset.local query name of the form
// "<name>.<namespace>.svc.clusterset.local[.]" and is case-insensitive. It returns
// ok=false for names that are in the zone but not a plain service A/AAAA name
// (e.g. SRV-style _port._proto labels, or malformed depth), and for names
// outside the zone.
func ParseClusterSetName(qname string) (name, namespace string, ok bool) {
	n := normalizeName(qname)
	if !strings.HasSuffix(n, "."+clusterSetZoneSuffix) {
		return "", "", false
	}
	prefix := strings.TrimSuffix(n, "."+clusterSetZoneSuffix)
	if prefix == "" {
		return "", "", false
	}
	labels := strings.Split(prefix, ".")
	if len(labels) != 2 {
		return "", "", false
	}
	if labels[0] == "" || labels[1] == "" {
		return "", "", false
	}
	return labels[0], labels[1], true
}

// normalizeName lowercases a DNS name and strips a single trailing dot.
func normalizeName(qname string) string {
	n := strings.ToLower(qname)
	return strings.TrimSuffix(n, ".")
}

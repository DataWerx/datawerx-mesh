package dns

import (
	"sort"
)

// ServiceKey identifies a cross-cluster service by its namespace and name. The
// resulting ServiceImport shares this namespace and name.
type ServiceKey struct {
	Namespace string
	Name      string
}

func (k ServiceKey) String() string { return k.Namespace + "/" + k.Name }

// ExportedService is one cluster's published export of a single service: the
// service identity plus that cluster's ExportedEndpoint. The import controller
// builds these from EndpointExport objects and feeds them to GroupExports.
type ExportedService struct {
	Namespace string
	Name      string
	Endpoint  ExportedEndpoint
}

// Key returns the ServiceKey for this exported service.
func (e ExportedService) Key() ServiceKey {
	return ServiceKey{Namespace: e.Namespace, Name: e.Name}
}

// GroupExports buckets exported services by ServiceKey and returns both the
// grouping and the deterministically sorted list of keys. The sorted keys are
// what AllocateClusterSetIPs consumes so that every cluster, seeing the same
// set of exports, computes identical ClusterSetIP assignments without any
// central coordinator.
//
// Within each bucket the endpoints are sorted by cluster ID so downstream
// aggregation PlanServiceImport is order-independent.
func GroupExports(exports []ExportedService) (map[ServiceKey][]ExportedEndpoint, []ServiceKey) {
	grouped := make(map[ServiceKey][]ExportedEndpoint)
	for _, e := range exports {
		k := e.Key()
		grouped[k] = append(grouped[k], e.Endpoint)
	}

	keys := make([]ServiceKey, 0, len(grouped))
	for k := range grouped {
		eps := grouped[k]
		sort.Slice(eps, func(i, j int) bool { return eps[i].Cluster < eps[j].Cluster })
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })

	return grouped, keys
}

// SortServiceKeys returns a deterministically ordered copy of keys.
func SortServiceKeys(keys []ServiceKey) []ServiceKey {
	out := append([]ServiceKey(nil), keys...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

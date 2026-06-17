package dns

import (
	"sort"

	discoveryv1 "k8s.io/api/discovery/v1"
)

// ReadyEndpointIPs returns the de-duplicated, sorted set of endpoint addresses
// across the given EndpointSlices that are eligible to serve traffic.
//
// An address is included when its endpoint is Ready or has no Ready condition
// (which Kubernetes treats as ready), and is not Terminating. This is the set a
// headless service should resolve to. The function is pure so the export
// controller can be unit-tested without a cluster.
func ReadyEndpointIPs(slices []discoveryv1.EndpointSlice) []string {
	seen := map[string]struct{}{}
	for i := range slices {
		for _, ep := range slices[i].Endpoints {
			if !endpointServing(ep) {
				continue
			}
			for _, addr := range ep.Addresses {
				if addr == "" {
					continue
				}
				seen[addr] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for ip := range seen {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// endpointServing reports whether an endpoint should contribute its addresses:
// ready and not terminating (nil Ready is treated as ready).
func endpointServing(ep discoveryv1.Endpoint) bool {
	if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
		return false
	}
	if ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
		return false
	}
	return true
}

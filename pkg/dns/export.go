package dns

import (
	corev1 "k8s.io/api/core/v1"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
)

// ServiceIsHeadless reports whether a Service is headless, meaning
// ClusterIP "None".
// Headless services resolve to the union of backing pod IPs rather than a
// virtual ClusterSetIP.
func ServiceIsHeadless(svc *corev1.Service) bool {
	return svc.Spec.ClusterIP == corev1.ClusterIPNone
}

// BuildExportedEndpoint derives this cluster's contribution to a cross-cluster
// service from the local Service object. It is pure, no API/network, so it can
// be unit-tested exhaustively and reused by both the export and import paths.
//
//   - Type is Headless for headless Services, otherwise ClusterSetIP.
//   - Ports are mapped from the Service's ports (name, protocol, appProtocol,
//     port). TargetPort/NodePort are intentionally dropped: they are local
//     concerns and meaningless to remote clusters.
//   - IPs carries the Service's ClusterIP(s) for a ClusterSetIP service and
//     is empty for headless services, whose pod IPs propagate separately
//     via EndpointSlices.
func BuildExportedEndpoint(clusterID string, svc *corev1.Service) ExportedEndpoint {
	ep := ExportedEndpoint{Cluster: clusterID}

	if ServiceIsHeadless(svc) {
		ep.Type = mcsv1alpha1.Headless
	} else {
		ep.Type = mcsv1alpha1.ClusterSetIP
		ep.IPs = clusterIPs(svc)
	}

	for _, p := range svc.Spec.Ports {
		sp := mcsv1alpha1.ServicePort{
			Name:     p.Name,
			Protocol: p.Protocol,
			Port:     p.Port,
		}
		if p.AppProtocol != nil {
			v := *p.AppProtocol
			sp.AppProtocol = &v
		}
		ep.Ports = append(ep.Ports, sp)
	}

	return ep
}

// clusterIPs returns the routable ClusterIP(s) for a non-headless Service,
// preferring the dual-stack ClusterIPs list and falling back to the singular
// ClusterIP. Unset ("") and "None" sentinels are filtered out.
func clusterIPs(svc *corev1.Service) []string {
	candidates := svc.Spec.ClusterIPs
	if len(candidates) == 0 && svc.Spec.ClusterIP != "" {
		candidates = []string{svc.Spec.ClusterIP}
	}
	var out []string
	for _, ip := range candidates {
		if ip == "" || ip == corev1.ClusterIPNone {
			continue
		}
		out = append(out, ip)
	}
	return out
}

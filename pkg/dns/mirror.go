package dns

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
)

const (
	// MirrorManagedBy marks the EndpointSlices DataWerx mirrors for imported
	// services, so the cluster's native EndpointSlice machinery leaves them alone
	// and an operator can tell at a glance who owns a slice.
	MirrorManagedBy = "datawerx-mesh.networking.datawerx.io"

	// maxEndpointsPerSlice caps the endpoints packed into one mirrored slice,
	// matching the Kubernetes EndpointSlice controller's default so consumers see
	// familiar slice sizes.
	maxEndpointsPerSlice = 100
)

// PlanEndpointSlices builds the desired set of mirrored EndpointSlices for a
// headless imported service from the cross-cluster endpoints contributing to it.
// Per KEP-1645 the consuming cluster materializes the imported endpoints into
// its own discovery.k8s.io API, so native consumers (and an MCS-aware
// kube-proxy) discover them through the standard surface, not only the
// clusterset DNS responder.
//
// It is pure — the import controller reconciles the returned objects against
// what exists. Slices are grouped per source cluster and address family and
// capped, with deterministic names and ordering so a given input always yields
// byte-identical output and the controller's diff is stable.
func PlanEndpointSlices(importName, namespace string, endpoints []ExportedEndpoint) []discoveryv1.EndpointSlice {
	var out []discoveryv1.EndpointSlice

	// Endpoints arrive grouped per cluster; sort defensively so the output is
	// independent of input order.
	sorted := append([]ExportedEndpoint(nil), endpoints...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Cluster < sorted[j].Cluster })

	for _, ep := range sorted {
		ports := mirrorPorts(ep.Ports)
		for _, fam := range []discoveryv1.AddressType{discoveryv1.AddressTypeIPv4, discoveryv1.AddressTypeIPv6} {
			addrs := addressesOfFamily(ep.IPs, fam)
			for chunk := 0; chunk < len(addrs); chunk += maxEndpointsPerSlice {
				end := chunk + maxEndpointsPerSlice
				if end > len(addrs) {
					end = len(addrs)
				}
				out = append(out, mirrorSlice(importName, namespace, ep.Cluster, fam, addrs[chunk:end], ports, chunk/maxEndpointsPerSlice))
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// mirrorSlice assembles one EndpointSlice for a single source cluster, address
// family, and chunk of addresses.
func mirrorSlice(importName, namespace, cluster string, fam discoveryv1.AddressType, addrs []string, ports []discoveryv1.EndpointPort, chunk int) discoveryv1.EndpointSlice {
	ready := true
	eps := make([]discoveryv1.Endpoint, 0, len(addrs))
	for _, a := range addrs {
		eps = append(eps, discoveryv1.Endpoint{
			Addresses:  []string{a},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		})
	}
	return discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mirrorSliceName(importName, cluster, fam, chunk),
			Namespace: namespace,
			Labels: map[string]string{
				mcsv1alpha1.LabelServiceName:   importName,
				mcsv1alpha1.LabelSourceCluster: cluster,
				discoveryv1.LabelManagedBy:     MirrorManagedBy,
			},
		},
		AddressType: fam,
		Endpoints:   eps,
		Ports:       ports,
	}
}

// mirrorSliceName builds a deterministic, DNS-safe, length-bounded slice name.
// The readable prefix aids debugging; a hash of the full identity keeps it
// unique and within the 253-character object-name limit when inputs are long.
func mirrorSliceName(importName, cluster string, fam discoveryv1.AddressType, chunk int) string {
	famShort := "ipv4"
	if fam == discoveryv1.AddressTypeIPv6 {
		famShort = "ipv6"
	}
	full := fmt.Sprintf("%s-%s-%s-%d", importName, cluster, famShort, chunk)
	const maxName = 253
	if len(full) <= maxName {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "-" + hex.EncodeToString(sum[:8])
	return full[:maxName-len(suffix)] + suffix
}

// mirrorPorts translates MCS ServicePorts into EndpointSlice ports, defaulting
// the protocol to TCP as the EndpointSlice API expects a concrete value.
func mirrorPorts(in []mcsv1alpha1.ServicePort) []discoveryv1.EndpointPort {
	if len(in) == 0 {
		return nil
	}
	out := make([]discoveryv1.EndpointPort, 0, len(in))
	for _, p := range in {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		name := p.Name
		port := p.Port
		ep := discoveryv1.EndpointPort{Protocol: &proto, Port: &port}
		if name != "" {
			ep.Name = &name
		}
		if p.AppProtocol != nil {
			ap := *p.AppProtocol
			ep.AppProtocol = &ap
		}
		out = append(out, ep)
	}
	return out
}

// addressesOfFamily returns the sorted, de-duplicated addresses of in that match
// the given address family.
func addressesOfFamily(in []string, fam discoveryv1.AddressType) []string {
	seen := map[string]struct{}{}
	for _, ip := range in {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			continue
		}
		isV4 := parsed.To4() != nil
		if (fam == discoveryv1.AddressTypeIPv4) != isV4 {
			continue
		}
		seen[ip] = struct{}{}
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

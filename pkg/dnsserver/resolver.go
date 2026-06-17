package dnsserver

import (
	"context"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	dwxdns "github.com/DataWerx/datawerx-mesh/pkg/dns"
)

// CachedResolver answers clusterset.local lookups from ServiceImport and
// EndpointExport objects via a controller-runtime cached reader, so each query
// is an in-memory lookup with no API round-trip.
type CachedResolver struct {
	Reader client.Reader
	// Timeout bounds the cache reads. Defaults to 2s.
	Timeout time.Duration
}

// LookupClusterSet returns the imported service for namespace/name.
//
//   - ClusterSetIP services resolve to the allocated virtual IP recorded on the
//     ServiceImport.
//   - Headless services resolve to the union of backing pod IPs published by all
//     exporting clusters, read from the EndpointExports. (A headless
//     ServiceImport carries no Spec.IPs, per the MCS contract.)
//
// found is false with a nil error only when the service is genuinely not
// imported. An imported service with no addresses returns found=true with empty
// IPs - the server then answers NXDOMAIN. A non-nil error signals a lookup
// FAILURE (cache miss-sync, timeout, transport error) and is distinct from
// "not found" so the server can answer SERVFAIL rather than poisoning downstream
// caches with a negative NXDOMAIN.
func (c *CachedResolver) LookupClusterSet(namespace, name string) (dwxdns.ResolvedService, bool, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var si mcsv1alpha1.ServiceImport
	if err := c.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &si); err != nil {
		if apierrors.IsNotFound(err) {
			return dwxdns.ResolvedService{}, false, nil
		}
		return dwxdns.ResolvedService{}, false, err
	}

	if si.Spec.Type == mcsv1alpha1.Headless {
		ips, err := c.headlessIPs(ctx, namespace, name)
		if err != nil {
			return dwxdns.ResolvedService{}, false, err
		}
		return dwxdns.ResolvedService{Type: mcsv1alpha1.Headless, IPs: ips}, true, nil
	}

	return dwxdns.ResolvedService{
		Type: si.Spec.Type,
		IPs:  append([]string(nil), si.Spec.IPs...),
	}, true, nil
}

// headlessIPs unions the endpoint addresses every cluster published for the
// service, sorted and de-duplicated.
func (c *CachedResolver) headlessIPs(ctx context.Context, namespace, name string) ([]string, error) {
	var list networkingv1alpha1.EndpointExportList
	if err := c.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for i := range list.Items {
		spec := list.Items[i].Spec
		if spec.ServiceNamespace != namespace || spec.ServiceName != name {
			continue
		}
		for _, ip := range spec.IPs {
			if ip != "" {
				seen[ip] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(seen))
	for ip := range seen {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out, nil
}

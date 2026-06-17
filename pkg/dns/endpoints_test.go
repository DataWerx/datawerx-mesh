package dns_test

import (
	"reflect"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/DataWerx/datawerx-mesh/pkg/dns"
)

func boolp(b bool) *bool { return &b }

func slice(eps ...discoveryv1.Endpoint) discoveryv1.EndpointSlice {
	return discoveryv1.EndpointSlice{Endpoints: eps}
}

func TestReadyEndpointIPs(t *testing.T) {
	tests := []struct {
		name   string
		slices []discoveryv1.EndpointSlice
		want   []string
	}{
		{
			name:   "nil",
			slices: nil,
			want:   nil,
		},
		{
			name: "ready endpoints across slices, sorted + deduped",
			slices: []discoveryv1.EndpointSlice{
				slice(
					discoveryv1.Endpoint{Addresses: []string{"10.0.0.3"}, Conditions: discoveryv1.EndpointConditions{Ready: boolp(true)}},
					discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}},
				),
				slice(
					discoveryv1.Endpoint{Addresses: []string{"10.0.0.1", "10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: boolp(true)}},
				),
			},
			want: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		},
		{
			name: "not-ready excluded",
			slices: []discoveryv1.EndpointSlice{
				slice(
					discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: boolp(false)}},
					discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: boolp(true)}},
				),
			},
			want: []string{"10.0.0.2"},
		},
		{
			name: "terminating excluded",
			slices: []discoveryv1.EndpointSlice{
				slice(
					discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: boolp(true), Terminating: boolp(true)}},
					discoveryv1.Endpoint{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: boolp(true)}},
				),
			},
			want: []string{"10.0.0.2"},
		},
		{
			name: "nil Ready treated as ready",
			slices: []discoveryv1.EndpointSlice{
				slice(discoveryv1.Endpoint{Addresses: []string{"10.0.0.9"}}),
			},
			want: []string{"10.0.0.9"},
		},
		{
			name: "empty addresses skipped",
			slices: []discoveryv1.EndpointSlice{
				slice(discoveryv1.Endpoint{Addresses: []string{""}, Conditions: discoveryv1.EndpointConditions{Ready: boolp(true)}}),
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dns.ReadyEndpointIPs(tt.slices); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ReadyEndpointIPs() = %v, want %v", got, tt.want)
			}
		})
	}
}

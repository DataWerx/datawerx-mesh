package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/impact"
	"github.com/datawerx/datawerx/pkg/meshfw"
	"github.com/datawerx/datawerx/pkg/topology"
)

// runPolicy implements `dwxctl policy --dry-run -f <file>`: it loads a proposed
// MeshNetworkPolicy or MeshPeer from a manifest and reports the impact of
// applying it against the cluster's current state — without applying anything.
// It is the free safety net for cross-cluster policy: see what a change exposes
// or breaks before it lands.
func runPolicy(args []string) int {
	fs := flag.NewFlagSet("policy", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "analyze the impact of the proposed manifest (required)")
	file := fs.String("f", "", "path to a proposed MeshNetworkPolicy or MeshPeer manifest (YAML/JSON)")
	localCIDRs := fs.String("local-cidrs", os.Getenv("DataWerx_LOCAL_CIDRS"), "comma-separated local pod/service ranges (for peer overlap analysis)")
	output := fs.String("output", "text", "output format: text|json")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	_ = fs.Parse(args)

	if !*dryRun {
		fmt.Fprintln(os.Stderr, "error: only --dry-run is supported (dwxctl never applies policy)")
		return 2
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "error: -f <manifest> is required")
		return 2
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	var tm metav1.TypeMeta
	if err := yaml.Unmarshal(raw, &tm); err != nil {
		fmt.Fprintf(os.Stderr, "error: parsing manifest: %v\n", err)
		return 1
	}

	c, err := newClient(*kubeconfig, *kubecontext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch tm.Kind {
	case "MeshNetworkPolicy":
		return policyImpact(ctx, c, raw, *output)
	case "MeshPeer":
		return peerImpact(ctx, c, raw, splitCSV(*localCIDRs), *output)
	default:
		fmt.Fprintf(os.Stderr, "error: unsupported kind %q (want MeshNetworkPolicy or MeshPeer)\n", tm.Kind)
		return 2
	}
}

// policyImpact analyzes a proposed MeshNetworkPolicy against the cluster's
// current policy set and peer topology.
func policyImpact(ctx context.Context, c client.Client, raw []byte, output string) int {
	var proposed networkingv1alpha1.MeshNetworkPolicy
	if err := yaml.Unmarshal(raw, &proposed); err != nil {
		fmt.Fprintf(os.Stderr, "error: parsing MeshNetworkPolicy: %v\n", err)
		return 1
	}

	var current networkingv1alpha1.MeshNetworkPolicyList
	if err := c.List(ctx, &current); err != nil {
		fmt.Fprintf(os.Stderr, "error: listing MeshNetworkPolicies: %v\n", err)
		return 1
	}
	clusterCIDRs, err := peerCIDRs(ctx, c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	curr := make([]meshfw.Policy, 0, len(current.Items))
	prop := make([]meshfw.Policy, 0, len(current.Items)+1)
	replaced := false
	for i := range current.Items {
		fw := toFWPolicy(current.Items[i])
		curr = append(curr, fw)
		if current.Items[i].Name == proposed.Name {
			prop = append(prop, toFWPolicy(proposed)) // the change replaces the existing one
			replaced = true
		} else {
			prop = append(prop, fw)
		}
	}
	if !replaced {
		prop = append(prop, toFWPolicy(proposed)) // a brand-new policy
	}

	imp := impact.AnalyzePolicyChange(curr, prop, clusterCIDRs)
	if output == "json" {
		s, err := jsonMarshal(imp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Println(s)
		return 0
	}
	writePolicyImpact(proposed.Name, imp)
	return 0
}

// peerImpact analyzes a proposed MeshPeer against the cluster's current peers.
func peerImpact(ctx context.Context, c client.Client, raw []byte, localCIDRs []string, output string) int {
	var proposed networkingv1alpha1.MeshPeer
	if err := yaml.Unmarshal(raw, &proposed); err != nil {
		fmt.Fprintf(os.Stderr, "error: parsing MeshPeer: %v\n", err)
		return 1
	}

	var peers networkingv1alpha1.MeshPeerList
	if err := c.List(ctx, &peers); err != nil {
		fmt.Fprintf(os.Stderr, "error: listing MeshPeers: %v\n", err)
		return 1
	}
	var existing []topology.PeerIdentity
	for i := range peers.Items {
		p := &peers.Items[i]
		if p.Name == proposed.Name {
			continue // the change replaces this one; analyze against the others
		}
		existing = append(existing, topology.PeerIdentity{
			ClusterID: p.Spec.ClusterID, PublicKey: p.Spec.PublicKey,
			Endpoint: p.Spec.Endpoint, CIDRs: p.Spec.AllCIDRs(),
		})
	}

	imp := impact.AnalyzePeerChange(proposed.Spec, localCIDRs, existing)
	if output == "json" {
		s, err := jsonMarshal(imp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Println(s)
		return 0
	}
	writePeerImpact(proposed.Name, imp)
	if !imp.Safe() {
		return 1
	}
	return 0
}

// peerCIDRs builds the cluster-ID → CIDRs map the firewall compiler resolves
// selectors against, from the cluster's MeshPeers.
func peerCIDRs(ctx context.Context, c client.Client) (map[string][]string, error) {
	var peers networkingv1alpha1.MeshPeerList
	if err := c.List(ctx, &peers); err != nil {
		return nil, fmt.Errorf("listing MeshPeers: %w", err)
	}
	m := map[string][]string{}
	for i := range peers.Items {
		p := &peers.Items[i]
		m[p.Spec.ClusterID] = p.Spec.AllCIDRs()
	}
	return m, nil
}

// toFWPolicy projects a MeshNetworkPolicy CRD onto the pure meshfw.Policy the
// compiler consumes.
func toFWPolicy(p networkingv1alpha1.MeshNetworkPolicy) meshfw.Policy {
	out := meshfw.Policy{Name: p.Name, Destinations: p.Spec.Destinations}
	for _, ing := range p.Spec.Ingress {
		r := meshfw.IngressRule{}
		for _, f := range ing.From {
			r.From = append(r.From, meshfw.PeerSelector{ClusterIDs: f.ClusterIDs, CIDRs: f.CIDRs})
		}
		for _, pt := range ing.Ports {
			r.Ports = append(r.Ports, meshfw.Port{Protocol: pt.Protocol, Port: pt.Port})
		}
		out.Ingress = append(out.Ingress, r)
	}
	return out
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

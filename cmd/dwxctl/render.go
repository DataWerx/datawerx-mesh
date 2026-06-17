package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/DataWerx/datawerx-mesh/pkg/impact"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// jsonMarshal renders v as indented JSON for the --output json paths.
func jsonMarshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// writeFindings prints diagnosis findings in a human-readable form, grouped most
// severe first - Diagnose already returns them in that order. Each line leads
// with the severity, then the title, the cited signal, and the suggested fix —
// so the grounding is always visible, never just an assertion.
func writeFindings(findings []verify.Finding) {
	if len(findings) == 0 {
		fmt.Println("No obvious causes found. The mesh looks healthy to the rule-based checker.")
		return
	}
	for _, f := range findings {
		fmt.Printf("[%s] %s\n", f.Severity, f.Title)
		if f.Detail != "" {
			fmt.Printf("    %s\n", f.Detail)
		}
		fmt.Printf("    signal: %s\n", f.Signal)
		if f.Remedy != "" {
			fmt.Printf("    fix:    %s\n", f.Remedy)
		}
		fmt.Fprintln(os.Stdout)
	}
}

// writePolicyImpact renders a MeshNetworkPolicy dry-run in human-readable form.
func writePolicyImpact(name string, imp impact.PolicyImpact) {
	fmt.Printf("Dry-run impact of MeshNetworkPolicy %q:\n\n", name)
	writeList("Newly protected destinations (become default-deny)", protectedStrings(imp.NewlyProtected))
	writeList("No longer protected", protectedStrings(imp.NoLongerProtected))
	writeExposures("Newly exposed (now reachable)", imp.NewlyExposed)
	writeExposures("Newly denied (would BREAK these flows)", imp.NewlyDenied)
	if len(imp.Skipped) > 0 {
		writeList("Skipped inputs (not programmed)", imp.Skipped)
	}
	if len(imp.Warnings) > 0 {
		fmt.Println("⚠ Warnings:")
		for _, w := range imp.Warnings {
			fmt.Printf("  - %s\n", w)
		}
		fmt.Println()
	}
	if len(imp.NewlyExposed) == 0 && len(imp.NewlyDenied) == 0 &&
		len(imp.NewlyProtected) == 0 && len(imp.NoLongerProtected) == 0 && len(imp.Warnings) == 0 {
		fmt.Println("No change to the programmed firewall.")
	}
}

// writePeerImpact renders a MeshPeer dry-run in human-readable form.
func writePeerImpact(name string, imp impact.PeerImpact) {
	fmt.Printf("Dry-run impact of MeshPeer %q:\n\n", name)
	fmt.Printf("  Resulting phase: %s\n", imp.Phase)
	fmt.Printf("  %s\n\n", imp.Message)
	writeList("Routable CIDRs", imp.Routable)
	writeList("Withheld CIDRs (overlap/malformed — not routed)", imp.Withheld)
	writeList("Dangerous CIDRs (never routed into the mesh)", imp.Dangerous)
	writeList("New topology conflicts", imp.NewConflicts)
	if imp.Safe() {
		fmt.Println("✓ Safe to apply: the peer programs cleanly with no conflicts.")
	} else {
		fmt.Println("✗ Not safe to apply as-is: resolve the items above first.")
	}
}

func writeExposures(title string, es []impact.Exposure) {
	if len(es) == 0 {
		return
	}
	fmt.Printf("%s:\n", title)
	for _, e := range es {
		fmt.Printf("  - %s\n", e)
	}
	fmt.Println()
}

func writeList(title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Printf("%s:\n", title)
	for _, it := range items {
		fmt.Printf("  - %s\n", it)
	}
	fmt.Println()
}

// protectedStrings renders the "" (protect-all) sentinel readably.
func protectedStrings(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		if s == "" {
			out[i] = "<all mesh ingress>"
		} else {
			out[i] = s
		}
	}
	return out
}

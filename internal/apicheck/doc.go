// Package apicheck holds consistency tests that guard the three hand-maintained
// API surfaces against silent drift: the Go types, their hand-written
// zz_generated.deepcopy.go, and the hand-written CRD YAML under config/crd.
//
// There is no controller-gen in this repository by design, so
// nothing regenerates these from a single source. These tests are the safety
// net that would otherwise be a code generator: they fail when a field is added
// to a Go type but not reflected in the deepcopy or the CRD schema.
//
// The package intentionally contains no production code — only tests.
package apicheck

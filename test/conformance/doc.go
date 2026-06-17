// Package conformance machine-checks DataWerx's Multi-Cluster Services
// implementation against the KEP-1645 API contract, so the claims in
// docs/mcs-conformance.md are executable rather than asserted in prose.
//
// It is hermetic — pure types and pure DNS/allocation logic, no cluster — so it
// runs in the default `go test ./...` on every push. The full upstream MCS
// conformance e2e needs a real clusterset and the upstream API group (a known
// delta, since DataWerx ships a focused subset of the types); this suite covers
// the contract that can be verified without one.
//
// The package intentionally contains no production code — only tests.
package conformance

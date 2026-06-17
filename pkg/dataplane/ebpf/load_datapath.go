//go:build ebpf_datapath

package ebpf

import (
	"errors"

	"github.com/go-logr/logr"
)

// ErrNotCompiled mirrors the open-core sentinel so callers can errors.Is on it
// under either build tag.
//
// This file is the seam the premium build overlays with the real loader.  It
// loads the compiled CO-RE object (generated via bpf2go), attaches the TC
// clsact ingress/egress programs to the mesh device, and returns a
// cilium-ebpf-backed MapOps bound to the program's LPM-trie maps. The
// open-core tree ships only this placeholder so `go build -tags ebpf_datapath`
// still compiles; it returns an error rather than silently no-op'ing.
var ErrNotCompiled = errors.New("ebpf remap datapath loader is not linked into this build; the compiled BPF object + libbpf-backed MapOps are provided by the premium build — see docs/design/0003-ebpf-overlap-remap.md")

// Load is overlaid by the premium build with the real TC/eBPF loader. The
// open-core placeholder returns ErrNotCompiled.
func Load(iface string, log logr.Logger) (MapOps, error) {
	return nil, ErrNotCompiled
}

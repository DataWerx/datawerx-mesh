//go:build !ebpf_datapath

package ebpf

import (
	"errors"

	"github.com/go-logr/logr"
)

// ErrNotCompiled is returned by Load in the open-core binary, which ships
// without the compiled CO-RE object and the libbpf/cilium-ebpf-backed MapOps.
var ErrNotCompiled = errors.New("ebpf remap datapath is a premium build: rebuild the agent with -tags ebpf_datapath (and the compiled BPF object) to enable it; see docs/design/0003-ebpf-overlap-remap.md")

// Load attaches the TC/eBPF remap programs to iface and returns a MapOps bound
// to their maps. In the open-core build it is a stub that returns
// ErrNotCompiled so selecting the eBPF backend fails fast and explicitly at
// startup, rather than silently degrading. The premium build replaces this file
// with the real loader gated behind the ebpf_datapath tag.
func Load(iface string, log logr.Logger) (MapOps, error) {
	return nil, ErrNotCompiled
}

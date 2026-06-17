package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

const (
	// ResponderPath is the HTTP path the responder serves and the prober dials.
	// It is namespaced under /dwx so it never collides with an application's own
	// health routes when the responder shares a host.
	ResponderPath = "/dwx/probe"

	// DefaultResponderAddr is the responder's default listen address. The port
	// sits above the ephemeral range used by WireGuard and the DNS responder.
	DefaultResponderAddr = ":9998"

	// envelopeMarker tags the probe response body so the prober can tell a real
	// responder from an arbitrary service that happens to return 200.
	envelopeMarker = "datawerx-mesh-probe"
)

// probeEnvelope is the JSON a responder returns: a stable marker plus the
// answering cluster's ID, which the prober checks against the cluster it dialed
// to catch misrouted traffic.
type probeEnvelope struct {
	Marker    string `json:"marker"`
	ClusterID string `json:"clusterID"`
}

// ProbeBody is the response body a responder for clusterID returns. It is
// exported so the prober and the tests share one definition of the envelope.
func ProbeBody(clusterID string) []byte {
	b, _ := json.Marshal(probeEnvelope{Marker: envelopeMarker, ClusterID: clusterID})
	return b
}

// parseProbeBody extracts the answering cluster ID from a probe response,
// reporting false unless the body is a well-formed DataWerx probe envelope.
func parseProbeBody(body []byte) (clusterID string, ok bool) {
	var env probeEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", false
	}
	if env.Marker != envelopeMarker {
		return "", false
	}
	return env.ClusterID, true
}

// Responder is a tiny HTTP server that answers a liveness probe from remote
// clusters, proving application-layer reachability *into* this cluster over the
// mesh. It is a thin controller-runtime Runnable: every node runs one, and a
// remote prober dials it at ResponderPath. The handler is trivial by design —
// answering at all is the signal — so there is no Resolver seam, just the local
// cluster ID baked into the envelope.
type Responder struct {
	// Addr is the listen address (host:port). Defaults to DefaultResponderAddr.
	Addr string
	// ClusterID is this cluster's mesh ID, returned in the envelope.
	ClusterID string
	// Log is optional.
	Log logr.Logger

	srv *http.Server
}

// NeedLeaderElection makes the responder run on every pod even when leader
// election is enabled: a remote prober dials each node's local responder.
func (r *Responder) NeedLeaderElection() bool { return false }

// Start runs the HTTP listener until the context is cancelled, satisfying
// manager.Runnable.
func (r *Responder) Start(ctx context.Context) error {
	addr := r.Addr
	if addr == "" {
		addr = DefaultResponderAddr
	}
	mux := http.NewServeMux()
	mux.HandleFunc(ResponderPath, r.handle)
	r.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- r.srv.ListenAndServe() }()
	r.Log.Info("mesh probe responder listening", "addr", addr, "path", ResponderPath)

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.srv.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// handle answers a probe with the local cluster's envelope. Any other path is a
// 404 so the responder cannot be mistaken for a general-purpose endpoint.
func (r *Responder) handle(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != ResponderPath {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ProbeBody(r.ClusterID))
}

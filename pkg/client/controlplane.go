// Package client defines the control-plane abstraction that decouples the
// open-source reconciliation core from where mesh topology originates.
//
// The single ControlPlaneClient interface is the seam along which DataWerx
// Mesh is split into its open-core and premium tiers:
//
//   - LocalGitOpsClient - Free/Open tier - reads MeshPeer CRDs straight out of
//     the local cluster. Peers are authored by the user's GitOps pipeline and
//     authentication is whatever the pod's ServiceAccount RBAC already grants.
//
//   - EnterpriseControlPlaneClient - Premium tier - hooks into a centralized
//     managed SaaS control plane, authenticating with an OIDC/SSO machine
//     token and pulling a globally reconciled topology map.
//
// Because both implementations satisfy the same interface, the reconciler and
// the manager bootstrap never branch on tier: the correct client is selected
// once, at startup, and injected. Premium logic is therefore additive and can
// be compiled in or swapped out without touching core control flow.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
)

// EnterpriseSSOTokenEnv is the environment variable that carries the machine
// identity token minted by the enterprise OIDC/SSO identity provider. The
// premium client injects its value as a bearer token on every control-plane
// request.
const EnterpriseSSOTokenEnv = "DataWerx_ENTERPRISE_SSO_TOKEN"

// RemotePeerConfig is the transport-neutral, tier-neutral description of a
// single remote cluster as returned by a control plane. It is deliberately a
// plain value type (no Kubernetes machinery) so that the enterprise client can
// unmarshal it directly from a REST/gRPC payload and the local client can
// project CRDs into it, yielding one uniform shape for the reconciler.
type RemotePeerConfig struct {
	// ClusterID is the stable unique identifier of the remote cluster.
	ClusterID string `json:"clusterID"`
	// PublicKey is the base64 WireGuard public key of the remote peer.
	PublicKey string `json:"publicKey"`
	// Endpoint is the reachable host:port of the remote relay/gateway.
	Endpoint string `json:"endpoint"`
	// PodCIDRs are the pod network ranges served by the remote cluster.
	PodCIDRs []string `json:"podCIDRs"`
	// ServiceCIDRs are the service ranges served by the remote cluster.
	ServiceCIDRs []string `json:"serviceCIDRs"`
}

// AllowedIPs returns the union of pod and service CIDRs, which is the set of
// destinations that should be routed into the WireGuard device for this peer.
func (r RemotePeerConfig) AllowedIPs() []string {
	out := make([]string, 0, len(r.PodCIDRs)+len(r.ServiceCIDRs))
	out = append(out, r.PodCIDRs...)
	out = append(out, r.ServiceCIDRs...)
	return out
}

// ControlPlaneClient is the abstraction implemented by every topology source.
//
// Authenticate establishes or verifies the credential context required to
// talk to the control plane. For the local tier this is a no-op.
// For the enterprise tier it validates the SSO token.
//
// FetchTopology returns the full set of remote peers the local node should be
// connected to. The reconciler treats the returned slice as advisory input
// that is ultimately reconciled against actual kernel state.
type ControlPlaneClient interface {
	Authenticate(ctx context.Context) error
	FetchTopology(ctx context.Context) ([]RemotePeerConfig, error)
}

// Free / Open tier: LocalGitOpsClient

// LocalGitOpsClient implements ControlPlaneClient by reading MeshPeer custom
// resources out of the local cluster using a standard cached controller-runtime
// client. This is the fully decoupled, self-hosted path: peers are declared by
// the operator's own GitOps pipeline as CRDs and require no external service.
type LocalGitOpsClient struct {
	// Reader is any cached/uncached controller-runtime reader. In production
	// this is the manager's cache-backed client so reads are cheap.
	Reader client.Reader

	// Namespace optionally restricts the listing. MeshPeer is cluster-scoped,
	// so this is normally empty; it exists to support namespaced test fixtures
	// and future multi-tenant layouts.
	Namespace string
}

// NewLocalGitOpsClient constructs a LocalGitOpsClient backed by the supplied
// reader.
func NewLocalGitOpsClient(reader client.Reader) *LocalGitOpsClient {
	return &LocalGitOpsClient{Reader: reader}
}

// Authenticate is intentionally a no-op for the free tier. Access is governed
// entirely by the pod ServiceAccount's RBAC bindings as enforced by the API
// server; there is no separate credential to acquire. We still validate that a
// reader was wired in so misconfiguration fails fast rather than at first use.
func (c *LocalGitOpsClient) Authenticate(ctx context.Context) error {
	if c.Reader == nil {
		return fmt.Errorf("local gitops client: nil kubernetes reader; cannot authenticate against local cluster")
	}
	// Honor caller cancellation even on this cheap path.
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// FetchTopology lists every MeshPeer in the local cluster and projects each one
// into a transport-neutral RemotePeerConfig.
func (c *LocalGitOpsClient) FetchTopology(ctx context.Context) ([]RemotePeerConfig, error) {
	if c.Reader == nil {
		return nil, fmt.Errorf("local gitops client: nil kubernetes reader")
	}

	var list networkingv1alpha1.MeshPeerList
	opts := []client.ListOption{}
	if c.Namespace != "" {
		opts = append(opts, client.InNamespace(c.Namespace))
	}
	if err := c.Reader.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("local gitops client: listing MeshPeers: %w", err)
	}

	out := make([]RemotePeerConfig, 0, len(list.Items))
	for i := range list.Items {
		mp := &list.Items[i]
		// Skip objects that are mid-deletion; the reconciler tears those down
		// individually and they should not be re-advertised as live topology.
		if mp.GetDeletionTimestamp() != nil {
			continue
		}
		out = append(out, RemotePeerConfig{
			ClusterID:    mp.Spec.ClusterID,
			PublicKey:    mp.Spec.PublicKey,
			Endpoint:     mp.Spec.Endpoint,
			PodCIDRs:     append([]string(nil), mp.Spec.PodCIDRs...),
			ServiceCIDRs: append([]string(nil), mp.Spec.ServiceCIDRs...),
		})
	}
	return out, nil
}

// Ensure interface conformance at compile time.
var _ ControlPlaneClient = (*LocalGitOpsClient)(nil)

// Premium/Enterprise tier: EnterpriseControlPlaneClient

// EnterpriseControlPlaneClient implements ControlPlaneClient against a
// centralized managed SaaS control plane over a REST API. It authenticates with
// a machine SSO token sourced from DataWerx_ENTERPRISE_SSO_TOKEN. In a
// real deployment is minted by an external OIDC flow and retrieves a globally
// reconciled network topology map.
//
// The REST API surface is intentionally minimal and stable so that it can
// later be reavamped with gRPC without disturbing callers.
type EnterpriseControlPlaneClient struct {
	// Endpoint is the base URL of the managed control plane,
	// e.g. https://mesh.datawerx.io. Required.
	Endpoint string

	// HTTPClient is the transport used for all requests. If nil a sane default
	// with a bounded timeout is installed.
	HTTPClient *http.Client

	// tokenLoader resolves the bearer token. It is a field (rather than a hard
	// os.Getenv call) so tests can inject a deterministic value and so future
	// token sources (file, projected SA token, IMDS) can be slotted in without
	// changing the auth flow.
	tokenLoader func() string

	// token caches the credential resolved during Authenticate so that
	// FetchTopology does not re-read the environment on every reconcile.
	token string

	// retry governs transient-failure retries for every request.
	retry RetryPolicy

	// rng sources retry jitter. Held on the client (rather than the global
	// source) so it is race-free under concurrent clients and so tests can make
	// jitter deterministic. nil disables jitter (used in tests).
	rng *rand.Rand

	// sleep waits out a backoff interval. Injectable so tests need not spend
	// real wall-clock time; defaults to a context-aware sleep.
	sleep func(ctx context.Context, d time.Duration) error
}

// EnterpriseClientOption mutates an EnterpriseControlPlaneClient during
// construction, following the functional-options pattern.
type EnterpriseClientOption func(*EnterpriseControlPlaneClient)

// WithHTTPClient overrides the default HTTP transport.
func WithHTTPClient(h *http.Client) EnterpriseClientOption {
	return func(c *EnterpriseControlPlaneClient) { c.HTTPClient = h }
}

// WithTokenLoader overrides how the SSO token is resolved. Primarily for tests.
func WithTokenLoader(loader func() string) EnterpriseClientOption {
	return func(c *EnterpriseControlPlaneClient) { c.tokenLoader = loader }
}

// WithRetryPolicy overrides the transient-failure retry policy.
func WithRetryPolicy(p RetryPolicy) EnterpriseClientOption {
	return func(c *EnterpriseControlPlaneClient) { c.retry = p }
}

// withSleeper overrides the backoff sleeper. Unexported: tests in this package
// use it to avoid real delays; production always uses the context-aware sleep.
func withSleeper(s func(ctx context.Context, d time.Duration) error) EnterpriseClientOption {
	return func(c *EnterpriseControlPlaneClient) { c.sleep = s }
}

// withoutJitter disables retry jitter so a test can assert exact backoff
// scheduling. Unexported by design.
func withoutJitter() EnterpriseClientOption {
	return func(c *EnterpriseControlPlaneClient) { c.rng = nil }
}

// NewEnterpriseControlPlaneClient builds a premium control-plane client
// for the given endpoint. By default the SSO token is read from the
// DataWerx_ENTERPRISE_SSO_TOKEN environment variable.
func NewEnterpriseControlPlaneClient(endpoint string, opts ...EnterpriseClientOption) *EnterpriseControlPlaneClient {
	c := &EnterpriseControlPlaneClient{
		Endpoint: strings.TrimRight(endpoint, "/"),
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		tokenLoader: func() string { return os.Getenv(EnterpriseSSOTokenEnv) },
		retry:       DefaultRetryPolicy,
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
		sleep:       sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	return c
}

// Authenticate resolves and validates the enterprise SSO machine token.
//
// In a production build this is where the OIDC client-credentials exchange or
// SPIFFE/workload-identity attestation would be performed against the IdP. The
// scaffold here models the end state: a non-empty bearer token must be present
// and the control plane must accept it. We perform a lightweight authenticated
// probe so that an invalid/expired token surfaces at startup rather than during
// the first reconcile.
func (c *EnterpriseControlPlaneClient) Authenticate(ctx context.Context) error {
	if c.Endpoint == "" {
		return fmt.Errorf("enterprise control plane: empty endpoint; set DataWerx_SAAS_ENDPOINT")
	}

	token := strings.TrimSpace(c.tokenLoader())
	if token == "" {
		return fmt.Errorf("enterprise control plane: %s is unset; an OIDC/SSO machine token is required for the premium tier", EnterpriseSSOTokenEnv)
	}
	c.token = token

	// Probe the control plane's auth endpoint to fail fast on bad credentials.
	resp, err := c.authorizedGet(ctx, "/api/v1/auth/whoami")
	if err != nil {
		return fmt.Errorf("enterprise control plane: auth probe: %w", err)
	}
	defer drainAndClose(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("enterprise control plane: SSO token rejected (HTTP %d); refresh the OIDC credential", resp.StatusCode)
	default:
		return fmt.Errorf("enterprise control plane: unexpected auth probe status HTTP %d", resp.StatusCode)
	}
}

// FetchTopology retrieves the centralized topology map and decodes it into the
// shared RemotePeerConfig shape consumed by the reconciler.
func (c *EnterpriseControlPlaneClient) FetchTopology(ctx context.Context) ([]RemotePeerConfig, error) {
	peers, _, err := c.FetchTopologyWithRevision(ctx)
	return peers, err
}

// FetchTopologyWithRevision is the revision-aware fetch. It returns the opaque
// revision string the control plane stamps on the topology map alongside the
// peers, letting a caller short-circuit work when the topology is unchanged.
// The plain FetchTopology delegates here and discards the revision, so the
// ControlPlaneClient contract is unchanged.
func (c *EnterpriseControlPlaneClient) FetchTopologyWithRevision(ctx context.Context) ([]RemotePeerConfig, string, error) {
	if c.token == "" {
		// Defensive flow allows FetchTopology to lazily authenticate if the caller
		// skipped the explicit Authenticate step.  A transient restart that
		// lost cached state still recovers.
		if err := c.Authenticate(ctx); err != nil {
			return nil, "", err
		}
	}

	resp, err := c.authorizedGet(ctx, "/api/v1/topology")
	if err != nil {
		return nil, "", fmt.Errorf("enterprise control plane: topology fetch: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("enterprise control plane: topology fetch returned HTTP %d", resp.StatusCode)
	}

	// The control plane returns an envelope so it can attach pagination and
	// revision metadata later without breaking older agents.
	var envelope struct {
		Revision string             `json:"revision"`
		Peers    []RemotePeerConfig `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, "", fmt.Errorf("enterprise control plane: decoding topology payload: %w", err)
	}
	return envelope.Peers, envelope.Revision, nil
}

// RevisionedControlPlane is the optional interface a control-plane client may
// satisfy to expose the topology revision. The syncer type-asserts for it to
// enable change-detection; clients that don't implement it simply always sync.
type RevisionedControlPlane interface {
	FetchTopologyWithRevision(ctx context.Context) ([]RemotePeerConfig, string, error)
}

var _ RevisionedControlPlane = (*EnterpriseControlPlaneClient)(nil)

// authorizedGet performs a GET against the control plane with two layers of
// resilience:
//
//   - Transient failures (HTTP 429/5xx) or transport errors are retried with
//     capped backoff per the client's RetryPolicy.
//   - HTTP 401/403 triggers a single token refresh re-reading the credential via
//     the token loader, modeling an OIDC re-issue with one more attempt.  A
//     token that rotates mid-run recovers without a process restart.
//
// When err is nil the returned response still has its body open; the caller
// owns draining/closing it. A nil-error non-2xx response is returned verbatim so
// the caller can apply endpoint-specific status semantics.
func (c *EnterpriseControlPlaneClient) authorizedGet(ctx context.Context, path string) (*http.Response, error) {
	url := c.Endpoint + path
	var refreshed bool
	var lastErr error

	for attempt := 0; attempt < c.retry.attempts(); attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, fullJitter(c.retry.backoff(attempt-1), c.rng)); err != nil {
				return nil, err
			}
		}

		resp, action, err := c.getAttempt(ctx, url, path, &refreshed)
		switch action {
		case actionReturn:
			if err != nil {
				return nil, err // fatal (e.g. request could not be built)
			}
			return resp, nil
		case actionRefresh:
			attempt-- // don't count the refresh retry against the budget
		default: // actionRetry
			lastErr = err
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("exhausted %d attempts", c.retry.attempts())
	}
	return nil, fmt.Errorf("after %d attempts: %w", c.retry.attempts(), lastErr)
}

// responseAction tells authorizedGet how to handle the outcome of one attempt.
type responseAction int

const (
	actionReturn  responseAction = iota // success, or a fatal error: stop looping
	actionRefresh                       // auth failure: refresh token; retry
	actionRetry                         // transient failure: retry with backoff
)

// getAttempt performs one GET and classifies the result. It returns a non-nil
// resp only on the terminal success path. A non-nil err with actionReturn is
// fatal; with actionRetry it is the (retryable) lastErr.
func (c *EnterpriseControlPlaneClient) getAttempt(ctx context.Context, url, path string, refreshed *bool) (*http.Response, responseAction, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, actionReturn, fmt.Errorf("building request for %s: %w", path, err)
	}
	c.decorate(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, actionRetry, fmt.Errorf("transport error: %w", err) // retryable
	}

	switch c.classifyResponse(resp, refreshed) {
	case actionRefresh:
		return nil, actionRefresh, nil
	case actionRetry:
		return nil, actionRetry, fmt.Errorf("retryable HTTP %d", resp.StatusCode)
	default:
		return resp, actionReturn, nil
	}
}

// classifyResponse decides how to handle a response, performing the one-shot
// token refresh side effect on a first auth failure and draining the body on
// the non-terminal paths. The success path leaves the body open for the caller.
func (c *EnterpriseControlPlaneClient) classifyResponse(resp *http.Response, refreshed *bool) responseAction {
	// On an auth failure, refresh the token once and retry immediately without
	// consuming a backoff step, since this is not a load problem.
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && !*refreshed {
		drainAndClose(resp)
		*refreshed = true
		if tok := strings.TrimSpace(c.tokenLoader()); tok != "" {
			c.token = tok
		}
		return actionRefresh
	}
	if isRetryableStatus(resp.StatusCode) {
		drainAndClose(resp)
		return actionRetry
	}
	return actionReturn
}

// decorate stamps the shared authorization and content headers onto a request.
func (c *EnterpriseControlPlaneClient) decorate(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "datawerx-mesh-agent")
}

// drainAndClose fully drains and closes a response body so the underlying
// connection can be reused by the HTTP transport's keep-alive pool.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	// Best-effort drain; errors here are not actionable.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// Ensure interface conformance at compile time.
var _ ControlPlaneClient = (*EnterpriseControlPlaneClient)(nil)

// Package agent is the DataWerx Mesh node agent bootstrap.
//
// It runs as a DaemonSet: one pod per node, each owning that node's local
// `dwx-mesh0` WireGuard device. On startup it:
//
//  1. selects a control-plane client based on configuration — the premium
//     EnterpriseControlPlaneClient when DataWerx_SAAS_ENDPOINT is set, else the
//     free self-hosted LocalGitOpsClient;
//  2. verifies credentials against that control plane (token verification);
//  3. brings up the WireGuard data plane (SyncInterface on dwx-mesh0);
//  4. registers the MeshPeerReconciler with a controller-runtime manager; and
//  5. blocks in mgr.Start under standard OS-signal trapping.
//
// The open-core operator (cmd/manager) and the premium operator both call Run;
// the only difference is whether Options.RegisterPremium is set. This is the
// single seam through which commercial operator-side components are wired, so
// the open-core build never imports any of them.
package agent

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/DataWerx/datawerx-mesh/internal/meshstate"
	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	dwxclient "github.com/DataWerx/datawerx-mesh/pkg/client"
	"github.com/DataWerx/datawerx-mesh/pkg/controllers"
	"github.com/DataWerx/datawerx-mesh/pkg/dataplane/ebpf"
	dwxdns "github.com/DataWerx/datawerx-mesh/pkg/dns"
	"github.com/DataWerx/datawerx-mesh/pkg/dnsserver"
	"github.com/DataWerx/datawerx-mesh/pkg/evidence"
	"github.com/DataWerx/datawerx-mesh/pkg/gateway"
	"github.com/DataWerx/datawerx-mesh/pkg/logging"
	"github.com/DataWerx/datawerx-mesh/pkg/meshfw"
	dwxmetrics "github.com/DataWerx/datawerx-mesh/pkg/metrics"
	"github.com/DataWerx/datawerx-mesh/pkg/mtu"
	"github.com/DataWerx/datawerx-mesh/pkg/nat"
	"github.com/DataWerx/datawerx-mesh/pkg/probe"
	"github.com/DataWerx/datawerx-mesh/pkg/routed"
	"github.com/DataWerx/datawerx-mesh/pkg/syncer"
	"github.com/DataWerx/datawerx-mesh/pkg/topology"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
	"github.com/DataWerx/datawerx-mesh/pkg/wg"
)

// Environment variables that configure the agent.
const (
	// envSaaSEndpoint, when present and non-empty, switches the agent into the
	// premium tier and points it at the managed control plane.
	envSaaSEndpoint = "DataWerx_SAAS_ENDPOINT"

	// envPrivateKey supplies the node's WireGuard private key (base64). It is
	// normally projected from a Kubernetes Secret. If unset a key is generated
	// for this process lifetime (useful for dev; ephemeral across restarts).
	envPrivateKey = "DataWerx_WG_PRIVATE_KEY"

	// envLocalCIDRs is a comma-separated list of this cluster's own pod and
	// service ranges, used to detect overlap with remote peers.
	envLocalCIDRs = "DataWerx_LOCAL_CIDRS"

	// envInterface overrides the managed link name (default dwx-mesh0).
	envInterface = "DataWerx_WG_INTERFACE"

	// envDataPlane selects the peer data plane: "wireguard" (default — DataWerx
	// owns a WireGuard device) or "routed" (bring-your-own-overlay: an existing
	// overlay provides connectivity and DataWerx only programs host routes).
	envDataPlane = "DataWerx_DATAPLANE"

	// envOverlayInterface is the existing overlay device used as the route output
	// device in routed mode (e.g. tailscale0, wt0, wg0). Empty lets the kernel
	// resolve the device from the next-hop.
	envOverlayInterface = "DataWerx_OVERLAY_INTERFACE"

	// envWGListenPort overrides the WireGuard UDP listen port (default 51820).
	envWGListenPort = "DataWerx_WG_LISTEN_PORT"

	// envWGKeepalive overrides the persistent-keepalive interval for NAT
	// traversal (Go duration, e.g. "25s"; default 25s).
	envWGKeepalive = "DataWerx_WG_KEEPALIVE"

	// envWGMTU overrides the mesh device MTU. Unset/<=0 keeps the kernel default
	// (1420). TCP is additionally protected by the MSS clamp.
	envWGMTU = "DataWerx_WG_MTU"

	// envMSSClampDisable turns off the cross-cluster TCP MSS clamp (on by default
	// in WireGuard mode). Set truthy only if your environment clamps elsewhere.
	envMSSClampDisable = "DataWerx_MESH_MSS_CLAMP_DISABLE"

	// envSyncInterval tunes the enterprise topology poll cadence.
	envSyncInterval = "DataWerx_SYNC_INTERVAL"

	// envClusterID identifies this cluster in the mesh. It stamps exported
	// services so remote clusters can attribute and de-duplicate contributions.
	envClusterID = "DataWerx_CLUSTER_ID"

	// envClusterSetCIDR overrides the IPv4 range virtual ClusterSetIPs are
	// allocated from (default 241.0.0.0/8).
	envClusterSetCIDR = "DataWerx_CLUSTERSET_CIDR"

	// envClusterSetCIDR6, when set, is the IPv6 range a second ClusterSetIP is
	// allocated from for dual-stack services (services with IPv6 backends).
	// Unset disables IPv6 ClusterSetIP allocation.
	envClusterSetCIDR6 = "DataWerx_CLUSTERSET_CIDR6"

	// envDNSBind overrides the listen address of the clusterset.local DNS
	// responder (default :5353). Cluster CoreDNS forwards the zone here.
	envDNSBind = "DataWerx_DNS_BIND"

	// envRemapCIDR, when set, enables basic overlapping-CIDR remap: conflicting
	// remote ranges are routed under deterministic virtual ranges carved from
	// this pool (default pool 172.16.0.0/12 when set to "true"/"default") and
	// 1:1 NETMAP-translated. Unset keeps the safe refuse-and-Error behavior.
	envRemapCIDR = "DataWerx_REMAP_CIDR"

	// envRemapBackend selects the data plane that programs the remap: "iptables"
	// (open-core NETMAP, the default) or "ebpf" (premium TC/eBPF datapath, only
	// in a build compiled with -tags ebpf_datapath).
	envRemapBackend = "DataWerx_REMAP_BACKEND"

	// envLBFailover enables health-gated ClusterSetIP load-balancing: backends
	// exported by a meshed cluster whose tunnel is observably down (stale probe
	// or handshake) are dropped from the DNAT set, so traffic fails over to the
	// remaining exporters. Off by default — liveness is reported but not
	// enforced. Best paired with DataWerx_PROBE_ENABLE, whose active probe is a
	// traffic-independent liveness signal (a handshake alone goes stale on an
	// idle but healthy tunnel).
	envLBFailover = "DataWerx_LB_FAILOVER"

	// envRole selects an optional extra role for this agent. "gateway" turns the
	// node into a remote-access gateway: it programs a masquerade so traffic from
	// remote clients (laptops on a shared overlay) returns via this node, and
	// publishes an access-profile ConfigMap a thin client consumes. Unset keeps
	// the plain node-agent behavior.
	envRole = "DataWerx_ROLE"

	// envGatewayClientCIDRs is the comma-separated set of overlay source ranges
	// remote clients connect from (e.g. the Tailscale/CGNAT or corporate-VPN
	// range). Required when DataWerx_ROLE=gateway; it scopes the masquerade.
	envGatewayClientCIDRs = "DataWerx_GATEWAY_CLIENT_CIDRS"

	// envGatewayAdvertiseIPs is the comma-separated set of overlay-reachable
	// addresses of this gateway, advertised to clients in the access profile.
	envGatewayAdvertiseIPs = "DataWerx_GATEWAY_ADVERTISE_IPS"

	// envGatewayDNSAddr is the host:port of the clusterset.local responder a
	// client should use for split-DNS, advertised in the access profile.
	envGatewayDNSAddr = "DataWerx_GATEWAY_DNS_ADDR"

	// envGatewayProfileNamespace overrides the namespace the access-profile
	// ConfigMap is published in (default datawerx-system).
	envGatewayProfileNamespace = "DataWerx_GATEWAY_PROFILE_NAMESPACE"

	// envGatewayNoNAT, when true, disables the client masquerade so pods see the
	// remote client's real source IP (identity-preserving). Requires the premium
	// per-node return-route component (the premium operator) or client-bound
	// replies will black-hole.
	envGatewayNoNAT = "DataWerx_GATEWAY_NO_NAT"

	// envProbeEnable turns on the active synthetic prober and its responder. Off
	// by default; when set, every node serves a probe responder and dials its
	// connected peers' responders to observe true application-layer reachability.
	envProbeEnable = "DataWerx_PROBE_ENABLE"
	// envProbeResponderAddr is the responder's listen address (host:port).
	envProbeResponderAddr = "DataWerx_PROBE_RESPONDER_ADDR"
	// envProbePort is the port the prober dials on each peer's endpoint host. It
	// must match the port peers expose their responder on.
	envProbePort = "DataWerx_PROBE_PORT"
	// envProbeInterval is the probe cadence.
	envProbeInterval = "DataWerx_PROBE_INTERVAL"
	// envProbeTimeout caps a single probe dial.
	envProbeTimeout = "DataWerx_PROBE_TIMEOUT"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	// Register the built-in Kubernetes types (events, leases, ...) and our own
	// networking + multi-cluster API groups with the shared scheme.
	utilRuntimeMust(clientgoscheme.AddToScheme(scheme))
	utilRuntimeMust(networkingv1alpha1.AddToScheme(scheme))
	utilRuntimeMust(mcsv1alpha1.AddToScheme(scheme))
}

// Options configures a Run. The open-core operator passes the zero value; the
// premium operator (in its own repo) sets RegisterPremium.
type Options struct {
	// RegisterPremium, when non-nil, wires commercial operator-side components
	// into the manager. The open-core build leaves it nil, so the default binary
	// imports nothing premium.
	RegisterPremium func(mgr ctrl.Manager) error
}

// Run boots the agent: it parses flags, selects and authenticates the
// control-plane client, brings up the data plane, registers every reconciler and
// server, optionally wires premium components via opts.RegisterPremium, and
// blocks in the manager loop until signalled.
func Run(opts Options) error {
	var (
		metricsAddr string
		probeAddr   string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	// Logging: bind the --zap-* flags and let DataWerx_LOG_* env vars supply
	// container-friendly defaults underneath them (see pkg/logging).
	logOpts := logging.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(logOpts.Build())

	// The canonical first line: stamp build + runtime identity so any captured
	// log bundle self-identifies which agent produced it.
	logging.LogStartup(setupLog)

	// Root context cancelled on SIGINT/SIGTERM.
	ctx := ctrl.SetupSignalHandler()

	// ------------------------------------------------------------------
	// 1. Select and authenticate the control-plane client.
	// ------------------------------------------------------------------
	mgr, err := buildManager(metricsAddr, probeAddr)
	if err != nil {
		return err
	}

	clusterID := os.Getenv(envClusterID)
	if clusterID == "" {
		setupLog.Info("no " + envClusterID + " set; exported services will not carry a cluster ID")
	}

	cpClient, topoSyncer := selectControlPlane(mgr.GetClient(), clusterID)

	// Verify credentials up-front so misconfiguration fails fast at boot rather
	// than on the first reconcile.
	if err := authenticate(ctx, cpClient, topoSyncer != nil); err != nil {
		return err
	}

	// ------------------------------------------------------------------
	// 2. Bring up the peer data plane.
	//
	// DataWerx can either own a WireGuard device itself (the default), or — in
	// "routed" mode — assume an existing overlay (Tailscale, NetBird, Cilium,
	// plain WireGuard, a cloud VPN) already provides node-to-node connectivity
	// and only program host routes on top of it. Either backend satisfies
	// controllers.PeerDataPlane, so the reconciler is identical for both.
	// ------------------------------------------------------------------
	peerDP, dpName, ingressIface, closeDP, err := selectPeerDataPlane()
	if err != nil {
		return err
	}
	defer func() { _ = closeDP() }()
	setupLog.Info("peer data plane ready", "mode", dpName)

	// ------------------------------------------------------------------
	// 3. Register the reconcilers.
	// ------------------------------------------------------------------
	cfg := appConfig{
		clusterID:  clusterID,
		localCIDRs: splitCSV(os.Getenv(envLocalCIDRs)),
		// Overlap remap: enabled when DataWerx_REMAP_CIDR is set. "true"/"default"
		// selects the standard pool; any CIDR overrides it.
		remapPool:    resolveRemapPool(os.Getenv(envRemapCIDR)),
		ingressIface: ingressIface,
		peerDP:       peerDP,
	}

	// The iptables NAT manager is shared by the ClusterSetIP/masquerade/remap
	// reconcilers and (when enabled) the remote-access gateway, so build it once.
	natManager, err := nat.NewManager(ctrl.Log)
	if err != nil {
		return fmt.Errorf("initializing ClusterSetIP NAT manager: %w", err)
	}

	if err := registerCoreReconcilers(mgr, cfg); err != nil {
		return err
	}
	if err := registerNATReconcilers(mgr, cfg, natManager); err != nil {
		return err
	}
	if err := registerFirewallReconciler(mgr, ingressIface); err != nil {
		return err
	}
	if err := registerGatewayRole(mgr, cfg, natManager); err != nil {
		return err
	}
	if err := registerMTUClamp(mgr, dpName, ingressIface); err != nil {
		return err
	}

	if err := registerServers(mgr, topoSyncer); err != nil {
		return err
	}
	if err := registerProbing(mgr, cfg); err != nil {
		return err
	}
	if err := registerEvidenceReporter(mgr, cpClient, topoSyncer != nil); err != nil {
		return err
	}

	// Commercial operator-side components. Nil in the open-core build, so the
	// default operator wires nothing premium; the premium operator injects them.
	if opts.RegisterPremium != nil {
		if err := opts.RegisterPremium(mgr); err != nil {
			return err
		}
	}

	// ------------------------------------------------------------------
	// 4. Block in the manager loop until signalled.
	// ------------------------------------------------------------------
	setupLog.Info("starting manager", "dataplane", dpName)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}
	return nil
}

// appConfig carries the already-resolved inputs the reconcilers share so the
// registration helpers have a small, uniform signature.
type appConfig struct {
	clusterID    string
	localCIDRs   []string
	remapPool    string
	ingressIface string
	peerDP       controllers.PeerDataPlane
}

// buildManager loads the in-cluster REST config and constructs the controller
// manager. The manager owns the cached client used for both reads and writes;
// we build it first because the free-tier control-plane client reads MeshPeer
// CRDs through the manager's cache.
func buildManager(metricsAddr, probeAddr string) (ctrl.Manager, error) {
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubernetes rest config: %w", err)
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		// DaemonSet agents each manage their own node's kernel state, so the
		// reconciler must run on every node — leader election is therefore OFF.
		LeaderElection: false,
	})
	if err != nil {
		return nil, fmt.Errorf("creating controller manager: %w", err)
	}
	return mgr, nil
}

// authenticate verifies control-plane credentials up-front, under a bounded
// context, so misconfiguration fails fast at boot rather than on first reconcile.
func authenticate(ctx context.Context, cpClient dwxclient.ControlPlaneClient, premium bool) error {
	authCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := cpClient.Authenticate(authCtx); err != nil {
		return fmt.Errorf("control-plane authentication failed: %w", err)
	}
	setupLog.Info("control-plane authenticated", "premium", premium)
	return nil
}

// registerCoreReconcilers wires up the MeshPeer reconciler and the MCS
// ServiceExport/ServiceImport reconcilers.
func registerCoreReconcilers(mgr ctrl.Manager, cfg appConfig) error {
	reconciler := &controllers.MeshPeerReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		DataPlane:  cfg.peerDP,
		LocalCIDRs: cfg.localCIDRs,
		ClusterID:  cfg.clusterID,
		RemapPool:  cfg.remapPool,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering MeshPeer controller: %w", err)
	}

	// Cross-cluster service discovery (MCS): validate ServiceExports against
	// their referenced Service and report readiness.
	exportReconciler := &controllers.ServiceExportReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		ClusterID: cfg.clusterID,
	}
	if err := exportReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering ServiceExport controller: %w", err)
	}

	importReconciler := &controllers.ServiceImportReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		ClusterSetCIDR:  getenvDefault(envClusterSetCIDR, controllers.DefaultClusterSetCIDR),
		ClusterSetCIDR6: getenvDefault(envClusterSetCIDR6, ""),
	}
	if err := importReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering ServiceImport controller: %w", err)
	}
	return nil
}

// registerNATReconcilers wires up the ClusterSetIP NAT reconciler and, when
// DataWerx_REMAP_CIDR is set, the overlapping-CIDR remap reconciler. The remap
// backend is pluggable behind controllers.RemapDataPlane: the open-core iptables
// NETMAP (nat.Manager) by default, or the premium TC/eBPF datapath when selected.
func registerNATReconcilers(mgr ctrl.Manager, cfg appConfig, natManager *nat.Manager) error {
	// ClusterSetIP DNAT/load-balancing: rewrite traffic destined for a
	// ServiceImport's virtual IP to the exporting clusters' real service IPs.
	natReconciler := &controllers.ServiceNATReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		DataPlane: natManager,
	}
	if envOn(os.Getenv(envLBFailover)) {
		natReconciler.FailoverStaleSeconds = controllers.DefaultFailoverStaleSeconds
		setupLog.Info("health-gated ClusterSetIP failover enabled",
			"staleSeconds", controllers.DefaultFailoverStaleSeconds)
	}
	if err := natReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering ServiceNAT controller: %w", err)
	}

	// Masquerade exemption: keep the node from SNAT-ing cross-cluster pod traffic
	// leaving the mesh device (which would break WireGuard's AllowedIPs check at
	// the peer). Always on — it is the core data-path requirement for real
	// cross-cluster connectivity, independent of overlap remap.
	masqReconciler := &controllers.MasqExemptReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		DataPlane:  natManager,
		LocalCIDRs: cfg.localCIDRs,
	}
	if err := masqReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering masquerade-exemption controller: %w", err)
	}

	if cfg.remapPool == "" {
		return nil
	}
	remapDP, err := selectRemapBackend(os.Getenv(envRemapBackend), natManager, cfg.ingressIface)
	if err != nil {
		return err
	}
	remapReconciler := &controllers.RemapReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		DataPlane:  remapDP,
		ClusterID:  cfg.clusterID,
		RemapPool:  cfg.remapPool,
		LocalCIDRs: cfg.localCIDRs,
	}
	if err := remapReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering Remap controller: %w", err)
	}
	setupLog.Info("overlapping-CIDR remap enabled", "pool", cfg.remapPool, "backend", remapBackendName(os.Getenv(envRemapBackend)))
	return nil
}

// registerFirewallReconciler compiles MeshNetworkPolicies into an iptables filter
// firewall on the mesh ingress interface. With no policies present the firewall
// is a no-op (all mesh traffic allowed), so it is safe to register; it activates
// only when a MeshNetworkPolicy is created.
//
// The firewall must hook a concrete interface (`-i <iface>`). In routed mode
// without DataWerx_OVERLAY_INTERFACE there is no such device (the kernel resolves
// the next-hop), so we skip it rather than crash the agent on an empty `-i`.
func registerFirewallReconciler(mgr ctrl.Manager, ingressIface string) error {
	if ingressIface == "" {
		setupLog.Info("mesh firewall (MeshNetworkPolicy) disabled: no mesh ingress interface; set DataWerx_OVERLAY_INTERFACE in routed mode to enable it")
		return nil
	}
	fwManager, err := meshfw.NewManager(ingressIface, ctrl.Log)
	if err != nil {
		return fmt.Errorf("initializing mesh firewall manager: %w", err)
	}
	policyReconciler := &controllers.MeshNetworkPolicyReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		DataPlane: fwManager,
	}
	if err := policyReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering MeshNetworkPolicy controller: %w", err)
	}
	return nil
}

// registerGatewayRole enables the remote-access gateway when DataWerx_ROLE is
// "gateway". The gateway lets a remote client (a laptop on a shared overlay)
// reach in-cluster ClusterSetIP VIPs and cross-cluster ranges like a VPN: it
// enables IP forwarding, programs a masquerade for client→mesh traffic (reusing
// the shared NAT manager), and publishes an access-profile ConfigMap a thin
// client consumes. With any other role this is a no-op, so it is always safe to
// call.
func registerGatewayRole(mgr ctrl.Manager, cfg appConfig, dp controllers.GatewayDataPlane) error {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envRole)), "gateway") {
		return nil
	}

	clientCIDRs := splitCSV(os.Getenv(envGatewayClientCIDRs))
	if len(clientCIDRs) == 0 {
		return fmt.Errorf("%s=gateway requires %s (the overlay source ranges remote clients connect from)", envRole, envGatewayClientCIDRs)
	}
	if err := validateCIDRs(envGatewayClientCIDRs, clientCIDRs); err != nil {
		return err
	}

	// Forwarding is the data-path prerequisite for routing client traffic into
	// the mesh. It is best-effort: if the pod lacks the privilege to set the
	// sysctl, log and continue so the misconfiguration is visible without
	// crash-looping the agent (the host may already have it enabled).
	if err := gateway.EnableIPForward(); err != nil {
		setupLog.Error(err, "enabling IP forwarding for gateway role; run the pod privileged or pre-set net.ipv4.ip_forward=1 on the gateway nodes, or the gateway will not route client traffic")
	}

	clusterSet := []string{getenvDefault(envClusterSetCIDR, controllers.DefaultClusterSetCIDR)}
	if v6 := strings.TrimSpace(os.Getenv(envClusterSetCIDR6)); v6 != "" {
		clusterSet = append(clusterSet, v6)
	}
	if err := validateCIDRs(envClusterSetCIDR, clusterSet); err != nil {
		return err
	}

	r := &controllers.GatewayReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		DataPlane:        dp,
		ClientCIDRs:      clientCIDRs,
		GatewayEndpoints: splitCSV(os.Getenv(envGatewayAdvertiseIPs)),
		ClusterSetCIDRs:  clusterSet,
		LocalCIDRs:       cfg.localCIDRs,
		DNS: gateway.DNSConfig{
			Addr:          strings.TrimSpace(os.Getenv(envGatewayDNSAddr)),
			SearchDomains: []string{dwxdns.ClusterSetDomain},
		},
		ProfileNamespace: getenvDefault(envGatewayProfileNamespace, gateway.DefaultProfileNamespace),
		NoNAT:            boolEnv(envGatewayNoNAT),
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering remote-access gateway controller: %w", err)
	}
	setupLog.Info("remote-access gateway role enabled",
		"clientCIDRs", clientCIDRs, "advertise", r.GatewayEndpoints, "profileNamespace", r.ProfileNamespace)
	return nil
}

// registerMTUClamp installs the cross-cluster TCP MSS-clamp ensurer when this
// agent owns a WireGuard device — the only mode where our encapsulation reduces
// the mesh MTU. In routed/BYO mode the existing overlay owns its own MTU, so we
// leave it alone. Disable with DataWerx_MESH_MSS_CLAMP_DISABLE.
func registerMTUClamp(mgr ctrl.Manager, dpName, ifaceName string) error {
	if !strings.HasPrefix(dpName, "wireguard") || ifaceName == "" {
		return nil
	}
	if boolEnv(envMSSClampDisable) {
		setupLog.Info("cross-cluster TCP MSS clamp disabled by " + envMSSClampDisable)
		return nil
	}
	m, err := mtu.NewManager(ctrl.Log)
	if err != nil {
		return fmt.Errorf("initializing MSS-clamp manager: %w", err)
	}
	if err := mgr.Add(&mtu.Ensurer{Iface: ifaceName, Plane: m, Log: ctrl.Log.WithName("mtu")}); err != nil {
		return fmt.Errorf("registering MSS-clamp ensurer: %w", err)
	}
	setupLog.Info("cross-cluster TCP MSS clamp enabled", "iface", ifaceName)
	return nil
}

// registerServers adds the non-reconciler runnables: the clusterset.local DNS
// server, DataWerx metrics, the premium topology syncer, and the health/ready
// probes for the DaemonSet.
func registerServers(mgr ctrl.Manager, topoSyncer *syncer.Syncer) error {
	// Serve the clusterset.local zone from ServiceImports/EndpointExports.
	// Cluster CoreDNS forwards that zone to this responder (see config/coredns).
	dnsSrv := &dnsserver.Server{
		Addr:     getenvDefault(envDNSBind, dnsserver.DefaultBindAddress),
		Resolver: &dnsserver.CachedResolver{Reader: mgr.GetClient()},
		Log:      ctrl.Log.WithName("dnsserver"),
	}
	if err := mgr.Add(dnsSrv); err != nil {
		return fmt.Errorf("registering clusterset.local DNS server: %w", err)
	}

	// Register DataWerx metrics on the controller-runtime registry served at the
	// manager's /metrics endpoint. Non-fatal: lost metrics must not stop the
	// data plane.
	if err := dwxmetrics.Register(mgr.GetClient()); err != nil {
		setupLog.Error(err, "registering DataWerx metrics")
	}

	// In the premium tier, run the topology syncer as a managed Runnable so its
	// lifecycle is tied to the manager's. It mirrors the centralized topology
	// into MeshPeer CRDs, which the (tier-agnostic) reconciler then programs.
	if topoSyncer != nil {
		if err := mgr.Add(topoSyncer); err != nil {
			return fmt.Errorf("registering enterprise topology syncer: %w", err)
		}
	}

	// Health and readiness probes for the DaemonSet.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding readyz check: %w", err)
	}
	return nil
}

// registerEvidenceReporter wires the premium-tier evidence reporter: a Runnable
// that periodically pushes this cluster's grounded evidence to the managed control
// plane for the "DataWerx Signal" fleet view. It is gated on the premium tier,
// which is the same condition that produced the topology syncer, and on
// the control-plane client actually supporting evidence push.  The open-core,
// self-hosted tier wires nothing and the reconcile loop is untouched. Snapshot
// gathering uses a dedicated read-only client (not the manager cache) exactly as
// the other read surfaces (dwxctl/dwx-mcp) do.
func registerEvidenceReporter(mgr ctrl.Manager, cpClient dwxclient.ControlPlaneClient, premium bool) error {
	if !premium {
		return nil
	}
	sink, ok := cpClient.(evidence.Sink)
	if !ok {
		// The premium control plane doesn't accept evidence (older endpoint); skip
		// rather than fail — evidence sync is additive and non-essential.
		setupLog.Info("evidence reporter not wired: control plane does not support evidence push")
		return nil
	}
	msClient, err := meshstate.NewClient("", "")
	if err != nil {
		return fmt.Errorf("evidence reporter: building kubernetes client: %w", err)
	}
	ns, ds := meshstate.DefaultNamespace, meshstate.DefaultDaemonSet
	reporter := &evidence.Reporter{
		Sink:     sink,
		Interval: resolveSyncInterval(),
		Log:      ctrl.Log.WithName("evidence-reporter"),
		Snapshot: func(ctx context.Context) (verify.Snapshot, error) {
			return meshstate.Snapshot(ctx, msClient, ns, ds)
		},
	}
	if err := mgr.Add(reporter); err != nil {
		return fmt.Errorf("registering evidence reporter: %w", err)
	}
	setupLog.Info("evidence reporter wired (premium tier)", "interval", reporter.Interval)
	return nil
}

// registerProbing wires the active synthetic prober and its responder when
// DataWerx_PROBE_ENABLE is set. Every node serves a tiny responder so remote
// clusters can prove application-layer reachability into this one, and dials its
// own connected peers' responders to observe whether the mesh is actually
// passing traffic. The observed signal feeds pkg/slo exactly as the WireGuard
// handshake does; see docs/active-probing.md. Off by default.
func registerProbing(mgr ctrl.Manager, cfg appConfig) error {
	if !boolEnv(envProbeEnable) {
		return nil
	}

	responder := &probe.Responder{
		Addr:      getenvDefault(envProbeResponderAddr, probe.DefaultResponderAddr),
		ClusterID: cfg.clusterID,
		Log:       ctrl.Log.WithName("probe-responder"),
	}
	if err := mgr.Add(responder); err != nil {
		return fmt.Errorf("registering mesh probe responder: %w", err)
	}

	port := probePort()
	prober := &probe.Prober{
		Interval: resolveDuration(envProbeInterval, probe.DefaultInterval),
		Timeout:  resolveDuration(envProbeTimeout, probe.DefaultTimeout),
		Peers:    peerLister(mgr.GetClient(), port),
		Publish:  probePublisher(mgr.GetClient(), ctrl.Log.WithName("prober")),
		Log:      ctrl.Log.WithName("prober"),
	}
	if err := mgr.Add(prober); err != nil {
		return fmt.Errorf("registering mesh prober: %w", err)
	}
	setupLog.Info("active mesh probing enabled", "responderAddr", responder.Addr, "peerProbePort", port)
	return nil
}

// probeStatusRefreshSeconds bounds how often a steady-state probe result is
// written back: a healthy peer's status is refreshed at most this often per
// node, well inside the stale window so the observed age never falsely ages out.
const probeStatusRefreshSeconds int64 = 60

// probePublisher records a cycle's probe results onto the matching MeshPeers'
// status, so the read surfaces reflect probe-observed liveness. It indexes peers
// by cluster ID, folds each result through probe.NextProbeStatus to suppress
// per-cycle write churn, and patches only the status subresource — the same
// fields the reconciler leaves untouched, so the two writers never collide.
func probePublisher(c client.Client, log logr.Logger) probe.Publisher {
	return func(ctx context.Context, results []probe.Result) error {
		var list networkingv1alpha1.MeshPeerList
		if err := c.List(ctx, &list); err != nil {
			return fmt.Errorf("listing MeshPeers to publish probe results: %w", err)
		}
		byCluster := make(map[string]*networkingv1alpha1.MeshPeer, len(list.Items))
		for i := range list.Items {
			byCluster[list.Items[i].Spec.ClusterID] = &list.Items[i]
		}

		for _, res := range results {
			mp, ok := byCluster[res.ClusterID]
			if !ok {
				continue
			}
			cur := probe.ProbeStatus{
				LastAttemptUnix: mp.Status.LastProbeAttempt,
				LastSuccessUnix: mp.Status.LastProbeTime,
			}
			next, changed := probe.NextProbeStatus(cur, res, probeStatusRefreshSeconds)
			if !changed {
				continue
			}
			base := mp.DeepCopy()
			mp.Status.LastProbeAttempt = next.LastAttemptUnix
			mp.Status.LastProbeTime = next.LastSuccessUnix
			if err := c.Status().Patch(ctx, mp, client.MergeFrom(base)); err != nil {
				log.Info("patching MeshPeer probe status failed", "clusterID", res.ClusterID, "err", err)
			}
		}
		return nil
	}
}

// peerLister reads the current MeshPeers and projects them into probe.Peer. A
// peer's responder is dialed at its endpoint host paired with the probe port:
// the endpoint host is the node's mesh-reachable address, and that node runs the
// responder. Only Connected peers carry an address, so unconnected and
// conflicting peers (which land in Error phase) are skipped by PlanTargets.
func peerLister(c client.Client, port string) probe.PeerLister {
	return func(ctx context.Context) ([]probe.Peer, error) {
		var list networkingv1alpha1.MeshPeerList
		if err := c.List(ctx, &list); err != nil {
			return nil, err
		}
		peers := make([]probe.Peer, 0, len(list.Items))
		for i := range list.Items {
			mp := &list.Items[i]
			connected := mp.Status.Phase == networkingv1alpha1.MeshPeerPhaseConnected
			peers = append(peers, probe.Peer{
				ClusterID:    mp.Spec.ClusterID,
				Connected:    connected,
				ProbeAddress: probeAddress(mp.Spec.Endpoint, port),
			})
		}
		return peers, nil
	}
}

// probeAddress pairs the host of a WireGuard endpoint with the probe port. An
// endpoint without a parseable host yields no address, so the peer is not probed.
func probeAddress(endpoint, port string) string {
	if endpoint == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" {
		return ""
	}
	return net.JoinHostPort(host, port)
}

// probePort is the port the prober dials on each peer, derived from the
// responder's configured address so a uniform deployment needs only one knob.
func probePort() string {
	if p := strings.TrimSpace(os.Getenv(envProbePort)); p != "" {
		return p
	}
	addr := getenvDefault(envProbeResponderAddr, probe.DefaultResponderAddr)
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "9998"
}

// resolveDuration reads a duration env var, falling back to def on absence or a
// parse error.
func resolveDuration(env string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		setupLog.Info("ignoring invalid duration; using default", "env", env, "value", v, "default", def.String())
		return def
	}
	return d
}

// selectControlPlane chooses the tier based on configuration. When
// DataWerx_SAAS_ENDPOINT is present it returns the premium client plus a
// *syncer.Syncer Runnable that mirrors remote topology into CRDs; otherwise it
// returns the free LocalGitOpsClient and a nil syncer.
func selectControlPlane(k8s client.Client, localClusterID string) (dwxclient.ControlPlaneClient, *syncer.Syncer) {
	if endpoint := strings.TrimSpace(os.Getenv(envSaaSEndpoint)); endpoint != "" {
		setupLog.Info("premium tier selected", "endpoint", endpoint)
		ent := dwxclient.NewEnterpriseControlPlaneClient(endpoint)
		return ent, &syncer.Syncer{
			CP:             ent,
			K8s:            k8s,
			Interval:       resolveSyncInterval(),
			Log:            ctrl.Log.WithName("topology-syncer"),
			LocalClusterID: localClusterID,
		}
	}
	setupLog.Info("free tier selected: self-hosted GitOps via local MeshPeer CRDs")
	return dwxclient.NewLocalGitOpsClient(k8s), nil
}

// resolvePrivateKey returns the node WireGuard private key, generating an
// ephemeral one if none is configured.
// selectPeerDataPlane builds the peer data plane chosen by DataWerx_DATAPLANE.
// It returns the data plane, a human-readable mode name, the network interface
// mesh ingress arrives on (the WireGuard device in standalone mode, or the
// overlay device in routed mode — used to scope the mesh firewall), a close
// func, and an error. Both backends satisfy controllers.PeerDataPlane, so the
// reconciler is unaffected by the choice.
func selectPeerDataPlane() (controllers.PeerDataPlane, string, string, func() error, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envDataPlane))) {
	case "", "wireguard", "wg":
		ifaceName := getenvDefault(envInterface, wg.DefaultInterfaceName)
		wgManager, err := wg.NewWireGuardManager(ifaceName, ctrl.Log,
			wg.WithListenPort(intEnv(envWGListenPort)),
			wg.WithKeepalive(durationEnv(envWGKeepalive)),
			wg.WithMTU(intEnv(envWGMTU)),
		)
		if err != nil {
			return nil, "", "", nil, fmt.Errorf("initializing wireguard manager: %w", err)
		}
		privKey, err := resolvePrivateKey()
		if err != nil {
			return nil, "", "", nil, err
		}
		if err := wgManager.SyncInterface(privKey); err != nil {
			return nil, "", "", nil, fmt.Errorf("syncing %s interface: %w", ifaceName, err)
		}
		return wgManager, "wireguard:" + ifaceName, ifaceName, wgManager.Close, nil

	case "routed", "overlay", "byo":
		overlayIface := strings.TrimSpace(os.Getenv(envOverlayInterface))
		rm, err := routed.NewManager(overlayIface, ctrl.Log)
		if err != nil {
			return nil, "", "", nil, fmt.Errorf("initializing routed data plane: %w", err)
		}
		name := "routed"
		if overlayIface != "" {
			name += ":" + overlayIface
			// Best-effort: surface this node's overlay address so operators know
			// what to set as spec.endpoint in remote clusters' MeshPeers.
			if ip, derr := routed.DiscoverOverlayIP(overlayIface); derr == nil {
				setupLog.Info("routed mode: this node's overlay address", "interface", overlayIface, "ip", ip,
					"hint", "set this as spec.endpoint in remote clusters' MeshPeers for this cluster")
			} else {
				setupLog.Info("routed mode: could not auto-discover overlay address", "interface", overlayIface, "err", derr.Error())
			}
		}
		return rm, name, overlayIface, rm.Close, nil

	default:
		return nil, "", "", nil, fmt.Errorf("unknown %s %q (want wireguard|routed)", envDataPlane, os.Getenv(envDataPlane))
	}
}

func resolvePrivateKey() (string, error) {
	if v := strings.TrimSpace(os.Getenv(envPrivateKey)); v != "" {
		return v, nil
	}
	setupLog.Info("no " + envPrivateKey + " set; generating an ephemeral private key (will not survive restart)")
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return "", fmt.Errorf("generating ephemeral wireguard key: %w", err)
	}
	return key.String(), nil
}

func resolveSyncInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv(envSyncInterval)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 30 * time.Second
}

func getenvDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// resolveRemapPool interprets DataWerx_REMAP_CIDR: empty/"false"/"off" disables
// overlap remap; "true"/"default" selects the standard pool; any other value is
// used as the pool CIDR.
func resolveRemapPool(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "", "false", "off", "0", "disabled":
		return ""
	case "true", "default", "on", "1", "enabled":
		return topology.DefaultRemapPool
	default:
		return v
	}
}

// envOn reports whether a boolean-style env value (e.g. DataWerx_LB_FAILOVER) is
// enabled. Anything other than an explicit truthy token is treated as off.
func envOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "enabled", "yes":
		return true
	default:
		return false
	}
}

// selectRemapBackend chooses the data plane that programs overlap remap. The
// default (and "iptables") uses the open-core NETMAP manager already built for
// ClusterSetIP NAT. "ebpf" selects the premium TC/eBPF datapath, which is only
// available when the agent is compiled with -tags ebpf_datapath; otherwise
// ebpf.Load returns a clear error so the misconfiguration fails fast at boot.
func selectRemapBackend(backend string, iptablesDP controllers.RemapDataPlane, iface string) (controllers.RemapDataPlane, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", "iptables", "nftables", "netmap":
		return iptablesDP, nil
	case "ebpf", "tc", "xdp":
		ops, err := ebpf.Load(iface, ctrl.Log)
		if err != nil {
			return nil, fmt.Errorf("selecting eBPF remap backend: %w", err)
		}
		return ebpf.NewManager(ops, ctrl.Log), nil
	default:
		return nil, fmt.Errorf("unknown %s %q (want iptables|ebpf)", envRemapBackend, backend)
	}
}

func remapBackendName(v string) string {
	if b := strings.ToLower(strings.TrimSpace(v)); b == "ebpf" || b == "tc" || b == "xdp" {
		return "ebpf"
	}
	return "iptables"
}

// validateCIDRs fails fast with a clear message when a configured CIDR list
// contains an unparseable entry, rather than letting malformed values reach the
// data plane (iptables/netlink) and fail obscurely at apply time.
func validateCIDRs(envName string, cidrs []string) error {
	for _, c := range cidrs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return fmt.Errorf("%s: %q is not a valid CIDR: %w", envName, c, err)
		}
	}
	return nil
}

// boolEnv reports whether an env var is set to a truthy value
// ("1"/"true"/"yes"/"on", case-insensitive).
func boolEnv(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

// intEnv parses an integer env var, returning 0 (meaning "use the default")
// when unset or invalid.
func intEnv(key string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		setupLog.Info("ignoring invalid integer env var", "key", key, "value", v)
		return 0
	}
	return n
}

// durationEnv parses a Go-duration env var, returning 0 (meaning "use the
// default") when unset or invalid.
func durationEnv(key string) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		setupLog.Info("ignoring invalid duration env var", "key", key, "value", v)
		return 0
	}
	return d
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// utilRuntimeMust panics on scheme-registration errors, which are programmer
// errors that should abort process startup.
func utilRuntimeMust(err error) {
	if err != nil {
		panic(fmt.Sprintf("scheme registration failed: %v", err))
	}
}

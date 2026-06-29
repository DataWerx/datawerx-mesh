package mesh

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/edge"
	"github.com/DataWerx/datawerx-mesh/pkg/gateway"
)

// runEdge implements the edge device connector's authoring + handoff surface:
//
//	dwx edge enroll    author an EdgeDevice for a device (the tier-agnostic contract)
//	dwx edge profile   render a device's wg-quick config / dwxedge.v1 token to hand off
//	dwx edge list      list enrolled EdgeDevices and their status
//
// `enroll` writes the free EdgeDevice CRD (or prints it with --dry-run). The
// device only carries traffic once an edge terminator is running — the premium
// managed connector, or the free BYO-overlay + gateway path (docs/byo-overlay.md).
func runEdge(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dwx edge <enroll|profile|list> [flags]")
		return 2
	}
	switch args[0] {
	case "enroll":
		return runEdgeEnroll(args[1:])
	case "profile":
		return runEdgeProfile(args[1:])
	case "list":
		return runEdgeList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown edge subcommand %q (want enroll|profile|list)\n", args[0])
		return 2
	}
}

// runEdgeEnroll authors an EdgeDevice. The device's public key is either supplied
// (its existing key) or freshly generated with --generate, in which case the
// private key is printed to stderr to be stored ON THE DEVICE. It is the one
// mutating edge command; --dry-run prints the object instead of applying it.
// Re-enrolling the same device ID is an idempotent upsert.
func runEdgeEnroll(args []string) int {
	fs := flag.NewFlagSet("edge enroll", flag.ExitOnError)
	deviceID := fs.String("device-id", "", "stable, human-facing device identity (required)")
	publicKey := fs.String("public-key", "", "the device's WireGuard public key (or use --generate)")
	generate := fs.Bool("generate", false, "generate a fresh device keypair (prints the private key to stderr)")
	address := fs.String("address", "", "optional /32 address pin (empty => deterministic allocation)")
	allowedServices := fs.String("allowed-services", "", "comma-separated clusterset service-name globs to scope reach")
	allowedCIDRs := fs.String("allowed-cidrs", "", "comma-separated extra raw destination ranges")
	identityPreserving := fs.Bool("identity-preserving", false, "pods see the device's real tunnel IP (needs the premium nonat component)")
	expiresAt := fs.String("expires-at", "", "optional RFC3339 instant past which the device is cut off")
	dryRun := fs.Bool("dry-run", false, "print the EdgeDevice that would be created, without applying it")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	_ = fs.Parse(args)

	if *deviceID == "" {
		fmt.Fprintln(os.Stderr, "error: --device-id is required")
		return 2
	}

	pub := *publicKey
	if *generate {
		kp, err := edge.GenerateKeypair()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		pub = kp.PublicKey
		fmt.Fprintf(os.Stderr, "Generated a new device keypair. Store this private key ON THE DEVICE (never upload it):\n  %s\n\n", kp.PrivateKey)
	}
	if pub == "" {
		fmt.Fprintln(os.Stderr, "error: provide --public-key or pass --generate")
		return 2
	}

	spec := networkingv1alpha1.EdgeDeviceSpec{
		DeviceID:           *deviceID,
		PublicKey:          pub,
		Address:            *address,
		AllowedServices:    splitCSV(*allowedServices),
		AllowedCIDRs:       splitCSV(*allowedCIDRs),
		IdentityPreserving: *identityPreserving,
	}
	if *expiresAt != "" {
		t, err := time.Parse(time.RFC3339, *expiresAt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: --expires-at %q is not RFC3339: %v\n", *expiresAt, err)
			return 2
		}
		mt := metav1.NewTime(t)
		spec.ExpiresAt = &mt
	}

	dev := edge.EdgeDeviceObject(spec)

	if *dryRun {
		out, err := yaml.Marshal(dev)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Print(string(out))
		return 0
	}

	c, err := newClient(*kubeconfig, *kubecontext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := applyEdgeDevice(ctx, c, dev); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("Enrolled EdgeDevice %q (device %q).\n", dev.Name, *deviceID)
	fmt.Fprintln(os.Stderr, "# Render the device config with: dwx edge profile --device-id "+*deviceID+" --endpoint <host:port> --peer-public-key <terminator-pubkey> --route-cidrs <cidrs>")
	return 0
}

// runEdgeProfile renders a device's wg-quick config (or dwxedge.v1 token) for
// handoff. The device's address comes from the EdgeDevice status (assigned by the
// terminator) unless --address overrides it; the terminator endpoint/public key
// and the reachable routes are operator-supplied.
func runEdgeProfile(args []string) int {
	fs := flag.NewFlagSet("edge profile", flag.ExitOnError)
	deviceID := fs.String("device-id", "", "the enrolled device's ID (required)")
	endpoint := fs.String("endpoint", "", "the edge terminator's public host:port the device dials (required)")
	peerPublicKey := fs.String("peer-public-key", "", "the edge terminator's WireGuard public key (required)")
	routeCIDRs := fs.String("route-cidrs", "", "comma-separated clusterset/mesh ranges the device routes through the tunnel")
	dnsAddr := fs.String("dns", "", "clusterset DNS responder host:port for split-DNS")
	dnsSearch := fs.String("dns-search", "clusterset.local", "comma-separated DNS search domains")
	address := fs.String("address", "", "override the device address (default: read from EdgeDevice status)")
	privateKey := fs.String("private-key", "", "device private key to embed (default: a placeholder)")
	asToken := fs.Bool("token", false, "print the dwxedge.v1 token instead of a wg-quick config")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	_ = fs.Parse(args)

	if *deviceID == "" || *endpoint == "" || *peerPublicKey == "" {
		fmt.Fprintln(os.Stderr, "error: --device-id, --endpoint and --peer-public-key are required")
		return 2
	}

	c, err := newClient(*kubeconfig, *kubecontext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var dev networkingv1alpha1.EdgeDevice
	name := edge.EdgeDeviceObject(networkingv1alpha1.EdgeDeviceSpec{DeviceID: *deviceID}).Name
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &dev); err != nil {
		fmt.Fprintf(os.Stderr, "error: looking up EdgeDevice for %q: %v\n", *deviceID, err)
		return 1
	}

	addr := *address
	if addr == "" {
		addr = dev.Status.Address
	}
	if addr == "" {
		fmt.Fprintf(os.Stderr, "error: device %q has no assigned address yet (status.address empty); pass --address or wait for the terminator to program it\n", *deviceID)
		return 1
	}

	profile, err := edge.BuildDeviceProfile(edge.DeviceProfileInput{
		DeviceID:      dev.Spec.DeviceID,
		PublicKey:     dev.Spec.PublicKey,
		Address:       addr,
		EdgeEndpoint:  *endpoint,
		PeerPublicKey: *peerPublicKey,
		RouteCIDRs:    splitCSV(*routeCIDRs),
		DNS:           gateway.DNSConfig{Addr: *dnsAddr, SearchDomains: splitCSV(*dnsSearch)},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if *asToken {
		token, err := profile.Encode()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "# Hand this token to the device.")
		fmt.Println(token)
		return 0
	}

	fmt.Fprintf(os.Stderr, "# wg-quick config for device %q. Save as /etc/wireguard/dwx-edge0.conf and `wg-quick up dwx-edge0`.\n", *deviceID)
	fmt.Print(profile.WireGuardQuickConfig(*privateKey))
	return 0
}

// runEdgeList lists enrolled EdgeDevices and their status.
func runEdgeList(args []string) int {
	fs := flag.NewFlagSet("edge list", flag.ExitOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	_ = fs.Parse(args)

	c, err := newClient(*kubeconfig, *kubecontext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var list networkingv1alpha1.EdgeDeviceList
	if err := c.List(ctx, &list); err != nil {
		fmt.Fprintf(os.Stderr, "error: listing EdgeDevices: %v\n", err)
		return 1
	}
	if len(list.Items) == 0 {
		fmt.Println("No EdgeDevices enrolled.")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DEVICE\tADDRESS\tPHASE\tHANDSHAKE\tMESSAGE")
	for i := range list.Items {
		d := &list.Items[i]
		handshake := "never"
		if d.Status.LastHandshakeTime > 0 {
			handshake = time.Unix(d.Status.LastHandshakeTime, 0).UTC().Format(time.RFC3339)
		}
		phase := string(d.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", d.Spec.DeviceID, orDash(d.Status.Address), phase, handshake, orDash(d.Status.Message))
	}
	_ = w.Flush()
	return 0
}

// applyEdgeDevice creates the EdgeDevice, or updates the spec of an existing one
// of the same name, so a re-enroll is a clean upsert rather than an error.
func applyEdgeDevice(ctx context.Context, c client.Client, dev *networkingv1alpha1.EdgeDevice) error {
	var existing networkingv1alpha1.EdgeDevice
	switch err := c.Get(ctx, types.NamespacedName{Name: dev.Name}, &existing); {
	case apierrors.IsNotFound(err):
		return c.Create(ctx, dev)
	case err != nil:
		return fmt.Errorf("checking for existing EdgeDevice %q: %w", dev.Name, err)
	default:
		existing.Spec = dev.Spec
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[edge.ManagedByLabel] = edge.ManagedByEdge
		return c.Update(ctx, &existing)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

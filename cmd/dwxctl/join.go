package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/bootstrap"
)

// runJoin implements the two halves of a zero-friction peering:
//
//	dwxctl join export    mint a bundle describing THIS cluster to hand to a peer
//	dwxctl join import    consume a peer's bundle and author the MeshPeer for it
//
// Run export on each cluster, swap the two tokens, run import on each with the
// other's token, and the mesh forms with no hand-written CRDs.
func runJoin(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: dwxctl join <export|import> [flags]")
		return 2
	}
	switch args[0] {
	case "export":
		return runJoinExport(args[1:])
	case "import":
		return runJoinImport(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown join subcommand %q (want export|import)\n", args[0])
		return 2
	}
}

// runJoinExport mints and prints this cluster's join bundle. The endpoint and
// cluster ID identify the cluster; the public key is either supplied (the node's
// existing key) or freshly generated with --generate, in which case the private
// key is printed to stderr to be stored as the node secret.
func runJoinExport(args []string) int {
	fs := flag.NewFlagSet("join export", flag.ExitOnError)
	clusterID := fs.String("cluster-id", "", "this cluster's stable mesh ID (required)")
	endpoint := fs.String("endpoint", "", "this cluster's reachable WireGuard host:port (required)")
	publicKey := fs.String("public-key", os.Getenv("DataWerx_WG_PUBLIC_KEY"), "this cluster's WireGuard public key")
	generate := fs.Bool("generate", false, "generate a fresh keypair (prints the private key to stderr)")
	podCIDRs := fs.String("pod-cidrs", "", "comma-separated local pod ranges to advertise")
	serviceCIDRs := fs.String("service-cidrs", "", "comma-separated local service ranges to advertise")
	_ = fs.Parse(args)

	if *clusterID == "" || *endpoint == "" {
		fmt.Fprintln(os.Stderr, "error: --cluster-id and --endpoint are required")
		return 2
	}

	pub := *publicKey
	if *generate {
		kp, err := bootstrap.GenerateKeypair()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		pub = kp.PublicKey
		fmt.Fprintf(os.Stderr, "Generated a new keypair. Store this private key as the node secret (DataWerx_WG_PRIVATE_KEY):\n  %s\n\n", kp.PrivateKey)
	}
	if pub == "" {
		fmt.Fprintln(os.Stderr, "error: provide --public-key or pass --generate")
		return 2
	}

	b, err := bootstrap.NewBundle(*clusterID, pub, *endpoint, splitCSV(*podCIDRs), splitCSV(*serviceCIDRs))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	token, err := b.Encode()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintln(os.Stderr, "# Hand this bundle to the peer cluster and run: dwxctl join import --bundle <token>")
	fmt.Println(token)
	return 0
}

// runJoinImport consumes a peer's bundle and authors the reciprocal MeshPeer. It
// is the one mutating dwxctl command; --dry-run prints the object instead of
// applying it. Re-importing the same bundle is an idempotent upsert.
func runJoinImport(args []string) int {
	fs := flag.NewFlagSet("join import", flag.ExitOnError)
	bundle := fs.String("bundle", "", "the peer's join bundle token")
	bundleFile := fs.String("bundle-file", "", "read the bundle token from a file ('-' for stdin)")
	dryRun := fs.Bool("dry-run", false, "print the MeshPeer that would be created, without applying it")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	_ = fs.Parse(args)

	token, err := readBundleToken(*bundle, *bundleFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	b, err := bootstrap.Decode(token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	peer := b.PeerObject()

	if *dryRun {
		out, err := yaml.Marshal(peer)
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

	if err := applyPeer(ctx, c, peer); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("Applied MeshPeer %q for cluster %q (endpoint %s).\n", peer.Name, b.ClusterID, b.Endpoint)
	return 0
}

// applyPeer creates the MeshPeer, or updates the spec of an existing one of the
// same name, so a re-import is a clean upsert rather than an error.
func applyPeer(ctx context.Context, c client.Client, peer *networkingv1alpha1.MeshPeer) error {
	var existing networkingv1alpha1.MeshPeer
	switch err := c.Get(ctx, types.NamespacedName{Name: peer.Name}, &existing); {
	case apierrors.IsNotFound(err):
		return c.Create(ctx, peer)
	case err != nil:
		return fmt.Errorf("checking for existing MeshPeer %q: %w", peer.Name, err)
	default:
		existing.Spec = peer.Spec
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[bootstrap.ManagedByLabel] = bootstrap.ManagedByJoin
		return c.Update(ctx, &existing)
	}
}

// readBundleToken resolves the bundle token from --bundle, --bundle-file, or
// stdin (when --bundle-file is "-").
func readBundleToken(inline, file string) (string, error) {
	switch {
	case inline != "":
		return inline, nil
	case file == "-":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading bundle from stdin: %w", err)
		}
		return string(data), nil
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading bundle file: %w", err)
		}
		return string(data), nil
	default:
		return "", fmt.Errorf("provide --bundle <token> or --bundle-file <path>")
	}
}

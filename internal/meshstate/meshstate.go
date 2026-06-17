// Package meshstate is the thin Kubernetes-reading shell that every read-only
// DataWerx surface shares: `dwxctl verify/snapshot/diagnose`, `dwxctl policy
// --dry-run`, and the `dwx-mcp` server. It gathers the cluster's observed mesh
// state through the same client the controllers use and hands it to the pure
// assembler in pkg/verify, so the snapshot the CLI prints, the health report
// verify renders, and the answers the MCP server gives can never disagree about
// what the mesh looks like.
//
// The decision logic lives in pkg/verify (pure, side-effect-free, exhaustively
// unit-tested with no cluster). Everything here is the I/O half of that split:
// it only reads the API and projects objects onto verify.SnapshotInputs.
package meshstate

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
)

const (
	// DefaultNamespace is the namespace the agent is installed in by the shipped
	// manifests (deploy/agent.yaml) and the Helm chart. The read-only commands
	// default to it so an operator running against a stock install needs no flags.
	DefaultNamespace = "datawerx-system"

	// DefaultDaemonSet is the agent DaemonSet name in the shipped manifests. It is
	// the object the health check reads desired/ready replica counts from.
	DefaultDaemonSet = "dwx-mesh-agent"
)

// NewClient builds the controller-runtime client the read-only surfaces share.
// It resolves cluster access the way a kubectl-style tool is expected to: an
// explicit kubeconfig path or context wins, otherwise it falls back to the
// in-cluster service account (so dwx-mcp can run as a pod) and then the ambient
// kubeconfig. The same scheme is registered everywhere so no surface can see a
// type another cannot.
func NewClient(kubeconfig, kubecontext string) (client.Client, error) {
	cfg, err := restConfig(kubeconfig, kubecontext)
	if err != nil {
		return nil, err
	}
	scheme, err := buildScheme()
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

// restConfig resolves a REST config. An explicit kubeconfig path or context is
// authoritative; with neither, an in-cluster config is preferred (the MCP server
// is meant to run as a pod), falling back to the ambient kubeconfig loading
// rules (KUBECONFIG / ~/.kube/config) for an operator at a laptop.
func restConfig(kubeconfig, kubecontext string) (*rest.Config, error) {
	if kubeconfig == "" && kubecontext == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubecontext != "" {
		overrides.CurrentContext = kubecontext
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

// buildScheme registers every group the gather touches onto a fresh scheme: the
// core/apps built-ins (DaemonSet, Event), both DataWerx API groups, and the
// apiextensions group used to probe CRD presence. It mirrors the registration
// cmd/manager performs.
func buildScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		networkingv1alpha1.AddToScheme,
		mcsv1alpha1.AddToScheme,
		apiextensionsv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return scheme, nil
}

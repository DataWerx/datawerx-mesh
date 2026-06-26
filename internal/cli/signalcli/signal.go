// Package signalcli implements the `signal` service of the unified `dwx` CLI —
// the open-core core of DataWerx Signal, a grounded natural-language
// question-answering layer over a running DataWerx mesh. It is also what the
// deprecated `dwx-signal` alias dispatches into (see cmd/dwx, design 0016).
//
// It assembles the same deterministic evidence every other read surface uses
// such as the versioned snapshot, the rule-based diagnosis, the expected
// reachability matrix, and the 'golden-signal' connectivity report.  It asks
// an AI model (Claude) to reason over only that evidence, returning a
// structured root-cause answer whose everyclaim cites the signal it came from.
// The model selects and explains; the facts are produced by the deterministic
// engines, so an answer can never drift from what
// `dwxctl snapshot/diagnose/reach/slo` and `dwx-mcp` already report.
//
// Two modes:
//
//	dwx-signal --snapshot snap.json "Why can't payments reach inventory?"
//	dwx-signal "Which clusters are unhealthy?"            # live cluster
//	dwx-signal --print-context --snapshot snap.json "..." # no model call
//
// The evidence can come from a file based on `dwxctl snapshot` / `dwx-mcp`
// output so the tool runs with no cluster, or live from the API. `--print-context`
// shows the exact grounded evidence the model would receive — useful for trust,
// debugging, and demos — and needs no API key.
//
// Like dwx-mcp, this binary is deliberately read-only. It answers questions but
// never mutates the mesh. Acting on the mesh stays the governed in premium tier.
//
// The model is reached with the API key in ANTHROPIC_API_KEY over the standard
// Messages API, using only the standard library so the open core gains no new
// module dependency.
package signalcli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/DataWerx/datawerx-mesh/internal/meshstate"
	"github.com/DataWerx/datawerx-mesh/pkg/logging"
	"github.com/DataWerx/datawerx-mesh/pkg/signal"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// Run executes the `signal` service. prog is the invocation prefix used in
// usage/version text (e.g. "dwx signal" or the legacy "dwx-signal"); args are
// the arguments after that prefix. It writes to the supplied streams and
// returns an error for the caller to map to an exit code.
func Run(prog string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(prog, flag.ContinueOnError)
	fs.SetOutput(stderr)

	snapshotPath := fs.String("snapshot", "", "read evidence from a snapshot JSON file (from `dwxctl snapshot` / `dwx-mcp`) instead of the live cluster; use - for stdin")
	printContext := fs.Bool("print-context", false, "print the grounded evidence that would be sent to the model, then exit (no API key needed)")
	asJSON := fs.Bool("json", false, "print the answer as JSON instead of formatted text")
	model := fs.String("model", signal.DefaultModel, "Claude model to reason with")
	namespace := fs.String("namespace", "", "namespace the agent is installed in (default: datawerx-system)")
	daemonset := fs.String("daemonset", "", "agent DaemonSet name (default: dwx-mesh-agent)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	showVersion := fs.Bool("version", false, "print the dwx-signal version and exit")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s [flags] \"<question>\"\n\n", prog)
		fmt.Fprintf(stderr, "Ask a grounded question about a DataWerx mesh. The question may also be piped on stdin.\n\n")
		fmt.Fprintf(stderr, "Flags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Fprintf(stdout, "%s %s\n", prog, logging.Version)
		return nil
	}

	question, err := resolveQuestion(fs.Args())
	if err != nil {
		return err
	}

	snap, err := loadSnapshot(*snapshotPath, snapshotConfig{
		namespace:   *namespace,
		daemonset:   *daemonset,
		kubeconfig:  *kubeconfig,
		kubecontext: *kubecontext,
	})
	if err != nil {
		return err
	}

	ev := signal.BuildEvidence(snap)

	if *printContext {
		b, err := ev.JSON()
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n", b)
		return nil
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY is not set; set it, or use --print-context to inspect the grounded evidence without a model call")
	}

	client := signal.NewClient(apiKey)
	client.Model = *model

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rc, err := client.Answer(ctx, question, ev)
	if err != nil {
		return err
	}

	if *asJSON {
		b, err := json.MarshalIndent(rc, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n", b)
		return nil
	}
	printRootCause(stdout, rc)
	return nil
}

// resolveQuestion takes the question from the remaining args, or from stdin when
// none was given (so it composes in a pipeline).
func resolveQuestion(args []string) (string, error) {
	if len(args) > 0 {
		return strings.TrimSpace(strings.Join(args, " ")), nil
	}
	info, err := os.Stdin.Stat()
	if err == nil && (info.Mode()&os.ModeCharDevice) == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read question from stdin: %w", err)
		}
		if q := strings.TrimSpace(string(b)); q != "" {
			return q, nil
		}
	}
	return "", fmt.Errorf("no question given; pass it as an argument or on stdin")
}

type snapshotConfig struct {
	namespace   string
	daemonset   string
	kubeconfig  string
	kubecontext string
}

// loadSnapshot reads evidence from a file (decoupled from any cluster) or
// gathers it live through the shared meshstate shell.
func loadSnapshot(path string, cfg snapshotConfig) (verify.Snapshot, error) {
	if path != "" {
		var data []byte
		var err error
		if path == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			return verify.Snapshot{}, fmt.Errorf("read snapshot %q: %w", path, err)
		}
		var snap verify.Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			return verify.Snapshot{}, fmt.Errorf("parse snapshot %q: %w", path, err)
		}
		return snap, nil
	}

	ns := cfg.namespace
	if ns == "" {
		ns = meshstate.DefaultNamespace
	}
	ds := cfg.daemonset
	if ds == "" {
		ds = meshstate.DefaultDaemonSet
	}
	client, err := meshstate.NewClient(cfg.kubeconfig, cfg.kubecontext)
	if err != nil {
		return verify.Snapshot{}, fmt.Errorf("connect to cluster: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return meshstate.Snapshot(ctx, client, ns, ds)
}

// printRootCause renders the structured answer for a terminal.
func printRootCause(w io.Writer, rc signal.RootCause) {
	fmt.Fprintf(w, "Problem:    %s\n", rc.Problem)
	fmt.Fprintf(w, "Cause:      %s\n", rc.Cause)
	fmt.Fprintf(w, "Impact:     %s\n", rc.Impact)
	fmt.Fprintf(w, "Confidence: %.2f\n", rc.Confidence)
	if len(rc.RecommendedActions) > 0 {
		fmt.Fprintf(w, "\nRecommended actions:\n")
		for i, a := range rc.RecommendedActions {
			fmt.Fprintf(w, "  %d. %s\n", i+1, a)
		}
	}
	if len(rc.Citations) > 0 {
		fmt.Fprintf(w, "\nGrounded in:\n")
		for _, c := range rc.Citations {
			fmt.Fprintf(w, "  - %s\n", c)
		}
	}
}

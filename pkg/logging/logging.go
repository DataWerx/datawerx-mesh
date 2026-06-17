// Package logging centralizes how the DataWerx Mesh agent builds its logger so
// every binary gets the same structured, operator-controllable, diagnosis-grade
// output without each call site re-deriving configuration.
//
// It is a thin, opinionated layer over controller-runtime's zap logger
// (sigs.k8s.io/controller-runtime/pkg/log/zap), adding:
//
//   - environment-variable configuration (DataWerx_LOG_*) layered UNDER the
//     existing --zap-* CLI flags, so a containerized DaemonSet can be tuned
//     without rewriting args while flags still win for ad-hoc debugging;
//   - source-location ("caller") annotation on by default, so every line points
//     at the file:line that emitted it;
//   - a single structured startup banner (LogStartup) stamping the build version,
//     Go toolchain, platform, and VCS revision — the canonical first line for
//     diagnosing any deployment.
//
// Verbosity convention (logr V-levels; higher == more detail, suppressed unless
// the configured level is raised). Think of it in Serilog terms:
//
//	V(0) Info  → Information : lifecycle and state-CHANGE events
//	V(1) Debug → Debug       : steady-state confirmations, per-reconcile detail
//	V(2) Trace → Verbose     : per-item, high-frequency detail
//
// logr has no Warn level: an expected, recoverable degradation is logged as
// Info with an "err" field, while Logger.Error is reserved for unexpected
// failures (it also triggers a stacktrace at the configured StacktraceLevel).
package logging

import (
	"flag"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// V-level constants naming the verbosity convention documented above. Use them
// as logger.V(logging.Debug) so the intent reads at the call site instead of a
// bare magic number.
const (
	// Debug is the V-level for steady-state, per-reconcile confirmations that are
	// noise in healthy steady state but invaluable when diagnosing.
	Debug = 1
	// Trace is the V-level for per-item, high-frequency detail.
	Trace = 2
)

// Environment variables that tune the logger. They are read once at startup and
// only take effect for a given facet when its corresponding --zap-* flag was NOT
// explicitly provided, so explicit flags always win.
const (
	// EnvLevel sets the verbosity threshold: "error", "warn", "info" (default),
	// "debug" (shows V(1)), "trace" (shows V(2)), or any positive integer N which
	// shows up to V(N) — matching the --zap-log-level integer semantics.
	EnvLevel = "DataWerx_LOG_LEVEL"
	// EnvFormat selects the encoder: "json" (default, machine-parseable) or
	// "console"/"text" (human-friendly, for local debugging).
	EnvFormat = "DataWerx_LOG_FORMAT"
	// EnvTime selects timestamp encoding: "iso8601" (default here), "rfc3339",
	// "rfc3339nano", "millis", "nano", or "epoch".
	EnvTime = "DataWerx_LOG_TIME"
	// EnvStacktrace sets the level at/above which stacktraces are captured:
	// "error" (default), "warn", "info", or "none" to disable them.
	EnvStacktrace = "DataWerx_LOG_STACKTRACE"
	// EnvCaller toggles source-location annotation. Truthy (default) adds a
	// "caller" field of file:line; set it falsy to drop it.
	EnvCaller = "DataWerx_LOG_CALLER"
	// EnvDevelopment, when truthy, flips controller-runtime into development mode
	// (console encoder, debug level, human-friendly defaults). Equivalent to
	// --zap-devel.
	EnvDevelopment = "DataWerx_LOG_DEVELOPMENT"
)

// Version is the build version, overridable at link time with
//
//	-ldflags "-X github.com/DataWerx/datawerx-mesh/pkg/logging.Version=v1.2.3"
//
// When left at "dev", LogStartup falls back to the module version embedded by
// the Go toolchain (debug.ReadBuildInfo).
var Version = "dev"

// Options builds the agent logger from CLI flags and environment, with
// precedence: explicit --zap-* flag > DataWerx_LOG_* env > built-in default.
//
// Typical use mirrors controller-runtime's flag idiom:
//
//	logOpts := logging.BindFlags(flag.CommandLine)
//	flag.Parse()
//	ctrl.SetLogger(logOpts.Build())
type Options struct {
	fs     *flag.FlagSet
	getenv func(string) string
	zap    crzap.Options
}

// BindFlags registers the --zap-* logging flags on fs and returns Options whose
// Build method should be called AFTER flag.Parse.
func BindFlags(fs *flag.FlagSet) *Options {
	o := &Options{fs: fs, getenv: os.Getenv}
	o.zap.BindFlags(fs)
	return o
}

// Build assembles the configured logr.Logger. It applies environment defaults
// for every facet whose --zap-* flag was not explicitly set, then constructs the
// controller-runtime zap logger.
func (o *Options) Build() logr.Logger {
	o.applyEnv()
	return crzap.New(crzap.UseFlagOptions(&o.zap))
}

// applyEnv fills logger options from the environment, but only for facets the
// user did not pin with an explicit flag so flags keep priority.
func (o *Options) applyEnv() {
	set := map[string]bool{}
	if o.fs != nil {
		o.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	}

	if !set["zap-devel"] {
		if b, ok := parseBool(o.getenv(EnvDevelopment)); ok {
			o.zap.Development = b
		}
	}
	if !set["zap-log-level"] {
		if lvl, ok := parseLevel(o.getenv(EnvLevel)); ok {
			o.zap.Level = lvl
		}
	}
	if !set["zap-encoder"] {
		switch strings.ToLower(strings.TrimSpace(o.getenv(EnvFormat))) {
		case "json":
			o.zap.NewEncoder = jsonEncoder
		case "console", "text":
			o.zap.NewEncoder = consoleEncoder
		}
	}
	if !set["zap-time-encoding"] {
		if te, ok := parseTimeEncoder(o.getenv(EnvTime)); ok {
			o.zap.TimeEncoder = te
		} else if o.zap.TimeEncoder == nil {
			// Default to human-readable ISO8601 rather than controller-runtime's
			// epoch seconds, which are unreadable at a glance during an incident.
			o.zap.TimeEncoder = zapcore.ISO8601TimeEncoder
		}
	}
	if !set["zap-stacktrace-level"] {
		if sl, ok := parseStacktrace(o.getenv(EnvStacktrace)); ok {
			o.zap.StacktraceLevel = sl
		}
	}

	// Source-location annotation, on unless explicitly disabled. zapr applies the
	// correct caller-skip for its wrapper, so the "caller" field points at the
	// real emitting site.
	if b, ok := parseBool(o.getenv(EnvCaller)); !ok || b {
		o.zap.ZapOpts = append(o.zap.ZapOpts, zap.AddCaller())
	}
}

// jsonEncoder / consoleEncoder mirror controller-runtime's own (unexported)
// encoder constructors so that EncoderConfigOptions — notably the TimeEncoder —
// still apply when we select the encoder from the environment.
func jsonEncoder(opts ...crzap.EncoderConfigOption) zapcore.Encoder {
	cfg := zap.NewProductionEncoderConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return zapcore.NewJSONEncoder(cfg)
}

func consoleEncoder(opts ...crzap.EncoderConfigOption) zapcore.Encoder {
	cfg := zap.NewDevelopmentEncoderConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return zapcore.NewConsoleEncoder(cfg)
}

// parseLevel maps a human level name (or integer verbosity) to a zap level
// threshold. logr verbosity V(n) maps to zap level -n, so "debug" (-1) reveals
// V(1) and "trace" (-2) reveals V(2). Returns ok=false for empty/unparseable
// input so the caller leaves the default untouched.
func parseLevel(s string) (zapcore.LevelEnabler, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "":
		return nil, false
	case "trace":
		return atLevel(zapcore.Level(-2)), true
	case "debug":
		return atLevel(zapcore.DebugLevel), true // -1, reveals V(1)
	case "info":
		return atLevel(zapcore.InfoLevel), true // 0
	case "warn", "warning":
		return atLevel(zapcore.WarnLevel), true
	case "error":
		return atLevel(zapcore.ErrorLevel), true
	}
	if n, err := strconv.Atoi(s); err == nil {
		// Positive verbosity N reveals up to V(N); clamp negatives to Info.
		if n < 0 {
			n = 0
		}
		return atLevel(zapcore.Level(-n)), true
	}
	return nil, false
}

func atLevel(l zapcore.Level) zapcore.LevelEnabler {
	a := zap.NewAtomicLevelAt(l)
	return &a
}

// parseTimeEncoder maps a name to a zap time encoder.
func parseTimeEncoder(s string) (zapcore.TimeEncoder, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "iso8601", "iso":
		return zapcore.ISO8601TimeEncoder, true
	case "rfc3339":
		return zapcore.RFC3339TimeEncoder, true
	case "rfc3339nano":
		return zapcore.RFC3339NanoTimeEncoder, true
	case "millis":
		return zapcore.EpochMillisTimeEncoder, true
	case "nano":
		return zapcore.EpochNanosTimeEncoder, true
	case "epoch":
		return zapcore.EpochTimeEncoder, true
	default:
		return nil, false
	}
}

// parseStacktrace maps a name to the level at/above which stacktraces attach.
// "none" disables them by setting the threshold above any emitted level.
func parseStacktrace(s string) (zapcore.LevelEnabler, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return atLevel(zapcore.InfoLevel), true
	case "warn", "warning":
		return atLevel(zapcore.WarnLevel), true
	case "error":
		return atLevel(zapcore.ErrorLevel), true
	case "none", "off", "disabled":
		return atLevel(zapcore.PanicLevel), true
	default:
		return nil, false
	}
}

// parseBool reports a truthy/falsy env value and whether it was set at all.
func parseBool(s string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return false, false
	case "1", "true", "yes", "on", "enabled":
		return true, true
	default:
		return false, true
	}
}

// ShortKey returns a truncated, log-safe rendering of sensitive key material
// (e.g. a WireGuard public key) so full keys never land in logs. Short inputs
// are returned unchanged so empty/sentinel values stay legible.
func ShortKey(k string) string {
	const keep = 8
	if len(k) <= keep {
		return k
	}
	return k[:keep] + "…"
}

// BuildInfo captures the identity stamped into the startup banner.
type BuildInfo struct {
	Version     string
	GoVersion   string
	Platform    string
	VCSRevision string
	VCSTime     string
	Modified    bool
}

// ReadBuildInfo resolves the running binary's identity, preferring the
// link-time Version and falling back to the module + VCS metadata the Go
// toolchain embeds.
func ReadBuildInfo() BuildInfo {
	bi := BuildInfo{
		Version:   Version,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return bi
	}
	if bi.Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		bi.Version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			bi.VCSRevision = s.Value
		case "vcs.time":
			bi.VCSTime = s.Value
		case "vcs.modified":
			bi.Modified = s.Value == "true"
		}
	}
	return bi
}

// LogStartup emits the single canonical startup line stamping build and runtime
// identity, plus any caller-supplied key/value context (tier, data plane, ...).
func LogStartup(log logr.Logger, kv ...any) {
	bi := ReadBuildInfo()
	fields := []any{
		"version", bi.Version,
		"goVersion", bi.GoVersion,
		"platform", bi.Platform,
	}
	if bi.VCSRevision != "" {
		rev := bi.VCSRevision
		if len(rev) > 12 {
			rev = rev[:12]
		}
		fields = append(fields, "commit", rev, "dirty", bi.Modified)
		if bi.VCSTime != "" {
			fields = append(fields, "buildTime", bi.VCSTime)
		}
	}
	log.Info("starting DataWerx Mesh agent", append(fields, kv...)...)
}

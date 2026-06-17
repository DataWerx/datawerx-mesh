package logging

import (
	"bytes"
	"encoding/json"
	"flag"
	"strings"
	"testing"

	"go.uber.org/zap/zapcore"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// newTestOptions wires Options to an in-memory sink and a fake environment so a
// fully-configured logger can be exercised without touching the process env or
// stderr.
func newTestOptions(buf *bytes.Buffer, env map[string]string) *Options {
	o := &Options{
		fs:     flag.NewFlagSet("test", flag.ContinueOnError),
		getenv: func(k string) string { return env[k] },
		zap:    crzap.Options{DestWriter: buf},
	}
	o.zap.BindFlags(o.fs)
	return o
}

// parseLines decodes the JSON log lines emitted into buf.
func parseLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("log line is not valid JSON (%q): %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}

func TestBuildDefaultsHideDebugAndAnnotateCaller(t *testing.T) {
	var buf bytes.Buffer
	log := newTestOptions(&buf, nil).Build()

	log.Info("info-line", "k", "v")
	log.V(Debug).Info("debug-line")

	lines := parseLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected only the Info line at default level, got %d lines: %s", len(lines), buf.String())
	}
	got := lines[0]
	if got["msg"] != "info-line" {
		t.Errorf("msg = %v, want info-line", got["msg"])
	}
	if got["level"] != "info" {
		t.Errorf("level = %v, want info", got["level"])
	}
	if got["k"] != "v" {
		t.Errorf("structured field k = %v, want v", got["k"])
	}
	caller, ok := got["caller"].(string)
	if !ok || !strings.Contains(caller, "logging_test.go") {
		t.Errorf("caller = %v, want it to point at logging_test.go", got["caller"])
	}
	// Default timestamp encoding should be human-readable ISO8601, not an epoch
	// float.
	if ts, ok := got["ts"].(string); !ok || !strings.Contains(ts, "T") {
		t.Errorf("ts = %v, want an ISO8601 string", got["ts"])
	}
}

func TestEnvLevelRevealsDebug(t *testing.T) {
	var buf bytes.Buffer
	log := newTestOptions(&buf, map[string]string{EnvLevel: "debug"}).Build()

	log.V(Debug).Info("debug-line")
	log.V(Trace).Info("trace-line") // still hidden at debug

	lines := parseLines(t, &buf)
	if len(lines) != 1 || lines[0]["msg"] != "debug-line" {
		t.Fatalf("debug level should reveal V(1) but not V(2); got %s", buf.String())
	}
}

func TestEnvCallerCanBeDisabled(t *testing.T) {
	var buf bytes.Buffer
	log := newTestOptions(&buf, map[string]string{EnvCaller: "false"}).Build()
	log.Info("no-caller")

	lines := parseLines(t, &buf)
	if _, ok := lines[0]["caller"]; ok {
		t.Errorf("caller field present despite %s=false: %s", EnvCaller, buf.String())
	}
}

func TestExplicitFlagBeatsEnv(t *testing.T) {
	var buf bytes.Buffer
	o := newTestOptions(&buf, map[string]string{EnvLevel: "debug"})
	// Simulate the operator passing --zap-log-level=info explicitly; env must not
	// override it back to debug.
	if err := o.fs.Parse([]string{"--zap-log-level=info"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	log := o.Build()
	log.V(Debug).Info("debug-line")

	if strings.TrimSpace(buf.String()) != "" {
		t.Errorf("explicit --zap-log-level=info should suppress V(1) despite %s=debug; got %s", EnvLevel, buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want zapcore.Level
		ok   bool
	}{
		{"", 0, false},
		{"info", zapcore.InfoLevel, true},
		{"debug", zapcore.DebugLevel, true},
		{"trace", zapcore.Level(-2), true},
		{"warn", zapcore.WarnLevel, true},
		{"error", zapcore.ErrorLevel, true},
		{"3", zapcore.Level(-3), true},
		{"-5", zapcore.InfoLevel, true}, // negatives clamp to Info
		{"nonsense", 0, false},
	}
	for _, c := range cases {
		got, ok := parseLevel(c.in)
		if ok != c.ok {
			t.Errorf("parseLevel(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		// Enabled(L) reports L >= threshold; the threshold equals want iff want is
		// enabled and want-1 is not.
		if !got.Enabled(c.want) || got.Enabled(c.want-1) {
			t.Errorf("parseLevel(%q) threshold not at %v", c.in, c.want)
		}
	}
}

func TestParseTimeEncoderAndStacktrace(t *testing.T) {
	if _, ok := parseTimeEncoder("iso8601"); !ok {
		t.Error("iso8601 should parse")
	}
	if _, ok := parseTimeEncoder("bogus"); ok {
		t.Error("bogus time encoding should not parse")
	}
	if _, ok := parseStacktrace("none"); !ok {
		t.Error("none should parse for stacktrace")
	}
	if _, ok := parseStacktrace("bogus"); ok {
		t.Error("bogus stacktrace level should not parse")
	}
}

func TestConsoleFormat(t *testing.T) {
	var buf bytes.Buffer
	log := newTestOptions(&buf, map[string]string{EnvFormat: "console"}).Build()
	log.Info("hello-console")
	// Console output is not JSON; it should still contain the message verbatim.
	if !strings.Contains(buf.String(), "hello-console") {
		t.Errorf("console output missing message: %s", buf.String())
	}
	if json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Errorf("expected non-JSON console output, got %s", buf.String())
	}
}

func TestLogStartupStampsBuildInfo(t *testing.T) {
	var buf bytes.Buffer
	log := newTestOptions(&buf, nil).Build()
	LogStartup(log, "tier", "free")

	lines := parseLines(t, &buf)
	got := lines[0]
	if got["msg"] != "starting DataWerx Mesh agent" {
		t.Errorf("msg = %v", got["msg"])
	}
	for _, k := range []string{"version", "goVersion", "platform", "tier"} {
		if _, ok := got[k]; !ok {
			t.Errorf("startup banner missing field %q: %s", k, buf.String())
		}
	}
	if got["tier"] != "free" {
		t.Errorf("tier = %v, want free", got["tier"])
	}
}

func TestReadBuildInfoPopulatesRuntime(t *testing.T) {
	bi := ReadBuildInfo()
	if bi.GoVersion == "" || bi.Platform == "" {
		t.Errorf("ReadBuildInfo missing runtime identity: %+v", bi)
	}
	if !strings.Contains(bi.Platform, "/") {
		t.Errorf("Platform = %q, want os/arch", bi.Platform)
	}
}

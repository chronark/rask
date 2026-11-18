package e2e

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestAxiomReportsOnlyExecutedSuites(t *testing.T) {
	capture, env := captureTraces(t, "test-traces")
	env["OTEL_RESOURCE_ATTRIBUTES"] = "vcs.ref.head.revision=abc123,service.name=ignored"
	env["OTEL_SERVICE_NAME"] = "ignored"
	module := scaffoldModule(t, packageSpec{name: "passing", test: testPass})
	assertCode(t, runRask(t, module.dir, env), 0)
	spans := capture.take()
	executions, run := splitRunSpan(t, spans)
	if len(executions) != 2 {
		t.Fatalf("got %d execution spans, want one package and one test", len(executions))
	}
	for _, span := range spans {
		if span.StartTimeUnixNano == 0 || span.EndTimeUnixNano < span.StartTimeUnixNano {
			t.Fatalf("invalid span timestamps: %v", span.Span)
		}
		if span.resource["service.name"] != "rask" || span.resource["vcs.ref.head.revision"] != "abc123" {
			t.Fatalf("span %q has resource %v", span.Name, span.resource)
		}
	}
	assertRunCounts(t, run, map[string]int64{"run.suites": 1, "run.suites.executed": 1, "run.suites.cached": 0})
	assertCode(t, runRask(t, module.dir, env), 0)
	executions, run = splitRunSpan(t, capture.take())
	if len(executions) != 0 {
		t.Fatal("cached suites must not export fresh execution spans")
	}
	assertRunCounts(t, run, map[string]int64{"run.suites": 1, "run.suites.executed": 0, "run.suites.cached": 1})
}

func TestAxiomRejectsMalformedResourceAttributes(t *testing.T) {
	capture, env := captureTraces(t, "test-traces")
	env["OTEL_RESOURCE_ATTRIBUTES"] = "missing-separator"
	module := scaffoldModule(t, packageSpec{name: "passing", test: testPass})
	result := runRask(t, module.dir, env)
	assertCode(t, result, 0)
	assertContains(t, result.stderr, "tracing disabled")
	if spans := capture.take(); len(spans) != 0 {
		t.Fatalf("got %d spans after rejecting the resource", len(spans))
	}
}

func TestAxiomDefaultsToRaskTraces(t *testing.T) {
	capture, env := captureTraces(t, "rask-traces")
	delete(env, "RASK_AXIOM_DATASET")
	env["RASK_AXIOM_TRACE_TOKEN"] = "test-token"
	env["RASK_AXIOM_TOKEN"] = "unused-token"
	module := scaffoldModule(t, packageSpec{name: "passing", test: testPass})
	assertCode(t, runRask(t, module.dir, env), 0)
	if executions, _ := splitRunSpan(t, capture.take()); len(executions) != 2 {
		t.Fatalf("got %d spans, want package and test with the default dataset", len(executions))
	}
}

func TestAxiomUsesClaimedTestIdentity(t *testing.T) {
	capture, env := captureTraces(t, "test-traces")
	claimsDir := t.TempDir()
	env["CLAIMS_DIR"] = claimsDir
	env["GOFLAGS"] = "-cpu=1,2"
	module := scaffoldModule(t, packageSpec{name: "claims", test: testPass})
	writeFile(t, module.dir, "go.mod", "module "+module.path+"\n\ngo 1.27.1\n")
	writeFile(t, module.dir, "claims/claims_test.go", `package claims
import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)
func TestMain(m *testing.M) {
	flag.Parse()
	if flag.Lookup("test.list").Value.String() != "" && os.Getenv("RASK_TRACEPARENT") != "" {
		panic("enumeration must not receive a trace context")
	}
	os.Exit(m.Run())
}
func TestScopes(t *testing.T) {
	for range 2 {
		t.Run("same", func(t *testing.T) {
			t.Parallel()
			parts := strings.Split(os.Getenv("RASK_TRACEPARENT"), "-")
			if len(parts) != 4 { t.Fatal("missing package trace context") }
			id := make([]byte, 8)
			rand.Read(id)
			parts[2] = hex.EncodeToString(id)
			t.Attr("rask.test.traceparent.v1", strings.Join(parts, "-"))
			if err := os.WriteFile(filepath.Join(os.Getenv("CLAIMS_DIR"), parts[2]), []byte(t.Name()), 0600); err != nil { t.Fatal(err) }
		})
	}
	t.Run("skip", func(t *testing.T) { t.Skip("not available") })
}
`)
	result := runRask(t, module.dir, env, "--x-junit-file", "junit.xml")
	assertCode(t, result, 0)
	spans, _ := splitRunSpan(t, capture.take())
	if len(spans) != 9 {
		t.Fatalf("got %d spans, want package plus four tests on each CPU setting", len(spans))
	}
	byID := make(map[string]capturedSpan)
	var packageSpan capturedSpan
	for _, span := range spans {
		id := hex.EncodeToString(span.SpanId)
		if byID[id].Span != nil {
			t.Fatal("repeated execution reused a span ID")
		}
		byID[id] = span
		if span.Name == module.path+"/claims" {
			packageSpan = span
		}
	}
	entries, err := os.ReadDir(claimsDir)
	if err != nil || len(entries) != 4 || packageSpan.Span == nil {
		t.Fatalf("claims=%d package=%v err=%v", len(entries), packageSpan.Span, err)
	}
	for _, entry := range entries {
		name, err := os.ReadFile(filepath.Join(claimsDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		span := byID[entry.Name()]
		if span.Span == nil || span.Name != string(name) || hex.EncodeToString(span.ParentSpanId) != hex.EncodeToString(packageSpan.SpanId) {
			t.Fatalf("claim %s for %s did not become the matching test span", entry.Name(), name)
		}
		if hex.EncodeToString(span.TraceId) != hex.EncodeToString(packageSpan.TraceId) {
			t.Fatal("claimed span left the package trace")
		}
	}
	report, err := os.ReadFile(filepath.Join(module.dir, "junit.xml"))
	if err != nil {
		t.Fatal(err)
	}
	assertNotContains(t, string(report), "rask.test.traceparent")
}

type traceCapture struct {
	mu    sync.Mutex
	spans []capturedSpan
}

type capturedSpan struct {
	*tracepb.Span
	scope    string
	resource map[string]string
}

// splitRunSpan separates the single per-invocation summary from execution spans.
func splitRunSpan(t *testing.T, spans []capturedSpan) ([]capturedSpan, capturedSpan) {
	t.Helper()
	var executions []capturedSpan
	var runs []capturedSpan
	for _, span := range spans {
		if span.scope == "rask.run" {
			runs = append(runs, span)
		} else {
			executions = append(executions, span)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("got %d run spans, want 1", len(runs))
	}
	return executions, runs[0]
}

func assertRunCounts(t *testing.T, run capturedSpan, want map[string]int64) {
	t.Helper()
	got := make(map[string]int64)
	for _, attribute := range run.Attributes {
		got[attribute.Key] = attribute.Value.GetIntValue()
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("run span %s = %d, want %d (attributes %v)", key, got[key], value, got)
		}
	}
}

func captureTraces(t *testing.T, dataset string) (*traceCapture, map[string]string) {
	t.Helper()
	capture := &traceCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-Axiom-Dataset") != dataset {
			t.Errorf("unexpected OTLP request: %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request collector.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		capture.mu.Lock()
		for _, resourceSpans := range request.ResourceSpans {
			resource := make(map[string]string)
			for _, attribute := range resourceSpans.Resource.GetAttributes() {
				resource[attribute.Key] = attribute.Value.GetStringValue()
			}
			for _, scope := range resourceSpans.ScopeSpans {
				for _, span := range scope.Spans {
					capture.spans = append(capture.spans, capturedSpan{Span: span, scope: scope.Scope.GetName(), resource: resource})
				}
			}
		}
		capture.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	env := cacheEnv(t.TempDir())
	env["RASK_AXIOM_DATASET"] = dataset
	env["RASK_AXIOM_TOKEN"] = "test-token"
	env["RASK_AXIOM_ENDPOINT"] = server.URL + "/v1/traces"
	return capture, env
}

func (c *traceCapture) take() []capturedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	spans := c.spans
	c.spans = nil
	return spans
}

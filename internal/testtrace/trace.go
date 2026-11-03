// Package testtrace implements the experimental Axiom test-timing report.
package testtrace

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const executionCountMax = 65536

const identityAttribute = "rask.test.traceparent.v1"

type Config struct {
	Dataset  string
	Token    string
	Endpoint string
}

type Reporter struct {
	provider *sdktrace.TracerProvider
	exporter *reportingExporter
	warnings io.Writer
	limited  atomic.Bool
}

type Package struct {
	reporter *Reporter
	ctx      context.Context
	span     trace.Span
	name     string
	active   map[string]*execution
	used     map[trace.SpanID]bool
	count    int
}

type Event struct {
	Time    time.Time
	Action  string
	Package string
	Test    string
	Output  string
	Elapsed float64
	Key     string
	Value   string
}

type execution struct {
	id      trace.SpanID
	claimed bool
	started time.Time
	paused  time.Time
	wait    time.Duration
}

func New(cfg Config, warnings io.Writer) (*Reporter, error) {
	if cfg.Dataset == "" && cfg.Token == "" {
		return nil, nil
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("tracing requires RASK_AXIOM_TRACE_TOKEN or RASK_AXIOM_TOKEN")
	}
	if cfg.Dataset == "" {
		cfg.Dataset = "rask-traces"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://us-east-1.aws.edge.axiom.co/v1/traces"
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return nil, fmt.Errorf("RASK_AXIOM_ENDPOINT must be an HTTP(S) traces URL")
	}
	exporter, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": "Bearer " + cfg.Token, "X-Axiom-Dataset": cfg.Dataset,
		}),
		otlptracehttp.WithTimeout(time.Second),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, fmt.Errorf("creating trace exporter: %w", err)
	}
	reporting := &reportingExporter{SpanExporter: exporter}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithIDGenerator(assignedIDs{}),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "rask"))),
		sdktrace.WithBatcher(reporting, sdktrace.WithBatchTimeout(100*time.Millisecond),
			sdktrace.WithMaxQueueSize(8192), sdktrace.WithExportTimeout(time.Second)),
	)
	return &Reporter{provider: provider, exporter: reporting, warnings: warnings}, nil
}

func (r *Reporter) Close() {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := r.provider.Shutdown(ctx)
	if err != nil || r.exporter.failed.Load() || r.limited.Load() {
		fmt.Fprintln(r.warnings, "rask: trace report is incomplete (export failed or tracing limit reached); test results are unchanged")
	}
}

func (r *Reporter) StartPackage(name string) *Package {
	if r == nil {
		return nil
	}
	ctx, span := r.provider.Tracer("rask.test").Start(context.Background(), name,
		trace.WithAttributes(attribute.String("test.package", name)))
	return &Package{reporter: r, ctx: ctx, span: span, name: name,
		active: make(map[string]*execution), used: map[trace.SpanID]bool{span.SpanContext().SpanID(): true}}
}

func (p *Package) Environment() []string {
	if p == nil {
		return nil
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(p.ctx, carrier)
	return []string{"RASK_TRACEPARENT=" + carrier.Get("traceparent")}
}

func (p *Package) Observe(ev Event) {
	if p == nil || ev.Test == "" {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	switch ev.Action {
	case "run":
		if p.count >= executionCountMax {
			p.reporter.limited.Store(true)
			return
		}
		p.count++
		id := assignedIDs{}.NewSpanID(context.Background(), trace.TraceID{})
		p.used[id] = true
		p.active[ev.Test] = &execution{id: id, started: ev.Time}
	case "attr":
		if ev.Key == identityAttribute {
			p.claim(ev.Test, ev.Value)
		}
	case "pause":
		if test := p.active[ev.Test]; test != nil {
			test.paused = ev.Time
		}
	case "cont":
		if test := p.active[ev.Test]; test != nil && !test.paused.IsZero() {
			test.wait += ev.Time.Sub(test.paused)
			test.paused = time.Time{}
		}
	case "pass", "fail", "skip":
		p.finishTest(ev.Test, ev.Action, ev.Time, &ev.Elapsed)
	}
}

func (p *Package) claim(name, value string) {
	test := p.active[name]
	if test == nil {
		return
	}
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": value})
	claim := trace.SpanContextFromContext(ctx)
	if !claim.IsValid() || claim.TraceID() != p.span.SpanContext().TraceID() || !claim.IsSampled() {
		p.reporter.limited.Store(true)
		return
	}
	if test.claimed && test.id == claim.SpanID() {
		return
	}
	if test.claimed || p.used[claim.SpanID()] {
		p.reporter.limited.Store(true)
		return
	}
	test.id, test.claimed = claim.SpanID(), true
	p.used[test.id] = true
}

func IsIdentityOutput(output string) bool {
	return strings.HasPrefix(output, "=== ATTR  ") && strings.Contains(output, " "+identityAttribute+" ")
}

func (p *Package) Finish(failed, killed bool) {
	if p == nil {
		return
	}
	outcome := "pass"
	if failed {
		outcome = "fail"
	} else if killed {
		outcome = "interrupted"
	}
	for name := range p.active {
		p.finishTest(name, "interrupted", time.Now(), nil)
	}
	p.span.SetAttributes(attribute.String("test.outcome", outcome))
	if failed {
		p.span.SetStatus(codes.Error, "package failed")
	}
	p.span.End()
}

func (p *Package) finishTest(name, outcome string, ended time.Time, elapsed *float64) {
	test := p.active[name]
	if test == nil {
		return
	}
	delete(p.active, name)
	if !test.paused.IsZero() {
		test.wait += ended.Sub(test.paused)
	}
	ctx := context.WithValue(p.ctx, assignedIDKey{}, test.id)
	_, span := p.reporter.provider.Tracer("rask.test").Start(ctx, name,
		trace.WithTimestamp(test.started), trace.WithAttributes(
			attribute.String("test.package", p.name), attribute.String("test.name", name),
			attribute.String("test.outcome", outcome), attribute.Float64("test.paused_seconds", test.wait.Seconds()),
		))
	if elapsed != nil {
		span.SetAttributes(attribute.Float64("test.elapsed_seconds", *elapsed))
	} else {
		span.SetAttributes(attribute.Bool("test.terminal_event_missing", true))
	}
	if outcome == "fail" {
		span.SetStatus(codes.Error, "test failed")
	}
	span.End(trace.WithTimestamp(ended))
}

// A claim supplies identity only; the runner still owns timestamps and outcome.
type assignedIDKey struct{}

type assignedIDs struct{}

func (g assignedIDs) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	var id trace.TraceID
	for !id.IsValid() {
		if _, err := rand.Read(id[:]); err != nil {
			panic(err)
		}
	}
	return id, g.NewSpanID(ctx, id)
}

func (assignedIDs) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if id, ok := ctx.Value(assignedIDKey{}).(trace.SpanID); ok && id.IsValid() {
		return id
	}
	var id trace.SpanID
	for !id.IsValid() {
		if _, err := rand.Read(id[:]); err != nil {
			panic(err)
		}
	}
	return id
}

// Export failures must not alter test results or print backend response bodies.
type reportingExporter struct {
	sdktrace.SpanExporter
	failed atomic.Bool
}

func (e *reportingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := e.SpanExporter.ExportSpans(ctx, spans); err != nil {
		e.failed.Store(true)
	}
	return nil
}

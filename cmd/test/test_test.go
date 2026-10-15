package testcmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chronark/rask/cmd/clean"
	"github.com/chronark/rask/internal/cache"
	"github.com/chronark/rask/internal/cli"
	"github.com/chronark/rask/internal/fingerprint"
	"github.com/chronark/rask/internal/frontend"
	"github.com/chronark/rask/internal/gotool"
	"github.com/chronark/rask/internal/run"
	"github.com/chronark/rask/internal/setup"
)

// commandResult carries run completion across a watchdog channel.
type commandResult struct {
	code int
	err  error
}

// TestExecuteReturnsZeroForPassingRun protects Execute success exit-code behavior.
func TestExecuteReturnsZeroForPassingRun(t *testing.T) {
	module := moduleFixtureRoot(t)
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	passing := packageFixture(module, "passing")
	writePackageFiles(t, passing)
	tool := &fakeTool{module: module, all: []gotool.Package{passing}, streams: map[string]string{
		passing.ImportPath: `{"Action":"run","Test":"TestOK"}` + "\n" +
			`{"Action":"pass","Test":"TestOK"}` + "\n" +
			`{"Action":"pass","Elapsed":0.01}` + "\n",
	}}
	code, out, errOut := executeWithTool(t, tool, "test", "./...")
	if code != 0 {
		t.Fatalf("code=%d errOut=%q, want zero", code, errOut)
	}
	if strings.Contains(out, "Compiling") {
		t.Fatalf("out contains Compiling line:\n%s", out)
	}
	if strings.Contains(out, "test TestOK ... ok") {
		t.Fatalf("default output contains passing test line:\n%s", out)
	}
}

// TestExecuteRootRunsTests protects `rask [patterns]` as the default action.
func TestExecuteRootRunsTests(t *testing.T) {
	module := moduleFixtureRoot(t)
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	passing := packageFixture(module, "passing")
	writePackageFiles(t, passing)
	tool := &fakeTool{module: module, all: []gotool.Package{passing}, streams: map[string]string{
		passing.ImportPath: `{"Action":"pass","Elapsed":0.01}` + "\n",
	}}
	code, _, errOut := executeWithTool(t, tool, "./...")
	if code != 0 {
		t.Fatalf("code=%d errOut=%q, want zero", code, errOut)
	}
}

// TestExecuteVerbosePrintsPassingTests protects the -v rendering path.
func TestExecuteVerbosePrintsPassingTests(t *testing.T) {
	module := moduleFixtureRoot(t)
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	passing := packageFixture(module, "passing")
	writePackageFiles(t, passing)
	tool := &fakeTool{module: module, all: []gotool.Package{passing}, streams: map[string]string{
		passing.ImportPath: `{"Action":"run","Test":"TestOK"}` + "\n" +
			`{"Action":"pass","Test":"TestOK"}` + "\n" +
			`{"Action":"pass","Elapsed":0.01}` + "\n",
	}}
	code, out, errOut := executeWithTool(t, tool, "test", "-v", "./...")
	if code != 0 {
		t.Fatalf("code=%d errOut=%q, want zero", code, errOut)
	}
	if !strings.Contains(out, "test TestOK ... ok") {
		t.Fatalf("verbose output missing passing test line:\n%s", out)
	}
}

// TestExecuteReturnsOneForFlagError protects Execute error exit-code behavior.
func TestExecuteReturnsOneForFlagError(t *testing.T) {
	code, _, errOut := executeWithTool(t, &fakeTool{}, "-run", "TestA")
	if code != 1 {
		t.Fatalf("code=%d, want one", code)
	}
	if !strings.Contains(errOut, "unknown flag") {
		t.Fatalf("errOut=%q, want unknown flag error", errOut)
	}
	if !strings.Contains(errOut, "supports only -v/--verbose, --no-cache, and --concurrency") {
		t.Fatalf("errOut=%q, want supported-flag message", errOut)
	}
}

// TestExecuteRejectsNegativeConcurrency protects startup validation before work exists.
func TestExecuteRejectsNegativeConcurrency(t *testing.T) {
	code, _, errOut := executeWithTool(t, &fakeTool{}, "--concurrency", "-1")
	if code != 1 {
		t.Fatalf("code=%d, want one", code)
	}
	if !strings.Contains(errOut, "--concurrency must be positive") {
		t.Fatalf("errOut=%q, want concurrency validation", errOut)
	}
}

// TestExecuteRejectsConcurrencyAboveCap protects the process fan-out bound.
func TestExecuteRejectsConcurrencyAboveCap(t *testing.T) {
	code, _, errOut := executeWithTool(t, &fakeTool{}, "--concurrency=33")
	if code != 1 {
		t.Fatalf("code=%d, want one", code)
	}
	if !strings.Contains(errOut, "--concurrency must be at most 32") {
		t.Fatalf("errOut=%q, want cap validation", errOut)
	}
}

// TestExecuteAllowsZeroConcurrency protects zero as the automatic sizing sentinel.
func TestExecuteAllowsZeroConcurrency(t *testing.T) {
	module := moduleFixtureRoot(t)
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	passing := packageFixture(module, "passing")
	writePackageFiles(t, passing)
	tool := &fakeTool{module: module, all: []gotool.Package{passing}, streams: map[string]string{
		passing.ImportPath: `{"Action":"pass","Elapsed":0.01}` + "\n",
	}}
	code, _, errOut := executeWithTool(t, tool, "--concurrency=0", "./...")
	if code != 0 {
		t.Fatalf("code=%d errOut=%q, want zero", code, errOut)
	}
}

// TestExecuteExitsNonzeroForHashingErrors protects nonzero exits for fingerprint failures.
func TestExecuteExitsNonzeroForHashingErrors(t *testing.T) {
	module := moduleFixtureRoot(t)
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	broken := packageFixture(module, "broken")
	tool := &fakeTool{module: module, all: []gotool.Package{broken}}
	code, _, errOut := executeWithTool(t, tool, "test", "./...")
	if code == 0 {
		t.Fatalf("code = 0, want nonzero")
	}
	if !strings.Contains(errOut, "hashing") {
		t.Fatalf("errOut = %q, want hashing error", errOut)
	}
}

// TestExecuteFingerprinterErrorCleanShutdown protects stream-end service errors.
func TestExecuteFingerprinterErrorCleanShutdown(t *testing.T) {
	module := moduleFixtureRoot(t)
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	left := packageFixture(module, "left")
	right := packageFixture(module, "right")
	left.Imports = []string{right.ImportPath}
	right.Imports = []string{left.ImportPath}
	writePackageFiles(t, left)
	writePackageFiles(t, right)
	tool := &fakeTool{module: module, all: []gotool.Package{left, right}}
	code, out, errOut := executeWithTool(t, tool, "test", "./...")
	if code == 0 {
		t.Fatalf("code = 0, want nonzero")
	}
	if !strings.Contains(errOut, "unresolved discovered imports") {
		t.Fatalf("errOut = %q, want unresolved import error", errOut)
	}
	if strings.Contains(out+errOut, "panic") {
		t.Fatalf("output contains panic:\n%s%s", out, errOut)
	}
}

// TestFailFastWithOneWorkerSkipsLaterSuites protects fail-fast counting.
func TestFailFastWithOneWorkerSkipsLaterSuites(t *testing.T) {
	module := moduleFixtureRoot(t)
	suites := manySuites(module, 2)
	executor := &fakeExecutor{results: failingResults(suites)}
	command := workerCommand(module, &memorySuiteCache{}, executor, make(chan frontend.Event, 8))
	command.concurrency = 1
	outcomes := command.runSuites(suites)
	summary := command.foldSummary(suites, outcomes)
	if len(summary.FailedSuites) != 1 || summary.Skipped != 1 || executor.totalCalls() != 1 {
		t.Fatalf("summary=%+v, want one failed and one skipped suite", summary)
	}
}

// TestCachedHitCountsAndEmitsEvent protects cached summary and rendering events.
func TestCachedHitCountsAndEmitsEvent(t *testing.T) {
	module := moduleFixtureRoot(t)
	suite := manySuites(module, 1)[0]
	entry := cache.Entry{ImportPath: suite.Package.ImportPath, Passed: 2, Ignored: 1}
	resultCache := newRecordingSuiteCache(suite.Digest, entry)
	events := make(chan frontend.Event, eventChannelCapacity)
	command := workerCommand(module, resultCache, &fakeExecutor{}, events)
	outcomes := command.runSuites([]fingerprint.Suite{suite})
	summary := command.foldSummary([]fingerprint.Suite{suite}, outcomes)
	if summary.Cached != 1 || summary.Passed != 2 || summary.Ignored != 1 {
		t.Fatalf("summary=%+v, want cached counts", summary)
	}
	if !drainHasCachedEvent(events) {
		t.Fatalf("cached path emitted no cached event")
	}
}

// TestNoCacheBypassesHitsAndRefreshesPasses protects docker-style no-cache semantics.
func TestNoCacheBypassesHitsAndRefreshesPasses(t *testing.T) {
	module := moduleFixtureRoot(t)
	suite := manySuites(module, 1)[0]
	entry := cache.Entry{ImportPath: suite.Package.ImportPath, Passed: 1, CreatedAt: time.Unix(1, 0)}
	resultCache := newRecordingSuiteCache(suite.Digest, entry)
	executor := &fakeExecutor{}
	events := make(chan frontend.Event, eventChannelCapacity)
	command := workerCommand(module, resultCache, executor, events)
	command.noCache = true
	command.runSuites([]fingerprint.Suite{suite})
	if resultCache.gets != 0 || resultCache.puts != 1 || executor.callsFor(suite.Digest) != 1 {
		t.Fatalf("gets=%d puts=%d executes=%d, want bypass read and refresh",
			resultCache.gets, resultCache.puts, executor.callsFor(suite.Digest))
	}
	if resultCache.entries[suite.Digest].CreatedAt.Equal(entry.CreatedAt) {
		t.Fatalf("cache entry was not refreshed: %+v", resultCache.entries[suite.Digest])
	}
	if drainHasCachedEvent(events) {
		t.Fatalf("no-cache path emitted cached event")
	}
}

// TestWorkerSemaphoreBoundsConcurrentExecutes protects --concurrency fan-out.
func TestWorkerSemaphoreBoundsConcurrentExecutes(t *testing.T) {
	module := moduleFixtureRoot(t)
	suites := manySuites(module, 8)
	release := make(chan struct{})
	executor := &fakeExecutor{release: release}
	command := workerCommand(module, &memorySuiteCache{}, executor, make(chan frontend.Event, 32))
	command.concurrency = 3
	done := make(chan []suiteOutcome, 1)
	go func() {
		done <- command.runSuites(suites)
	}()
	waitForActiveExecutes(t, executor, 3)
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("workers did not finish")
	}
	if got := executor.maxActive.Load(); got > 3 {
		t.Fatalf("max active executes=%d, want at most 3", got)
	}
}

// TestConcurrentCacheLookupsOverlap protects worker-per-suite cache parallelism.
func TestConcurrentCacheLookupsOverlap(t *testing.T) {
	module := moduleFixtureRoot(t)
	suites := manySuites(module, 3)
	resultCache := newConcurrentLookupCache(2, suites)
	command := workerCommand(module, resultCache, &fakeExecutor{}, make(chan frontend.Event, 16))
	command.concurrency = 2
	done := make(chan []suiteOutcome, 1)
	go func() {
		done <- command.runSuites(suites)
	}()
	select {
	case <-resultCache.gate:
	case <-time.After(2 * time.Second):
		t.Fatalf("cache lookups did not overlap")
	}
	select {
	case outcomes := <-done:
		summary := command.foldSummary(suites, outcomes)
		if summary.Cached != len(suites) {
			t.Fatalf("summary=%+v, want all suites cached", summary)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("workers did not finish")
	}
}

// TestInterruptCancelsBlockedCacheLookups protects Ctrl+C exit latency.
func TestInterruptCancelsBlockedCacheLookups(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	module := moduleFixtureRoot(t)
	packages := make([]gotool.Package, 0, 8)
	for _, suite := range manySuites(module, 8) {
		writePackageFiles(t, suite.Package)
		packages = append(packages, suite.Package)
	}
	out := new(strings.Builder)
	errOut := new(strings.Builder)
	ui, events := frontendForTest(out, errOut)
	resultCache := &interruptLookupCache{started: make(chan struct{})}
	command := newTestCommand(ctx, &fakeTool{module: module, all: packages}, module, module.Dir, resultCache, testArguments{
		patterns: []string{"./..."},
	}, ui, events)
	done := make(chan commandResult, 1)
	go func() {
		code, err := command.run()
		done <- commandResult{code: code, err: err}
	}()
	select {
	case <-resultCache.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("cache lookup did not start")
	}
	startedAt := time.Now()
	cancel()
	select {
	case result := <-done:
		if result.err != nil || result.code != 130 {
			t.Fatalf("code=%d err=%v, want interrupted code", result.code, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("interrupted run did not return promptly")
	}
	if elapsed := time.Since(startedAt); elapsed > 5*time.Second {
		t.Fatalf("interrupt latency=%s, want under 5s", elapsed)
	}
	if !strings.Contains(errOut.String(), "interrupted;") {
		t.Fatalf("errOut=%q, want interrupted summary", errOut.String())
	}
}

// TestSendRunPlanUsesShortenedPathWidth protects frontend column planning.
func TestSendRunPlanUsesShortenedPathWidth(t *testing.T) {
	module := moduleFixtureRoot(t)
	events := make(chan frontend.Event, 1)
	command := &testCommand{module: module, events: events}
	suites := []fingerprint.Suite{
		{Package: gotool.Package{ImportPath: module.Path}},
		{Package: gotool.Package{ImportPath: module.Path + "/internal/deep/pkg"}},
	}
	command.sendRunPlan(suites)
	event := <-events
	want := len([]rune("internal/deep/pkg"))
	if event.RunPlan.MaxImportPathWidth != want {
		t.Fatalf("path width = %d, want shortened width %d",
			event.RunPlan.MaxImportPathWidth, want)
	}
}

// TestCommandRecordsPruneFailureAsCacheNote protects prune non-fatality.
func TestCommandRecordsPruneFailureAsCacheNote(t *testing.T) {
	module := moduleFixtureRoot(t)
	passing := packageFixture(module, "passing")
	writePackageFiles(t, passing)
	tool := &fakeTool{module: module, all: []gotool.Package{passing}, streams: map[string]string{
		passing.ImportPath: `{"Action":"pass","Elapsed":0.01}` + "\n",
	}}
	out := new(strings.Builder)
	errOut := new(strings.Builder)
	ui, events := frontendForTest(out, errOut)
	command := newTestCommand(
		context.Background(), tool, module, module.Dir, &failingPruneCache{}, testArguments{
			patterns: []string{"./..."},
		}, ui, events)
	code, err := command.run()
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v errOut=%q, want passing command", code, err, errOut.String())
	}
	if !strings.Contains(out.String(), "note: 1 cache errors") {
		t.Fatalf("out=%q, want cache note", out.String())
	}
}

// TestCommandRootCancellationRendersInterrupted protects first-signal shutdown behavior.
func TestCommandRootCancellationRendersInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	module := moduleFixtureRoot(t)
	passing := packageFixture(module, "passing")
	writePackageFiles(t, passing)
	out := new(strings.Builder)
	errOut := new(strings.Builder)
	ui, events := frontendForTest(out, errOut)
	command := newTestCommand(
		ctx, &fakeTool{module: module, all: []gotool.Package{passing}}, module, module.Dir,
		&memorySuiteCache{}, testArguments{
			patterns: []string{"./..."},
		}, ui, events)
	code, err := command.run()
	if err != nil || code != 130 {
		t.Fatalf("code=%d err=%v, want interrupted code", code, err)
	}
	if !strings.Contains(errOut.String(), "interrupted;") || !strings.Contains(errOut.String(), "skipped") {
		t.Fatalf("errOut=%q, want interruption summary", errOut.String())
	}
}

// TestOnlyPassingSuitesAreCachedAndFailuresDelete protects the cache-write policy.
func TestOnlyPassingSuitesAreCachedAndFailuresDelete(t *testing.T) {
	resultCache := newRecordingSuiteCache(strings.Repeat("c", 64), cache.Entry{
		ImportPath: "example.com/m/pass", Passed: 1,
	})
	passing := suiteTaskForTest("example.com/m/pass")
	command := &testCommand{ctx: context.Background(), resultCache: resultCache, cancelRun: func() {}}
	command.recordRunOutcome(run.SuiteResult{
		Package: passing.Package, Digest: passing.Digest, Completed: true,
		Tests: []run.TestResult{{Name: "TestOK", Outcome: run.TestPassed}},
	})
	if _, found := resultCache.Get(context.Background(), passing.Digest); !found {
		t.Fatalf("passing suite must be cached")
	}
	command.recordRunOutcome(run.SuiteResult{
		Package: passing.Package, Digest: passing.Digest, Completed: true, Failed: true,
		Tests: []run.TestResult{{Name: "TestBad", Outcome: run.TestFailed}},
	})
	if _, found := resultCache.Get(context.Background(), passing.Digest); found {
		t.Fatalf("failed suite must delete stale cache entry")
	}
}

// TestCacheWriteFailureIncrementsSummary protects cache write diagnostics.
func TestCacheWriteFailureIncrementsSummary(t *testing.T) {
	module := moduleFixtureRoot(t)
	suite := manySuites(module, 1)[0]
	resultCache := &recordingSuiteCache{
		entries: map[string]cache.Entry{},
		putErr:  fmt.Errorf("put failed"),
	}
	command := workerCommand(module, resultCache, &fakeExecutor{}, make(chan frontend.Event, 8))
	outcomes := command.runSuites([]fingerprint.Suite{suite})
	summary := command.foldSummary([]fingerprint.Suite{suite}, outcomes)
	if summary.CacheErrors != 1 {
		t.Fatalf("summary=%+v, want one cache error", summary)
	}
}

// TestSummaryIncludesRemoteCounters protects remote stats rendering inputs.
func TestSummaryIncludesRemoteCounters(t *testing.T) {
	module := moduleFixtureRoot(t)
	suite := manySuites(module, 1)[0]
	resultCache := &recordingSuiteCache{
		entries: map[string]cache.Entry{},
		remote:  true,
		remoteStats: cache.RemoteStats{
			Reads: 3, Writes: 2,
		},
	}
	command := workerCommand(module, resultCache, &fakeExecutor{}, make(chan frontend.Event, 8))
	summary := command.foldSummary([]fingerprint.Suite{suite}, []suiteOutcome{{kind: outcomeCached}})
	if !summary.RemoteConfigured || summary.RemoteReads != 3 || summary.RemoteWrites != 2 || summary.RemoteCached != 3 {
		t.Fatalf("summary=%+v, want remote counters", summary)
	}
}

// TestExecuteUsesRemoteTierEndToEnd protects command wiring to shared remote cache.
func TestExecuteUsesRemoteTierEndToEnd(t *testing.T) {
	module := moduleFixtureRoot(t)
	passing := packageFixture(module, "passing")
	writePackageFiles(t, passing)
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	t.Setenv("RASK_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("RASK_REMOTE_CACHE_BUCKET", "bucket")
	t.Setenv("RASK_REMOTE_CACHE_PREFIX", "prefix")
	t.Setenv("RASK_REMOTE_CACHE_ACCESS_KEY_ID", "test-access")
	t.Setenv("RASK_REMOTE_CACHE_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("RASK_REMOTE_CACHE_REGION", "auto")
	t.Setenv("RASK_REMOTE_CACHE_ENDPOINT", "http://rask.test")
	oldTransport := http.DefaultTransport
	http.DefaultTransport = cmdHandlerTransport{handler: newCommandS3Handler(t)}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	firstTool := &fakeTool{module: module, all: []gotool.Package{passing}, streams: map[string]string{
		passing.ImportPath: `{"Action":"run","Test":"TestOK"}` + "\n" +
			`{"Action":"pass","Test":"TestOK"}` + "\n" +
			`{"Action":"pass","Elapsed":0.01}` + "\n",
	}}
	code, _, errOut := executeWithTool(t, firstTool, "test", "./...")
	if code != 0 {
		t.Fatalf("first code=%d errOut=%q, want success", code, errOut)
	}
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	secondTool := &fakeTool{module: module, all: []gotool.Package{passing}}
	code, out, errOut := executeWithTool(t, secondTool, "test", "./...")
	if code != 0 || !strings.Contains(out, "Cached passing") {
		t.Fatalf("second code=%d out=%q errOut=%q, want remote cache hit", code, out, errOut)
	}
	if secondTool.startedCount(passing.ImportPath) != 0 {
		t.Fatalf("second run started %d suites, want remote cache hit", secondTool.startedCount(passing.ImportPath))
	}
}

// manySuites returns count selected package suites with unique digests.
func manySuites(module gotool.Module, count int) []fingerprint.Suite {
	suites := make([]fingerprint.Suite, 0, count)
	for index := range count {
		name := fmt.Sprintf("pkg%02d", index)
		pkg := packageFixture(module, name)
		suites = append(suites, fingerprint.Suite{Package: pkg, Digest: fmt.Sprintf("%064x", index+1)})
	}
	return suites
}

// failingResults returns one failed runner result per suite digest.
func failingResults(suites []fingerprint.Suite) map[string]run.SuiteResult {
	results := make(map[string]run.SuiteResult, len(suites))
	for _, suite := range suites {
		results[suite.Digest] = run.SuiteResult{
			Failed: true,
			Tests:  []run.TestResult{{Name: "TestBad", Outcome: run.TestFailed}},
		}
	}
	return results
}

// workerCommand returns a command ready for direct worker-model tests.
func workerCommand(
	module gotool.Module,
	resultCache suiteCache,
	executor suiteExecutor,
	events chan frontend.Event,
) *testCommand {
	return &testCommand{
		ctx: context.Background(), module: module, resultCache: resultCache,
		executor: executor, events: events, startedAt: time.Now(),
	}
}

// waitForActiveExecutes waits until a fake executor observes active workers.
func waitForActiveExecutes(t *testing.T, executor *fakeExecutor, want int32) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if executor.active.Load() == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("active executes=%d, want %d", executor.active.Load(), want)
		case <-tick.C:
		}
	}
}

// drainHasCachedEvent reports whether buffered frontend events include a cache hit.
func drainHasCachedEvent(events <-chan frontend.Event) bool {
	for len(events) > 0 {
		event := <-events
		if event.Kind == frontend.EventCached {
			return true
		}
	}
	return false
}

// executeWithTool runs Execute with a fake command environment.
func executeWithTool(t *testing.T, tool gotool.Tool, arguments ...string) (int, string, string) {
	t.Helper()
	t.Setenv("RASK_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	oldEnvironment := setup.BuildEnvironment
	out := new(strings.Builder)
	errOut := new(strings.Builder)
	setup.BuildEnvironment = func() *setup.Environment {
		events := make(chan frontend.Event, eventChannelCapacity)
		return &setup.Environment{
			Tool: tool, Stdout: out, Stderr: errOut,
			Frontend: frontend.New(out, errOut, false, events),
			Events:   events,
		}
	}
	t.Cleanup(func() {
		setup.BuildEnvironment = oldEnvironment
	})
	code := setup.Execute(rootCommandForTest(), append([]string{"rask"}, arguments...))
	return code, out.String(), errOut.String()
}

// rootCommandForTest returns the command tree used by Execute tests.
func rootCommandForTest() *cli.Command {
	testCommand := Command()
	return &cli.Command{
		Name:               "rask",
		Usage:              "Run Go tests with content-addressed result caching",
		Description:        "Testing takes package patterns plus -v/--verbose, --no-cache, and --concurrency.",
		Flags:              testCommand.Flags,
		Action:             testCommand.Action,
		AcceptsArgs:        true,
		DefaultActionNames: []string{"test", "t"},
		UnknownFlagHint:    "rask supports only -v/--verbose, --no-cache, and --concurrency; use `go test` for other flags",
		Commands: []*cli.Command{
			clean.Command(),
		},
	}
}

// newCommandS3Handler returns an in-memory S3-shaped cache endpoint.
func newCommandS3Handler(t *testing.T) http.Handler {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case http.MethodGet:
			mu.Lock()
			body, found := objects[key]
			mu.Unlock()
			if !found {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(append([]byte(nil), body...))
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			objects[key] = append([]byte(nil), body...)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

// cmdHandlerTransport routes HTTP clients into a handler without opening a port.
type cmdHandlerTransport struct {
	handler http.Handler
}

// RoundTrip serves req through the configured handler and returns its response.
func (t cmdHandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	t.handler.ServeHTTP(response, req)
	return response.Result(), nil
}

// frontendForTest returns a non-terminal frontend and its event channel.
func frontendForTest(out io.Writer, errOut io.Writer) (*frontend.Frontend, chan frontend.Event) {
	events := make(chan frontend.Event, eventChannelCapacity)
	return frontend.New(out, errOut, false, events), events
}

// moduleFixtureRoot creates a minimal module directory for orchestration tests.
func moduleFixtureRoot(t *testing.T) gotool.Module {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.26\n")
	return gotool.Module{Path: "example.com/m", Dir: dir}
}

// packageFixture returns a selected package with one source and test file.
func packageFixture(module gotool.Module, name string) gotool.Package {
	dir := filepath.Join(module.Dir, name)
	return gotool.Package{
		Dir: dir, ImportPath: module.Path + "/" + name, Name: name,
		GoFiles: []string{name + ".go"}, TestGoFiles: []string{name + "_test.go"},
		Selected: true,
	}
}

// writeFile creates a test file and fails the test on filesystem errors.
func writeFile(t *testing.T, dir string, name string, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// writePackageFiles creates the source files described by a package fixture.
func writePackageFiles(t *testing.T, p gotool.Package) {
	t.Helper()
	writeFile(t, p.Dir, p.GoFiles[0], "package "+p.Name+"\n")
	writeFile(t, p.Dir, p.TestGoFiles[0], "package "+p.Name+"\n")
}

// suiteTaskForTest returns a runner suite with a valid cache-shaped digest.
func suiteTaskForTest(importPath string) run.SuiteTask {
	return run.SuiteTask{
		Package: gotool.Package{Dir: "/tmp", ImportPath: importPath, TestGoFiles: []string{"x_test.go"}},
		Digest:  strings.Repeat("c", 64),
	}
}

// fakeTool provides the gotool seam for command orchestration tests.
type fakeTool struct {
	mu          sync.Mutex
	module      gotool.Module
	selected    []gotool.Package
	all         []gotool.Package
	allByDir    map[string][]gotool.Package
	modules     []gotool.Module
	environment []byte
	streams     map[string]string
	started     map[string]int
	listed      map[string][]string
}

// CurrentModule returns the configured module fixture.
func (f *fakeTool) CurrentModule(context.Context) (gotool.Module, error) {
	return f.module, nil
}

// ListPackages returns either the full closure or selected fixture packages.
func (f *fakeTool) ListPackages(_ context.Context, dir string, patterns []string) ([]gotool.Package, error) {
	if len(patterns) == 1 && patterns[0] == "./..." {
		if f.allByDir != nil {
			return f.allByDir[dir], nil
		}
		return f.all, nil
	}
	return f.selected, nil
}

func (f *fakeTool) ListDependencies(context.Context, string, []string) ([]gotool.Package, error) {
	packages := append([]gotool.Package(nil), f.all...)
	for _, local := range f.allByDir {
		packages = append(packages, local...)
	}
	return packages, nil
}

// ListModules returns configured workspace modules for command tests.
func (f *fakeTool) ListModules(context.Context, string) ([]gotool.Module, error) {
	return f.modules, nil
}

// Environment returns a stable fake Go environment.
func (f *fakeTool) Environment(context.Context, string) ([]byte, error) {
	if f.environment != nil {
		return f.environment, nil
	}
	return []byte("GOARCH='amd64'\nGOOS='linux'\nGOVERSION='go1.26'\nGOWORK=''\n"), nil
}

// ListTests returns configured names or an empty successful listing.
func (f *fakeTool) ListTests(ctx context.Context, _ string, importPath string) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return append([]string(nil), f.listed[importPath]...), nil
}

// StartTests returns a fake process stream for orchestration tests.
func (f *fakeTool) StartTests(ctx context.Context, _ string, importPath string) (gotool.TestProcess, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	stream, ok := f.streams[importPath]
	if !ok {
		return nil, fmt.Errorf("unexpected StartTests call for %s", importPath)
	}
	f.mu.Lock()
	if f.started == nil {
		f.started = make(map[string]int)
	}
	f.started[importPath]++
	f.mu.Unlock()
	return fakeTestProcess{events: strings.NewReader(stream)}, nil
}

// startedCount returns a race-safe process start count for importPath.
func (f *fakeTool) startedCount(importPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[importPath]
}

// fakeTestProcess implements gotool.TestProcess for command orchestration tests.
type fakeTestProcess struct {
	events io.Reader
}

// Events returns the configured JSON event stream.
func (p fakeTestProcess) Events() io.Reader {
	return p.events
}

// Wait returns success for the fake process.
func (p fakeTestProcess) Wait() error {
	return nil
}

// Stderr returns no diagnostic output for the fake process.
func (p fakeTestProcess) Stderr() string {
	return ""
}

// fakeExecutor gives worker tests a deterministic per-suite runner.
type fakeExecutor struct {
	mu        sync.Mutex
	results   map[string]run.SuiteResult
	calls     map[string]int
	release   <-chan struct{}
	active    atomic.Int32
	maxActive atomic.Int32
}

// Execute records concurrency and returns a configured or passing result.
func (e *fakeExecutor) Execute(ctx context.Context, task run.SuiteTask, hooks run.Hooks) run.SuiteResult {
	if hooks.Started != nil {
		hooks.Started()
	}
	e.recordCall(task.Digest)
	active := e.active.Add(1)
	e.recordMax(active)
	defer e.active.Add(-1)
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return run.SuiteResult{Package: task.Package, Digest: task.Digest, Killed: true}
		}
	}
	if result, ok := e.resultFor(task.Digest); ok {
		result.Package = task.Package
		result.Digest = task.Digest
		return result
	}
	return run.SuiteResult{
		Package: task.Package, Digest: task.Digest, Completed: true,
		Tests: []run.TestResult{{Name: "TestOK", Outcome: run.TestPassed}},
	}
}

// recordCall counts one execution for digest under the fake's lock.
func (e *fakeExecutor) recordCall(digest string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.calls == nil {
		e.calls = make(map[string]int)
	}
	e.calls[digest]++
}

// recordMax raises the observed concurrency high-water mark.
func (e *fakeExecutor) recordMax(active int32) {
	for {
		current := e.maxActive.Load()
		if active <= current || e.maxActive.CompareAndSwap(current, active) {
			return
		}
	}
}

// resultFor returns a configured result for digest.
func (e *fakeExecutor) resultFor(digest string) (run.SuiteResult, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	result, ok := e.results[digest]
	return result, ok
}

// callsFor returns how many times digest executed.
func (e *fakeExecutor) callsFor(digest string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[digest]
}

// totalCalls returns all executions observed by the fake.
func (e *fakeExecutor) totalCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := 0
	for _, calls := range e.calls {
		total += calls
	}
	return total
}

// memorySuiteCache is an in-memory no-op cache for command cancellation tests.
type memorySuiteCache struct{}

// Get reports a cache miss.
func (c *memorySuiteCache) Get(context.Context, string) (cache.Entry, bool) {
	return cache.Entry{}, false
}

// Put accepts a cache write.
func (c *memorySuiteCache) Put(context.Context, string, cache.Entry) error {
	return nil
}

// Delete accepts a cache delete.
func (c *memorySuiteCache) Delete(context.Context, string) {}

// Prune succeeds because memory tests have no disk retention.
func (c *memorySuiteCache) Prune() error {
	return nil
}

// HasRemoteTier reports that the memory cache is local-only.
func (c *memorySuiteCache) HasRemoteTier() bool {
	return false
}

// RemoteStats returns no remote traffic for the memory cache.
func (c *memorySuiteCache) RemoteStats() cache.RemoteStats {
	return cache.RemoteStats{}
}

// failingPruneCache simulates a final cache prune failure.
type failingPruneCache struct {
	memorySuiteCache
}

// Prune returns the configured prune failure.
func (c *failingPruneCache) Prune() error {
	return fmt.Errorf("prune failed")
}

// recordingSuiteCache tracks cache operations for orchestration tests.
type recordingSuiteCache struct {
	entries     map[string]cache.Entry
	remoteStats cache.RemoteStats
	remote      bool
	putErr      error
	gets        int
	puts        int
	deletes     int
}

// newRecordingSuiteCache returns a cache seeded with one digest entry.
func newRecordingSuiteCache(digest string, entry cache.Entry) *recordingSuiteCache {
	return &recordingSuiteCache{entries: map[string]cache.Entry{digest: entry}}
}

// Get records a cache lookup and returns any seeded entry.
func (c *recordingSuiteCache) Get(_ context.Context, digest string) (cache.Entry, bool) {
	c.gets++
	entry, found := c.entries[digest]
	return entry, found
}

// Put records a cache write and refreshes the digest entry.
func (c *recordingSuiteCache) Put(_ context.Context, digest string, entry cache.Entry) error {
	c.puts++
	if c.putErr != nil {
		return c.putErr
	}
	c.entries[digest] = entry
	return nil
}

// Delete records a cache delete and removes the digest entry.
func (c *recordingSuiteCache) Delete(_ context.Context, digest string) {
	c.deletes++
	delete(c.entries, digest)
}

// Prune succeeds because tests assert write behavior separately.
func (c *recordingSuiteCache) Prune() error {
	return nil
}

// HasRemoteTier reports whether the recording cache has a remote tier.
func (c *recordingSuiteCache) HasRemoteTier() bool {
	return c.remote
}

// RemoteStats returns the configured remote traffic snapshot.
func (c *recordingSuiteCache) RemoteStats() cache.RemoteStats {
	return c.remoteStats
}

// concurrentLookupCache blocks until several lookups overlap.
type concurrentLookupCache struct {
	memorySuiteCache
	entries map[string]cache.Entry
	delays  map[string]time.Duration
	gate    chan struct{}
	once    sync.Once
	need    int32
	seen    atomic.Int32
}

// newConcurrentLookupCache returns hits with earlier suites resolving later.
func newConcurrentLookupCache(need int, suites []fingerprint.Suite) *concurrentLookupCache {
	entries := make(map[string]cache.Entry, len(suites))
	delays := make(map[string]time.Duration, len(suites))
	for index, suite := range suites {
		entries[suite.Digest] = cache.Entry{ImportPath: suite.Package.ImportPath, Passed: 1}
		delays[suite.Digest] = time.Duration(len(suites)-index) * 10 * time.Millisecond
	}
	return &concurrentLookupCache{entries: entries, delays: delays, gate: make(chan struct{}), need: int32(need)}
}

// Get proves lookup overlap by opening the gate only after enough callers wait.
func (c *concurrentLookupCache) Get(ctx context.Context, digest string) (cache.Entry, bool) {
	if c.seen.Add(1) >= c.need {
		c.once.Do(func() { close(c.gate) })
	}
	select {
	case <-c.gate:
	case <-ctx.Done():
		return cache.Entry{}, false
	}
	time.Sleep(c.delays[digest])
	entry, found := c.entries[digest]
	return entry, found
}

// interruptLookupCache waits on ctx to prove cache lookup cancellation is threaded.
type interruptLookupCache struct {
	memorySuiteCache
	started chan struct{}
	once    sync.Once
}

// Get blocks like a remote request and returns a miss once ctx is cancelled.
func (c *interruptLookupCache) Get(ctx context.Context, _ string) (cache.Entry, bool) {
	c.once.Do(func() { close(c.started) })
	<-ctx.Done()
	return cache.Entry{}, false
}

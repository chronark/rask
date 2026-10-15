package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chronark/rask/internal/gotool"
)

// TestDefaultWorkerCountProtectsHostCapacity pins the caller-facing concurrency default.
func TestDefaultWorkerCountProtectsHostCapacity(t *testing.T) {
	old := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
	if got := DefaultWorkerCount(); got != 1 {
		t.Fatalf("DefaultWorkerCount with two CPUs = %d, want 1", got)
	}
	runtime.GOMAXPROCS(128)
	if got := DefaultWorkerCount(); got != WorkerCountMax {
		t.Fatalf("DefaultWorkerCount with wide machine = %d, want %d", got, WorkerCountMax)
	}
}

// TestReadEventsHandlesStreamLimitsAndNonTerminalTests protects stream draining after event limits.
func TestReadEventsHandlesStreamLimitsAndNonTerminalTests(t *testing.T) {
	suite := suiteForTest("example.com/m/pkg")
	reader := &chunkReader{chunks: [][]byte{
		[]byte(`{"Action":"run","Test":"TestA"}` + "\n"),
		[]byte(`{"Action":"run","Test":"TestB"}` + "\n"),
		[]byte(`{"Action":"fail","Elapsed":0.01}` + "\n"),
	}}
	state := newSuiteState(suite, nil)
	readEvents(reader, state, 1, func() {})
	if reader.index != len(reader.chunks) {
		t.Fatalf("reader stopped at %d, want drained", reader.index)
	}
	finishSuiteResult(context.Background(), state, nil, "")
	result := state.result()
	if !result.Failed || len(result.Tests) != 1 || result.Tests[0].Outcome != TestFailed {
		t.Fatalf("result = %+v, want failed limited suite", result)
	}
}

// TestExecuteFoldsJSONAndOutputBounds protects test outcomes and memory caps.
func TestExecuteFoldsJSONAndOutputBounds(t *testing.T) {
	suite := suiteForTest("example.com/m/folding")
	tool := &fakeTool{streams: map[string]string{suite.Package.ImportPath: foldingStream()}}
	result := newExecutor(tool, "/tmp", time.Second).Execute(context.Background(), suite, Hooks{})
	if !result.Failed || !result.Completed || result.ElapsedSeconds != 0.25 {
		t.Fatalf("result = %+v, want failed completed suite", result)
	}
	wantOutcomes(t, result.Tests, map[string]TestOutcome{
		"TestPass": TestPassed, "TestFail": TestFailed, "TestSkip": TestIgnored,
	})
	failed := wantTestResult(t, result.Tests, "TestFail")
	if len(failed.Output) != outputLinesPerTestKept || failed.DroppedOutput != 3 {
		t.Fatalf("failed output kept=%d dropped=%d, want kept=%d dropped=3",
			len(failed.Output), failed.DroppedOutput, outputLinesPerTestKept)
	}
}

// TestExecuteLongLineFailsWithoutPanic protects scanner-bound handling.
func TestExecuteLongLineFailsWithoutPanic(t *testing.T) {
	longLine := strings.Repeat("x", lineBytesMax+1) + "\n"
	suite := suiteForTest("example.com/m/pkg")
	tool := &fakeTool{streams: map[string]string{suite.Package.ImportPath: longLine}}
	result := newExecutor(tool, "/tmp", time.Second).Execute(context.Background(), suite, Hooks{})
	if !result.Failed || result.Err == "" || !containsLine(result.PackageOutput, result.Err+"\n") {
		t.Fatalf("result = %+v, want long-line diagnostic in package output", result)
	}
}

// TestExecuteStartFailureFoldsDiagnosticPackageOutput protects start errors as suite results.
func TestExecuteStartFailureFoldsDiagnosticPackageOutput(t *testing.T) {
	suite := suiteForTest("example.com/m/start")
	tool := &fakeTool{startErrs: map[string]error{suite.Package.ImportPath: fmt.Errorf("start exploded")}}
	result := newExecutor(tool, "/tmp", time.Second).Execute(context.Background(), suite, Hooks{})
	if !result.Failed || result.Err != "start exploded" || result.PackageOutput[0] != "start exploded\n" {
		t.Fatalf("result = %+v, want failed suite with start diagnostic", result)
	}
}

// TestExecuteWatchdogKillsSilentChild protects the per-suite stall watchdog.
func TestExecuteWatchdogKillsSilentChild(t *testing.T) {
	suite := suiteForTest("example.com/m/stalled")
	tool := &fakeTool{readers: map[string]readerFactory{
		suite.Package.ImportPath: func(ctx context.Context) io.Reader {
			return &blockingReader{ctx: ctx, chunks: [][]byte{
				[]byte(`{"Action":"run","Test":"TestSlow"}` + "\n"),
			}}
		},
	}}
	started := time.Now()
	result := newExecutor(tool, "/tmp", 20*time.Millisecond).Execute(context.Background(), suite, Hooks{})
	if time.Since(started) > time.Second {
		t.Fatalf("watchdog took too long to kill stalled suite")
	}
	// A stall must surface as a loud failure, never as Killed: callers fold
	// Killed into "silently skipped", which would hide the hang.
	if !result.Failed || result.Killed || !strings.Contains(result.Err, "suite produced no output for 20ms") {
		t.Fatalf("result = %+v, want loud stall failure", result)
	}
}

// TestExecuteWatchdogResetsOnActivity protects long but active child streams.
func TestExecuteWatchdogResetsOnActivity(t *testing.T) {
	suite := suiteForTest("example.com/m/active")
	tool := &fakeTool{readers: map[string]readerFactory{
		suite.Package.ImportPath: func(ctx context.Context) io.Reader {
			return steadyReader(ctx, 10*time.Millisecond, activeStreamLines())
		},
	}}
	result := newExecutor(tool, "/tmp", 30*time.Millisecond).Execute(context.Background(), suite, Hooks{})
	if result.Failed || result.Killed || strings.Contains(result.Err, "child process stalled") {
		t.Fatalf("result = %+v, want active suite to pass", result)
	}
}

// TestExecuteCancellationIsKilledNotFailed protects fail-fast result accounting.
func TestExecuteCancellationIsKilledNotFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	suite := suiteForTest("example.com/m/canceled")
	tool := &fakeTool{readers: map[string]readerFactory{
		suite.Package.ImportPath: func(ctx context.Context) io.Reader {
			return &blockingReader{ctx: ctx}
		},
	}}
	started := make(chan struct{})
	resultCh := make(chan SuiteResult, 1)
	go func() {
		hooks := Hooks{Started: func() { close(started) }}
		resultCh <- newExecutor(tool, "/tmp", time.Second).Execute(ctx, suite, hooks)
	}()
	<-started
	cancel()
	wantCanceledSuite(t, resultCh)
}

func TestExecuteEmitsProgressWithoutListing(t *testing.T) {
	suite := suiteForTest("example.com/m/progress")
	tool := &fakeTool{
		streams: map[string]string{suite.Package.ImportPath: twoTestProgressStream()},
	}
	progress := collectProgress(context.Background(), t, tool, suite)
	wantProgress(t, progress, []Progress{
		{Completed: 1},
		{Completed: 2},
	})
}

// TestExecuteProgressCountsOnlyTopLevelCompletions protects x/y unit matching.
func TestExecuteProgressCountsOnlyTopLevelCompletions(t *testing.T) {
	suite := suiteForTest("example.com/m/subtests")
	tool := &fakeTool{
		streams: map[string]string{suite.Package.ImportPath: subtestProgressStream()},
	}
	progress := collectProgress(context.Background(), t, tool, suite)
	wantProgress(t, progress, []Progress{{Completed: 1}})
}

func TestExecuteProgressExcludesExamplesAndFuzz(t *testing.T) {
	suite := suiteForTest("example.com/m/examples-fuzz")
	tool := &fakeTool{
		streams: map[string]string{suite.Package.ImportPath: unlistedProgressStream()},
	}
	progress := collectProgress(context.Background(), t, tool, suite)
	wantProgress(t, progress, []Progress{
		{Completed: 1},
		{Completed: 2},
	})
}

func TestExecuteProgressUsesOnlyObservedTests(t *testing.T) {
	suite := suiteForTest("example.com/m/unknown")
	tool := &fakeTool{
		streams: map[string]string{suite.Package.ImportPath: singleTestProgressStream()},
	}
	progress := collectProgress(context.Background(), t, tool, suite)
	wantProgress(t, progress, []Progress{{Completed: 1}})
}

func TestExecuteIncompleteStreamFails(t *testing.T) {
	for _, stream := range []string{"", `{"Action":"run","Test":"TestInterrupted"}` + "\n"} {
		suite := suiteForTest("example.com/m/incomplete")
		tool := &fakeTool{streams: map[string]string{suite.Package.ImportPath: stream}}
		result := newExecutor(tool, "/tmp", time.Second).Execute(context.Background(), suite, Hooks{})
		if !result.Failed || result.Completed || result.Err != "go test exited without a package result" {
			t.Fatalf("result = %+v, want incomplete failed suite", result)
		}
		if stream != "" {
			wantOutcomes(t, result.Tests, map[string]TestOutcome{"TestInterrupted": TestFailed})
		}
	}
}

func TestExecuteProgressCountsDuplicateTerminalsOnce(t *testing.T) {
	suite := suiteForTest("example.com/m/repeated")
	stream := `{"Action":"pass","Test":"TestRepeated"}` + "\n" +
		`{"Action":"run","Test":"TestRepeated"}` + "\n" +
		`{"Action":"pass","Test":"TestRepeated"}` + "\n" +
		`{"Action":"pass"}` + "\n"
	tool := &fakeTool{streams: map[string]string{suite.Package.ImportPath: stream}}
	progress := collectProgress(context.Background(), t, tool, suite)
	wantProgress(t, progress, []Progress{{Completed: 1}})
}

// TestExecutePreservesShuffleSeedPackageOutput pins Go's package-level shuffle event.
func TestExecutePreservesShuffleSeedPackageOutput(t *testing.T) {
	suite := suiteForTest("example.com/m/shuffled")
	tool := &fakeTool{streams: map[string]string{suite.Package.ImportPath: shuffleFailureStream()}}
	result := newExecutor(tool, "/tmp", time.Second).Execute(context.Background(), suite, Hooks{})
	if !result.Failed || len(result.PackageOutput) != 1 ||
		result.PackageOutput[0] != "-test.shuffle 12345\n" {
		t.Fatalf("result = %+v, want retained package shuffle seed", result)
	}
}

// suiteForTest returns a suite with enough package metadata to pass internal guards.
func suiteForTest(importPath string) SuiteTask {
	return SuiteTask{
		Package: gotool.Package{Dir: "/tmp", ImportPath: importPath, TestGoFiles: []string{"x_test.go"}},
		Digest:  strings.Repeat("c", 64),
	}
}

// collectProgress keeps progress assertions focused on callback payloads.
func collectProgress(ctx context.Context, t *testing.T, tool gotool.Tool, suite SuiteTask) []Progress {
	t.Helper()
	var progress []Progress
	hooks := Hooks{Progress: func(p Progress) { progress = append(progress, p) }}
	result := newExecutor(tool, "/tmp", time.Second).Execute(ctx, suite, hooks)
	if result.Failed {
		t.Fatalf("result = %+v, want passing suite", result)
	}
	return progress
}

// wantProgress compares complete sequences so callback regressions show order.
func wantProgress(t *testing.T, got []Progress, want []Progress) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("progress = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("progress[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// wantCanceledSuite bounds cancellation tests so a stuck child fails quickly.
func wantCanceledSuite(t *testing.T, resultCh <-chan SuiteResult) {
	t.Helper()
	select {
	case result := <-resultCh:
		if result.Failed || !result.Killed || result.Err != "" {
			t.Fatalf("result = %+v, want killed non-failure", result)
		}
	case <-time.After(time.Second):
		t.Fatalf("cancellation did not stop suite")
	}
}

// wantOutcomes checks folded names without depending on output ordering details.
func wantOutcomes(t *testing.T, tests []TestResult, want map[string]TestOutcome) {
	t.Helper()
	if len(tests) != len(want) {
		t.Fatalf("tests = %+v, want %d folded tests", tests, len(want))
	}
	for name, outcome := range want {
		if got := wantTestResult(t, tests, name).Outcome; got != outcome {
			t.Fatalf("%s outcome = %s, want %s", name, got, outcome)
		}
	}
}

// wantTestResult returns one folded test so callers fail at the missing name.
func wantTestResult(t *testing.T, tests []TestResult, name string) TestResult {
	t.Helper()
	for _, test := range tests {
		if test.Name == name {
			return test
		}
	}
	t.Fatalf("missing test %s in %+v", name, tests)
	return TestResult{}
}

// containsLine checks package-output diagnostics without hiding duplicate lines.
func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// foldingStream builds enough output to prove retention and drop accounting.
func foldingStream() string {
	var b strings.Builder
	b.WriteString(`{"Action":"run","Test":"TestPass"}` + "\n")
	b.WriteString(`{"Action":"pass","Test":"TestPass"}` + "\n")
	b.WriteString(`{"Action":"run","Test":"TestFail"}` + "\n")
	for i := range outputLinesPerTestKept + 3 {
		fmt.Fprintf(&b, "{\"Action\":\"output\",\"Test\":\"TestFail\",\"Output\":\"line %d\\n\"}\n", i)
	}
	b.WriteString(`{"Action":"fail","Test":"TestFail"}` + "\n")
	b.WriteString(`{"Action":"run","Test":"TestSkip"}` + "\n")
	b.WriteString(`{"Action":"skip","Test":"TestSkip"}` + "\n")
	b.WriteString(`{"Action":"fail","Elapsed":0.25}` + "\n")
	return b.String()
}

// activeStreamLines keeps watchdog tests readable while proving timer resets.
func activeStreamLines() []string {
	return []string{
		`{"Action":"run","Test":"TestSlow"}` + "\n",
		`{"Action":"output","Test":"TestSlow","Output":"still running\n"}` + "\n",
		`{"Action":"pass","Test":"TestSlow"}` + "\n",
		`{"Action":"pass","Elapsed":0.04}` + "\n",
	}
}

func twoTestProgressStream() string {
	return `{"Action":"run","Test":"TestA"}` + "\n" +
		`{"Action":"pass","Test":"TestA"}` + "\n" +
		`{"Action":"run","Test":"TestB"}` + "\n" +
		`{"Action":"skip","Test":"TestB"}` + "\n" +
		`{"Action":"pass","Elapsed":0.01}` + "\n"
}

func subtestProgressStream() string {
	return `{"Action":"run","Test":"TestA"}` + "\n" +
		`{"Action":"run","Test":"TestA/one"}` + "\n" +
		`{"Action":"pass","Test":"TestA/one"}` + "\n" +
		`{"Action":"run","Test":"TestA/two"}` + "\n" +
		`{"Action":"pass","Test":"TestA/two"}` + "\n" +
		`{"Action":"pass","Test":"TestA"}` + "\n" +
		`{"Action":"pass","Elapsed":0.01}` + "\n"
}

func unlistedProgressStream() string {
	return `{"Action":"pass","Test":"TestA"}` + "\n" +
		`{"Action":"pass","Test":"TestA/sub"}` + "\n" +
		`{"Action":"pass","Test":"ExampleDoc"}` + "\n" +
		`{"Action":"pass","Test":"FuzzParse"}` + "\n" +
		`{"Action":"pass","Test":"TestB"}` + "\n" +
		`{"Action":"pass","Elapsed":0.01}` + "\n"
}

// singleTestProgressStream keeps unknown-total progress assertions small.
func singleTestProgressStream() string {
	return `{"Action":"run","Test":"TestA"}` + "\n" +
		`{"Action":"pass","Test":"TestA"}` + "\n" +
		`{"Action":"pass","Elapsed":0.01}` + "\n"
}

// shuffleFailureStream preserves package-level Go diagnostics separately from tests.
func shuffleFailureStream() string {
	return `{"Action":"output","Output":"-test.shuffle 12345\n"}` + "\n" +
		`{"Action":"run","Test":"TestBad"}` + "\n" +
		`{"Action":"output","Test":"TestBad","Output":"boom\n"}` + "\n" +
		`{"Action":"fail","Test":"TestBad"}` + "\n" +
		`{"Action":"fail","Elapsed":0.01}` + "\n"
}

// fakeTool provides process streams for executor tests.
type fakeTool struct {
	streams   map[string]string
	readers   map[string]readerFactory
	stderr    map[string]string
	startErrs map[string]error
}

// readerFactory binds fake process output to the StartTests context.
type readerFactory func(context.Context) io.Reader

// CurrentModule fails because executor tests pass modules directly.
func (f *fakeTool) CurrentModule(context.Context) (gotool.Module, error) {
	return gotool.Module{}, fmt.Errorf("unexpected CurrentModule call")
}

// ListPackages fails because executor tests pass suites directly.
func (f *fakeTool) ListPackages(context.Context, string, []string) ([]gotool.Package, error) {
	return nil, fmt.Errorf("unexpected ListPackages call")
}

func (f *fakeTool) ListDependencies(context.Context, string, []string) ([]gotool.Package, error) {
	return nil, fmt.Errorf("unexpected ListDependencies call")
}

// ListModules fails because executor tests pass suites directly.
func (f *fakeTool) ListModules(context.Context, string) ([]gotool.Module, error) {
	return nil, fmt.Errorf("unexpected ListModules call")
}

// Environment fails because executor tests do not fingerprint.
func (f *fakeTool) Environment(context.Context, string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected Environment call")
}

func (f *fakeTool) ListTests(context.Context, string, string) ([]string, error) {
	panic("executor must not list tests")
}

// StartTests returns a fake process so executor tests own stream behavior.
func (f *fakeTool) StartTests(ctx context.Context, _ string, importPath string) (gotool.TestProcess, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, ctx.Err()
	}
	if err := f.startErrs[importPath]; err != nil {
		return nil, err
	}
	if reader, ok := f.readers[importPath]; ok {
		return fakeProcess{events: reader(ctx), stderr: f.stderr[importPath]}, nil
	}
	stream, ok := f.streams[importPath]
	if !ok {
		return nil, fmt.Errorf("missing stream for %s", importPath)
	}
	return fakeProcess{events: strings.NewReader(stream), stderr: f.stderr[importPath]}, nil
}

// fakeProcess implements gotool.TestProcess without spawning the Go command.
type fakeProcess struct {
	events io.Reader
	stderr string
}

// Events returns the configured event stream.
func (p fakeProcess) Events() io.Reader {
	return p.events
}

// Wait succeeds because fake streams encode the terminal behavior.
func (p fakeProcess) Wait() error {
	return nil
}

// Stderr returns configured process diagnostics after Wait.
func (p fakeProcess) Stderr() string {
	return p.stderr
}

// chunkReader returns one configured chunk per Read call.
type chunkReader struct {
	chunks [][]byte
	index  int
}

// Read copies the next chunk so drain assertions can count consumed chunks.
func (r *chunkReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

// blockingReader returns initial chunks, then waits for executor cancellation.
type blockingReader struct {
	ctx    context.Context
	chunks [][]byte
	index  int
}

// Read copies one chunk or waits until cancellation closes the fake process.
func (r *blockingReader) Read(p []byte) (int, error) {
	if r.index < len(r.chunks) {
		n := copy(p, r.chunks[r.index])
		r.index++
		return n, nil
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// steadyReader writes delayed lines so watchdog tests exercise activity resets.
func steadyReader(ctx context.Context, delay time.Duration, lines []string) io.Reader {
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		for _, line := range lines {
			select {
			case <-ctx.Done():
				_ = writer.CloseWithError(ctx.Err())
				return
			case <-time.After(delay):
			}
			if _, err := io.WriteString(writer, line); err != nil {
				return
			}
		}
	}()
	return reader
}

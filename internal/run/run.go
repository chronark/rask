// Package run executes one suite and folds its `go test -json` stream.
//
// Callers own suite fan-out. This package keeps only the per-suite work that
// has to stay beside process execution: JSON folding with memory bounds,
// live progress, and the no-output watchdog that kills a
// child before a compile or lock wait can stall the whole command.
package run

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/gotool"
)

const (
	// testCountPerSuiteMax bounds the number of test names tracked for one suite.
	testCountPerSuiteMax = 65536

	// eventCountPerSuiteMax bounds the number of JSON events read for one suite.
	eventCountPerSuiteMax = 1 << 22

	// defaultSuiteStallAfter bounds a child `go test` process that stops producing output.
	//
	// A production hang showed workers blocked in stdout reads for more than
	// five minutes while rask itself stayed healthy. `go test -timeout` only
	// bounds the test binary after it starts, not compile-phase or lock-wait
	// stalls, so the executor supervises every child process directly.
	defaultSuiteStallAfter = 5 * time.Minute

	// outputLinesPerTestKept bounds retained diagnostic output per test and package.
	outputLinesPerTestKept = 4096

	// lineBytesMax bounds one line read from `go test -json` stdout.
	lineBytesMax = 4 << 20

	// WorkerCountMax caps suite parallelism so user input cannot fan out unbounded processes.
	WorkerCountMax = 32
)

// DefaultWorkerCount leaves one CPU for orchestration while bounding process fan-out.
func DefaultWorkerCount() int {
	return min(WorkerCountMax, max(1, runtime.GOMAXPROCS(0)-1))
}

// TestOutcome is the terminal state of one test reported by `go test -json`.
type TestOutcome string

const (
	// TestPassed means the test reported a pass event.
	TestPassed TestOutcome = "pass"

	// TestFailed means the test reported a fail event or was still running when the suite failed.
	TestFailed TestOutcome = "fail"

	// TestIgnored means the test reported a skip event.
	TestIgnored TestOutcome = "skip"

	// testRunning marks tests that have started but not reached a terminal event.
	testRunning TestOutcome = "run"
)

// SuiteTask identifies one package test suite that needs execution.
type SuiteTask struct {
	// Package is the Go package to test.
	Package gotool.Package

	// Digest is the content digest associated with the suite.
	Digest string
}

// TestResult records the folded result for one test name.
type TestResult struct {
	// Name is the test name reported by `go test -json`.
	Name string

	// Outcome is the folded terminal state for the test.
	Outcome TestOutcome

	// Output contains retained diagnostic output for the test.
	Output []string

	// DroppedOutput is the number of output lines omitted after the retention bound.
	DroppedOutput int
}

// SuiteResult records the folded result for one package suite.
type SuiteResult struct {
	// Package is the package that was tested.
	Package gotool.Package

	// Digest is the suite content digest used by callers for cache decisions.
	Digest string

	// Tests contains folded test results in start order.
	Tests []TestResult

	// PackageOutput contains retained package-level and build diagnostic output.
	PackageOutput []string

	// ElapsedSeconds is the package elapsed time reported by `go test -json`.
	ElapsedSeconds float64

	// Failed reports whether the package or executor reached a user-visible failure.
	Failed bool

	// Completed reports whether a package-level pass, fail, or skip event was seen.
	Completed bool

	// Killed reports whether fail-fast cancellation stopped the suite.
	Killed bool

	// Err is the process or folding error text, or empty when no extra error occurred.
	Err string
}

// Progress lets callers render one suite without coupling run to a UI protocol.
type Progress struct {
	// Completed counts top-level Test terminal events observed in the JSON stream.
	Completed int

	// Total carries the test denominator when known.
	Total int

	// TotalKnown is false during streaming because unstarted tests are unknown.
	TotalKnown bool
}

// Hooks keeps lifecycle side effects caller-owned while Execute stays synchronous.
type Hooks struct {
	// Started fires only after a child process exists, so callers can mark real work.
	Started func()

	// Progress fires on terminal top-level test events, so callers can update one row.
	Progress func(Progress)
}

// Executor owns the dependencies for one suite execution.
type Executor struct {
	tool       gotool.Tool
	moduleDir  string
	stallAfter time.Duration
}

// NewExecutor uses production stall bounds so one silent child cannot hang a run.
func NewExecutor(tool gotool.Tool, moduleDir string) *Executor {
	return newExecutor(tool, moduleDir, defaultSuiteStallAfter)
}

// newExecutor lets tests shrink watchdog time without changing production defaults.
func newExecutor(tool gotool.Tool, moduleDir string, stallAfter time.Duration) *Executor {
	return &Executor{tool: tool, moduleDir: moduleDir, stallAfter: stallAfter}
}

// testEvent keeps the decoder narrow so unknown JSON fields cannot affect folding.
type testEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
	Elapsed float64
}

// suiteState accumulates JSON events until a [SuiteResult] can be built.
type suiteState struct {
	suite                  SuiteTask
	testNames              []string
	outcomes               map[string]TestOutcome
	outputs                map[string][]string
	outputsDropped         map[string]int
	packageOutput          []string
	completedTopLevelTests int
	elapsedSeconds         float64
	failed                 bool
	completed              bool
	killed                 bool
	err                    error
	emitProgress           func(Progress)
}

// Execute folds one suite so callers can choose their own parallelism model.
//
// Kill attribution reads the CALLER's context, never the process context: the
// watchdog cancels the process context when the child stalls, and if that
// cancellation were used to classify the kill, a stalled suite would fold as
// Killed (silently skipped by callers) instead of Failed with a named stall
// error — hiding exactly the hang the watchdog exists to expose.
func (e *Executor) Execute(callerCtx context.Context, suite SuiteTask, hooks Hooks) SuiteResult {
	processCtx, cancel := context.WithCancel(callerCtx)
	defer cancel()
	state := newSuiteState(suite, hooks.Progress)
	process, err := e.tool.StartTests(processCtx, e.moduleDir, suite.Package.ImportPath)
	if err != nil {
		if errors.Is(callerCtx.Err(), context.Canceled) {
			return SuiteResult{Package: suite.Package, Digest: suite.Digest, Killed: true}
		}
		state.recordError(err)
		state.failed = true
		return state.result()
	}
	if hooks.Started != nil {
		hooks.Started()
	}
	watchdog := startSuiteWatchdog(processCtx, cancel, e.stallAfter)
	readEvents(process.Events(), state, eventCountPerSuiteMax, watchdog.activity)
	waitErr := process.Wait()
	watchdog.stop()
	if watchdog.stalled() && callerCtx.Err() == nil {
		err := fmt.Errorf("suite produced no output for %s; killed (child process stalled)", e.stallAfter)
		state.recordError(err)
	}
	finishSuiteResult(callerCtx, state, waitErr, process.Stderr())
	return state.result()
}

// newSuiteState centralizes bounds maps so every suite starts with the same invariants.
func newSuiteState(
	suite SuiteTask,
	emitProgress func(Progress),
) *suiteState {
	assert.True(suite.Package.HasTests())
	return &suiteState{
		suite:          suite,
		outcomes:       make(map[string]TestOutcome),
		outputs:        make(map[string][]string),
		outputsDropped: make(map[string]int),
		emitProgress:   emitProgress,
	}
}

// readEvents folds stdout JSON events and always drains the stream before Wait.
//
// Draining after scanner stop is required because a full stdout pipe can
// deadlock the child process before [gotool.TestProcess.Wait] returns.
// Watchdog activity is reported at the read layer, not per parsed line, so
// mid-line progress on huge output lines and the post-stop drain both count.
func readEvents(stream io.Reader, state *suiteState, eventLimit int, activity func()) {
	events := activityReader{reader: stream, activity: activity}
	scanner := bufio.NewScanner(&events)
	scanner.Buffer(make([]byte, 64*1024), lineBytesMax)
	for eventCount := 0; scanner.Scan(); eventCount++ {
		if eventCount >= eventLimit {
			state.recordError(
				fmt.Errorf("suite %s exceeded %d events", state.suite.Package.ImportPath, eventLimit),
			)
			break
		}
		var ev testEvent
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			state.recordPackageOutput(scanner.Text() + "\n")
			continue
		}
		if state.applyEvent(ev) {
			state.reportProgress()
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) && state.err == nil {
		state.recordError(fmt.Errorf("reading %s events: %w", state.suite.Package.ImportPath, err))
	}
	_, _ = io.Copy(io.Discard, &events)
}

// activityReader reports child stdout bytes before handing them to a caller.
type activityReader struct {
	reader   io.Reader
	activity func()
}

// Read proxies one stream read and reports activity when bytes are received.
func (r *activityReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.activity()
	}
	return n, err
}

// suiteWatchdog owns the per-suite no-output timer and child-process cancel.
type suiteWatchdog struct {
	activityCh chan struct{}
	stopCh     chan struct{}
	doneCh     chan struct{}
	stalledNow atomic.Bool
}

// startSuiteWatchdog starts a timer that kills the child on stdout silence.
//
// The scanner remains a normal blocking read loop. When this timer fires it
// cancels the child process context; the killed process closes stdout, which
// unblocks the scanner with EOF or a read error.
func startSuiteWatchdog(
	ctx context.Context,
	cancel context.CancelFunc,
	stallAfter time.Duration,
) *suiteWatchdog {
	w := &suiteWatchdog{
		activityCh: make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	go w.run(ctx, cancel, stallAfter)
	return w
}

// run waits for activity, cancellation, explicit stop, or suite silence.
func (w *suiteWatchdog) run(ctx context.Context, cancel context.CancelFunc, stallAfter time.Duration) {
	timer := time.NewTimer(stallAfter)
	defer close(w.doneCh)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			w.stalledNow.Store(true)
			cancel()
			return
		case <-w.activityCh:
			// Go 1.23+ timer semantics guarantee Reset without a drain: no
			// stale tick can be received after Reset returns.
			timer.Reset(stallAfter)
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// activity records child stdout activity without blocking the event reader.
func (w *suiteWatchdog) activity() {
	select {
	case w.activityCh <- struct{}{}:
	default:
	}
}

// stalled reports whether the watchdog killed the child for stdout silence.
func (w *suiteWatchdog) stalled() bool {
	return w.stalledNow.Load()
}

// stop terminates the watchdog goroutine and waits until it exits.
//
// stop is called once per suite. Closing stopCh after the goroutine already
// exited (timer fired or ctx cancelled) is harmless: nothing else touches it.
func (w *suiteWatchdog) stop() {
	close(w.stopCh)
	<-w.doneCh
}

// finishSuiteResult reconciles process exit state with folded package events.
//
// A cancelled ctx means rask killed the child on purpose (fail-fast), so the
// suite is marked killed rather than failed and kill noise is suppressed.
func finishSuiteResult(ctx context.Context, state *suiteState, waitErr error, stderr string) {
	killedByUs := errors.Is(ctx.Err(), context.Canceled)
	if stderr != "" && !killedByUs {
		state.recordPackageOutput(stderr)
	}
	if state.completed {
		if state.err != nil && !killedByUs {
			state.failed = true
		}
		return
	}
	if killedByUs {
		state.failed = false
		state.killed = true
		return
	}
	state.failed = true
	if state.err != nil {
		return
	}
	if waitErr != nil {
		state.recordError(fmt.Errorf("go test: %w", waitErr))
		return
	}
	state.recordError(errors.New("go test exited without a package result"))
}

// applyEvent routes one JSON event and reports whether progress changed.
func (s *suiteState) applyEvent(ev testEvent) bool {
	if ev.Test == "" {
		s.applyPackageEvent(ev)
		return false
	}
	switch ev.Action {
	case "run":
		s.recordTestStarted(ev.Test)
	case "output":
		s.recordTestOutput(ev.Test, ev.Output)
	case "pass", "fail", "skip":
		return s.recordTerminalTest(ev)
	}
	return false
}

// applyPackageEvent folds package-level events into suite state.
func (s *suiteState) applyPackageEvent(ev testEvent) {
	switch ev.Action {
	case "output", "build-output":
		s.recordPackageOutput(ev.Output)
	case "pass", "fail", "skip":
		s.elapsedSeconds = ev.Elapsed
		s.failed = ev.Action == "fail"
		s.completed = true
	case "start", "run", "build-fail":
	}
}

// recordTerminalTest records a pass, fail, or skip event for a test once.
func (s *suiteState) recordTerminalTest(ev testEvent) bool {
	before, known := s.outcomes[ev.Test]
	if !known {
		s.recordTestStarted(ev.Test)
		before = testRunning
	}
	s.outcomes[ev.Test] = TestOutcome(ev.Action)
	if isTerminalOutcome(before) {
		return false
	}
	if !isTopLevelTestName(ev.Test) {
		return false
	}
	s.completedTopLevelTests++
	return true
}

// isTerminalOutcome reports whether an outcome has already contributed progress.
func isTerminalOutcome(outcome TestOutcome) bool {
	return outcome == TestPassed || outcome == TestFailed || outcome == TestIgnored
}

func isTopLevelTestName(name string) bool {
	return !strings.Contains(name, "/") && gotool.MatchesListTestsPattern(name)
}

// reportProgress emits the current suite counters when a receiver exists.
func (s *suiteState) reportProgress() {
	if s.emitProgress == nil {
		return
	}
	s.emitProgress(Progress{Completed: s.completedTopLevelTests})
}

// recordTestStarted records a test name once and enforces the per-suite bound.
func (s *suiteState) recordTestStarted(name string) {
	if _, known := s.outcomes[name]; known {
		return
	}
	if len(s.testNames) >= testCountPerSuiteMax {
		if s.err == nil {
			s.recordError(
				fmt.Errorf("suite %s exceeded %d tests", s.suite.Package.ImportPath, testCountPerSuiteMax),
			)
		}
		return
	}
	s.testNames = append(s.testNames, name)
	s.outcomes[name] = testRunning
}

// recordTestOutput appends bounded diagnostic output for one test.
func (s *suiteState) recordTestOutput(name string, line string) {
	if len(s.outputs[name]) >= outputLinesPerTestKept {
		s.outputsDropped[name]++
		return
	}
	s.outputs[name] = append(s.outputs[name], line)
}

// recordPackageOutput appends bounded package-level diagnostic output.
func (s *suiteState) recordPackageOutput(line string) {
	if len(s.packageOutput) >= outputLinesPerTestKept {
		return
	}
	s.packageOutput = append(s.packageOutput, line)
}

// recordError preserves the first failure and makes diagnostics renderable.
func (s *suiteState) recordError(err error) {
	if s.err == nil {
		s.err = err
	}
	s.recordPackageOutput(err.Error() + "\n")
}

// result converts mutable folding state into an immutable suite result.
func (s *suiteState) result() SuiteResult {
	tests := make([]TestResult, 0, len(s.testNames))
	for _, name := range s.testNames {
		outcome := s.outcomes[name]
		// A test still "running" when its suite concluded or failed crashed
		// or was cut off, so it folds to failed. Killed suites skip the remap
		// so fail-fast cancellation is not misreported as test failures.
		if outcome == testRunning && (s.completed || s.failed) {
			outcome = TestFailed
		}
		tests = append(tests, TestResult{
			Name: name, Outcome: outcome, Output: append([]string(nil), s.outputs[name]...),
			DroppedOutput: s.outputsDropped[name],
		})
	}
	errText := ""
	if s.err != nil {
		errText = s.err.Error()
	}
	return SuiteResult{
		Package: s.suite.Package, Digest: s.suite.Digest, Tests: tests,
		PackageOutput: append([]string(nil), s.packageOutput...), ElapsedSeconds: s.elapsedSeconds,
		Failed: s.failed, Completed: s.completed, Killed: s.killed, Err: errText,
	}
}

// Package frontend renders all terminal output for rask.
//
// The package owns status verbs, cached and executed suite blocks, failure
// details, summaries, color handling, terminal-width detection, and the sticky
// progress region. It accepts rendering-shaped data from callers so it does not
// import discovery, fingerprinting, running, or caching packages.
//
// [Frontend.Run] is the sole output owner for test runs. It consumes render
// events, handles sticky-region ticks inside the same goroutine, and returns the
// final exit code after the event channel is closed.
package frontend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chronark/rask/internal/assert"
)

const (
	// terminalWidthDefault is used when terminal width cannot be detected.
	terminalWidthDefault = 120

	// terminalWidthMin is the minimum accepted terminal width.
	terminalWidthMin = 20

	// runningSuiteRowsMax bounds rows below the header, including overflow.
	//
	// This mirrors internal/run workerCountMax. Keep it at least as large as
	// the runner's worker cap so every concurrently running suite is visible.
	runningSuiteRowsMax = 32

	// progressRowIndent visually separates running-suite rows from aggregate progress.
	progressRowIndent = "    "

	// suiteVerbWidth is the fixed width of permanent suite outcome verbs.
	suiteVerbWidth = 6

	// suitePathColumnCap bounds the permanent suite import-path column.
	suitePathColumnCap = 70

	// suiteDurationColumnWidth right-aligns elapsed durations before counts.
	suiteDurationColumnWidth = 8

	// suiteCountsReserveWidth budgets permanent terminal rows for realistic counts.
	//
	// It covers "16369 passed; 2330 ignored", keeping the recoverable path as
	// the field that bends first when terminal width is tight.
	suiteCountsReserveWidth = 26

	// suiteCountsSeparatorWidth is the two-space gap before count text.
	suiteCountsSeparatorWidth = 2

	// colorReset clears ANSI styling.
	colorReset = "\x1b[0m"

	// colorBold enables bold ANSI styling.
	colorBold = "\x1b[1m"

	// colorDim enables dim ANSI styling.
	colorDim = "\x1b[2m"

	// colorGreenBold renders a bold green status.
	colorGreenBold = "\x1b[1;32m"

	// colorRedBold renders a bold red status.
	colorRedBold = "\x1b[1;31m"

	// colorYellowBold renders a bold yellow status.
	colorYellowBold = "\x1b[1;33m"

	// colorCyanBold renders a bold cyan status.
	colorCyanBold = "\x1b[1;36m"

	// eraseToEndOfScreen clears stale rows before a synchronized frame is repainted.
	eraseToEndOfScreen = "\x1b[0J"

	// synchronizedOutputOpen starts a DEC synchronized-output frame.
	synchronizedOutputOpen = "\x1b[?2026h"

	// synchronizedOutputClose ends a DEC synchronized-output frame.
	synchronizedOutputClose = "\x1b[?2026l"
)

// TestOutcome is the rendered terminal state of one test.
type TestOutcome string

const (
	// TestPassed renders a passing test verdict.
	TestPassed TestOutcome = "pass"

	// TestFailed renders a failing test verdict.
	TestFailed TestOutcome = "fail"

	// TestIgnored renders a skipped test verdict.
	TestIgnored TestOutcome = "skip"
)

// TestResult is the rendering data for one test.
type TestResult struct {
	// Name is the displayed test name.
	Name string

	// Outcome is the displayed test outcome.
	Outcome TestOutcome

	// Output is retained diagnostic output for the test.
	Output []string

	// DroppedOutput is the number of omitted diagnostic lines.
	DroppedOutput int
}

// SuiteResult is the rendering data for one executed suite.
type SuiteResult struct {
	// ImportPath is the displayed package import path.
	ImportPath string

	// Tests contains rendered test results in start order.
	Tests []TestResult

	// PackageOutput contains package-level diagnostics for suite failures.
	PackageOutput []string

	// ElapsedSeconds is the displayed suite runtime.
	ElapsedSeconds float64

	// Failed reports whether the suite should render as failed.
	Failed bool

	// Err is extra runner or process error text.
	Err string
}

// EventKind identifies the render payload carried by Event.
type EventKind int

const (
	// EventRunPlan records the selected suite count and path layout before results stream.
	EventRunPlan EventKind = iota + 1

	// EventCached renders a cache-hit suite result. The output verb is
	// "Cached", not cargo's "Fresh", because "Fresh" can read as freshly run
	// even though the suite did not run.
	EventCached

	// EventStarted records a suite that entered the runner pool.
	EventStarted

	// EventTestProgress updates a running suite's sticky-region counters.
	EventTestProgress

	// EventSuiteResult renders one completed suite result.
	EventSuiteResult

	// EventSummary renders the final summary and sets the exit code.
	EventSummary

	// EventError renders a fatal error and sets a failing exit code.
	EventError

	// EventRemoved renders a module-cache removal status.
	EventRemoved
)

// CachedEvent is the render payload for a cache-hit suite.
type CachedEvent struct {
	// ImportPath is the cached package import path.
	ImportPath string

	// Passed is the cached passing test count.
	Passed int

	// Ignored is the cached skipped test count.
	Ignored int
}

// RunPlan is the render payload for suite totals and permanent-row layout.
type RunPlan struct {
	// Suites is the number of selected suites in this run.
	Suites int

	// MaxImportPathWidth is the widest shortened selected import path in display runes.
	MaxImportPathWidth int
}

// TestProgress is the render payload for live per-suite progress counters.
type TestProgress struct {
	// ImportPath identifies the running suite row to update.
	ImportPath string

	// Completed is the number of terminal test events seen for the suite.
	Completed int

	// Total is the known test total when TotalKnown is true.
	Total int

	// TotalKnown is false when the runner cannot know how many tests will execute.
	TotalKnown bool
}

// Event is the closed render union consumed by Frontend.Run.
//
// Test runs may send events from the orchestrator and suite workers. The
// orchestrator remains the only close owner so rendering sees one complete
// stream and never races worker sends against close.
type Event struct {
	// Kind selects which payload field is meaningful.
	Kind EventKind

	// Cached carries cache-hit suite data for EventCached.
	Cached CachedEvent

	// RunPlan carries suite totals and path width for EventRunPlan.
	RunPlan RunPlan

	// ImportPath carries package names for started events.
	ImportPath string

	// TestProgress carries running counters for EventTestProgress.
	TestProgress TestProgress

	// SuiteResult carries completed suite data for EventSuiteResult.
	SuiteResult SuiteResult

	// Summary carries final aggregate data for EventSummary.
	Summary Summary

	// Error carries fatal error text for EventError.
	Error string

	// ModulePath carries the module path for EventRemoved.
	ModulePath string
}

// Summary is the rendering data for the final run summary.
type Summary struct {
	// Suites is the total suite count considered in the run.
	Suites int

	// Tests is the total folded test count.
	Tests int

	// Passed is the total passing test count.
	Passed int

	// Failed is the total failing test count.
	Failed int

	// Ignored is the total skipped test count.
	Ignored int

	// Cached is the number of suites served from cache.
	Cached int

	// RemoteCached is the number of cached suites served by the remote tier.
	RemoteCached int

	// Skipped is the number of suites skipped by fail-fast.
	Skipped int

	// FailedSuites contains failed package import paths.
	FailedSuites []string

	// CacheErrors is the number of cache write or prune errors.
	CacheErrors int

	// CacheDisabledReason explains why result caching was bypassed for the run.
	CacheDisabledReason string

	// RemoteConfigured reports that the run used a configured remote tier.
	RemoteConfigured bool

	// RemoteReads is the number of successful remote cache hits.
	RemoteReads int

	// RemoteWrites is the number of successful remote cache writes.
	RemoteWrites int

	// Interrupted reports that root signal cancellation stopped queued suites.
	Interrupted bool

	// Elapsed is the total elapsed run time.
	Elapsed time.Duration
}

// runningSuite is one in-flight suite and the time it entered the runner pool.
type runningSuite struct {
	importPath  string
	startedAt   time.Time
	completed   int
	total       int
	totalKnown  bool
	initialized bool
}

// Frontend writes suite output, sticky progress, and summaries.
//
// Frontend is not safe for concurrent method calls during a test run. Run owns
// all rendering state, consumes its event subscription until it is closed, and
// returns the final exit code. Cancellation stops rendering and returns the
// context error.
type Frontend struct {
	out        io.Writer
	errOut     io.Writer
	in         <-chan Event
	terminal   bool
	color      bool
	verbose    bool
	modulePath string
	tickEvery  time.Duration
	startedAt  time.Time
	width      int
	total      int
	done       int
	pathWidth  int
	passed     int
	failed     int
	ignored    int
	running    []runningSuite
	linesDrawn int
	exitCode   int
	now        func() time.Time
	ticks      <-chan time.Time
	framing    bool
}

// New returns a frontend writing to out and errOut and consuming in during Run.
func New(out io.Writer, errOut io.Writer, terminal bool, in <-chan Event) *Frontend {
	assert.NotNil(out)
	assert.NotNil(errOut)
	width := terminalWidth()
	assert.GreaterOrEqual(width, terminalWidthMin)
	now := time.Now
	return &Frontend{
		out: out, errOut: errOut, in: in, terminal: terminal, color: terminal,
		startedAt: now(), width: width, tickEvery: 100 * time.Millisecond,
		now: now,
	}
}

// ConfigureRun sets display options that are only known after test-command setup.
//
// The verbose option keeps cargo-shaped per-test lines for passed and ignored
// tests. Quiet output still renders failed per-test lines and full diagnostics.
func (f *Frontend) ConfigureRun(verbose bool, modulePath string) {
	f.verbose = verbose
	f.modulePath = modulePath
}

// RenderError renders a fatal CLI error and marks the frontend as failing.
func (f *Frontend) RenderError(message string) {
	f.fatal(message)
}

// RenderRemoved renders a module-cache removal status.
func (f *Frontend) RenderRemoved(modulePath string) {
	f.removed(modulePath)
}

// RenderRemoteRemoved renders an explicit remote-cache purge status.
func (f *Frontend) RenderRemoteRemoved(objectCount int) {
	f.status(colorGreenBold, "Removed", "%d remote %s", objectCount, plural(objectCount, "object"))
}

// Run consumes render events until the event channel closes and returns the final exit code.
//
// Run should be the only goroutine writing terminal output during test runs.
// Cancellation erases any sticky region and returns ctx.Err; normal completion
// happens when orchestration closes the event channel after sending
// EventSummary. Run does not close the channel; orchestration is the only close
// owner even though workers may send suite events before the summary.
func (f *Frontend) Run(ctx context.Context) (int, error) {
	if f.terminal {
		return f.runTerminal(ctx)
	}
	for {
		select {
		case event, ok := <-f.in:
			if !ok {
				f.erase()
				return f.exitCode, nil
			}
			if err := f.applyEvent(event); err != nil {
				return 1, err
			}
		case <-ctx.Done():
			f.erase()
			return f.exitCode, ctx.Err()
		}
	}
}

// runTerminal consumes events in batches and flushes one synchronized frame per tick.
func (f *Frontend) runTerminal(ctx context.Context) (int, error) {
	ticks, stop := f.terminalTicks()
	defer stop()
	var pending []Event
	for {
		select {
		case event, ok := <-f.in:
			if !ok {
				return f.finishTerminal(ctx, pending)
			}
			pending = append(pending, event)
			if eventCompletesRun(event) {
				if err := f.flushTerminal(pending, true); err != nil {
					return 1, err
				}
				pending = pending[:0]
			}
		case <-ticks:
			if err := f.flushTerminal(pending, false); err != nil {
				return 1, err
			}
			pending = pending[:0]
		case <-ctx.Done():
			return f.finishTerminal(ctx, pending)
		}
	}
}

// terminalTicks returns the production ticker or a test-supplied tick channel.
func (f *Frontend) terminalTicks() (<-chan time.Time, func()) {
	if f.ticks != nil {
		return f.ticks, func() {}
	}
	ticker := time.NewTicker(f.tickEvery)
	return ticker.C, ticker.Stop
}

// finishTerminal flushes pending events and erases any remaining progress rows.
func (f *Frontend) finishTerminal(ctx context.Context, pending []Event) (int, error) {
	if err := f.flushTerminal(pending, true); err != nil {
		return 1, err
	}
	if err := ctx.Err(); err != nil {
		return f.exitCode, err
	}
	return f.exitCode, nil
}

// flushTerminal applies a pending batch and emits at most one synchronized frame.
func (f *Frontend) flushTerminal(pending []Event, final bool) error {
	previousRows := f.linesDrawn
	permanent, err := f.applyTerminalEvents(pending)
	if err != nil {
		return err
	}
	rows := f.terminalProgressRows(final)
	if len(permanent) == 0 && len(rows) == 0 && previousRows == 0 {
		return nil
	}
	return f.writeTerminalFrame(previousRows, permanent, rows)
}

// applyTerminalEvents captures permanent output for inclusion in one frame.
func (f *Frontend) applyTerminalEvents(pending []Event) ([]byte, error) {
	var permanent bytes.Buffer
	originalOut, framing := f.out, f.framing
	f.out, f.framing = &permanent, true
	defer func() { f.out, f.framing = originalOut, framing }()
	for _, event := range pending {
		if err := f.applyEvent(event); err != nil {
			return nil, err
		}
	}
	return permanent.Bytes(), nil
}

// terminalProgressRows returns the freshly painted region rows for a frame.
func (f *Frontend) terminalProgressRows(final bool) []string {
	if final || f.done >= f.total {
		return nil
	}
	return f.progressLines(f.now())
}

// writeTerminalFrame writes cursor movement, permanent output, and rows once.
func (f *Frontend) writeTerminalFrame(previousRows int, permanent []byte, rows []string) error {
	var frame bytes.Buffer
	frame.WriteString(synchronizedOutputOpen)
	frame.WriteString("\r")
	if previousRows > 1 {
		fmt.Fprintf(&frame, "\x1b[%dA", previousRows-1)
	}
	frame.WriteString(eraseToEndOfScreen)
	frame.Write(permanent)
	drawnRows := writeProgressRows(&frame, rows)
	frame.WriteString(synchronizedOutputClose)
	_, err := f.out.Write(frame.Bytes())
	f.linesDrawn = drawnRows
	return err
}

// applyEvent routes one render event to the corresponding frontend operation.
func (f *Frontend) applyEvent(event Event) error {
	switch event.Kind {
	case EventRunPlan:
		f.runPlan(event.RunPlan)
	case EventCached:
		f.cached(event.Cached)
	case EventStarted:
		f.suiteStarted(event.ImportPath)
	case EventTestProgress:
		f.suiteProgress(event.TestProgress)
	case EventSuiteResult:
		f.suiteFinished(event.SuiteResult)
	case EventSummary:
		f.summary(event.Summary)
	case EventError:
		f.fatal(event.Error)
	case EventRemoved:
		f.removed(event.ModulePath)
	default:
		return fmt.Errorf("unknown frontend event kind %d", event.Kind)
	}
	return nil
}

// runPlan fixes run-wide suite totals and permanent-row path width.
func (f *Frontend) runPlan(plan RunPlan) {
	f.total = plan.Suites
	f.pathWidth = f.plannedSuitePathWidth(plan.MaxImportPathWidth)
}

// cached renders one cache-hit suite line and updates aggregate progress.
// Cached lines print consecutively without blank separators.
func (f *Frontend) cached(event CachedEvent) {
	f.done++
	f.passed += event.Passed
	f.ignored += event.Ignored
	f.preparePermanentOutput()
	fmt.Fprintln(f.out, f.cachedSuiteRow(event.ImportPath))
}

// suiteStarted records an executing suite for sticky-region elapsed rows.
func (f *Frontend) suiteStarted(importPath string) {
	f.running = append(f.running, runningSuite{importPath: importPath, startedAt: f.now()})
}

// suiteProgress updates cosmetic counters for the sticky terminal region only.
func (f *Frontend) suiteProgress(progress TestProgress) {
	if !f.terminal {
		return
	}
	for index := range f.running {
		if f.running[index].importPath != progress.ImportPath {
			continue
		}
		f.running[index].completed = progress.Completed
		f.running[index].total = progress.Total
		f.running[index].totalKnown = progress.TotalKnown
		f.running[index].initialized = true
		return
	}
}

// suiteFinished renders one executed suite and updates aggregate progress.
func (f *Frontend) suiteFinished(result SuiteResult) {
	f.preparePermanentOutput()
	f.renderSuiteResult(result)
	f.removeRunning(result.ImportPath)
	f.done++
	passed, failed, ignored := countTests(result.Tests)
	f.passed += passed
	f.failed += failed
	f.ignored += ignored
}

// summary stops the sticky ticker and renders the final run summary.
func (f *Frontend) summary(summary Summary) {
	f.preparePermanentOutput()
	f.total = summary.Suites
	if len(summary.FailedSuites) > 0 {
		f.exitCode = 1
	}
	if summary.CacheErrors > 0 {
		fmt.Fprintf(f.out, "%s\n", f.paint(colorDim,
			fmt.Sprintf("note: %d cache errors; affected suites will rerun", summary.CacheErrors)))
	}
	if summary.CacheDisabledReason != "" {
		fmt.Fprintf(f.out, "%s\n", f.paint(colorDim, summary.CacheDisabledReason))
	}
	if summary.Interrupted {
		f.exitCode = 130
		fmt.Fprintf(f.errOut, "%s interrupted; %d %s skipped\n",
			f.paint(colorRedBold, "error:"), summary.Skipped, plural(summary.Skipped, "suite"))
		return
	}
	if len(summary.FailedSuites) > 0 {
		skipNote := ""
		if summary.Skipped > 0 {
			skipNote = fmt.Sprintf(" (%d %s skipped)", summary.Skipped, plural(summary.Skipped, "suite"))
		}
		fmt.Fprintf(f.errOut, "%s test failed in %s%s\n",
			f.paint(colorRedBold, "error:"), f.shortenedList(summary.FailedSuites), skipNote)
		return
	}
	cachedNote := ""
	if summary.Cached > 0 {
		cachedNote = fmt.Sprintf(", %d cached", summary.Cached)
		if summary.RemoteConfigured {
			cachedNote += fmt.Sprintf(" (%d remote)", summary.RemoteCached)
		}
	}
	remoteNote := ""
	if summary.RemoteConfigured {
		remoteNote = fmt.Sprintf(" | remote: %d reads, %d writes",
			summary.RemoteReads, summary.RemoteWrites)
	}
	f.status(colorGreenBold, "Finished",
		"%d %s, %d %s: %d passed; %d failed; %d ignored in %s%s%s",
		summary.Suites, plural(summary.Suites, "suite"), summary.Tests,
		plural(summary.Tests, "test"), summary.Passed, summary.Failed,
		summary.Ignored, formatPermanentDuration(summary.Elapsed), cachedNote, remoteNote)
}

// removed renders a module-cache removal message.
func (f *Frontend) removed(modulePath string) {
	f.status(colorGreenBold, "Removed", "%s", modulePath)
}

// fatal renders a fatal CLI error and sets the failing exit code.
func (f *Frontend) fatal(message string) {
	f.exitCode = 1
	f.preparePermanentOutput()
	fmt.Fprintf(f.errOut, "%s %s\n", f.paint(colorRedBold, "error:"), message)
}

// renderSuiteResult renders one complete suite block with a merged outcome row.
func (f *Frontend) renderSuiteResult(result SuiteResult) {
	fmt.Fprintln(f.out, f.suiteResultRow(result))
	for _, test := range result.Tests {
		// Quiet output still lists every failed test; -v lists all of them.
		if f.verbose || test.Outcome == TestFailed {
			fmt.Fprintf(f.out, "test %s ... %s\n", test.Name, f.testVerdict(test.Outcome))
		}
	}
	failures := failingTests(result.Tests)
	if result.Failed {
		f.renderPackageOutput(result.PackageOutput)
	}
	if len(failures) > 0 {
		f.renderFailureDetails(failures)
	}
	if result.Failed && len(failures) == 0 {
		f.renderPackageError(result.Err)
	}
}

// suiteResultRow renders the merged permanent outcome, duration, and count row.
func (f *Frontend) suiteResultRow(result SuiteResult) string {
	verb, color := "Passed", colorGreenBold
	if result.Failed {
		verb, color = "Failed", colorRedBold
	}
	path := ShortenImportPath(result.ImportPath, f.modulePath)
	passed, failed, ignored := countTests(result.Tests)
	duration := time.Duration(result.ElapsedSeconds * float64(time.Second))
	row := f.alignedSuiteRow(verb, path,
		suiteCountsText(passed, failed, ignored), formatPermanentDuration(duration))
	return paintSuiteVerb(row, f.paint(color, verb), f.color)
}

// cachedSuiteRow renders the dim, count-free cache-hit row.
func (f *Frontend) cachedSuiteRow(importPath string) string {
	path := ShortenImportPath(importPath, f.modulePath)
	pathWidth := min(suitePathColumnCap, max(f.permanentRowWidth()-suiteVerbWidth-1, 0))
	row := fmt.Sprintf("%-*s %s", suiteVerbWidth, "Cached", truncateMiddle(path, pathWidth))
	return f.paint(colorDim, row)
}

// alignedSuiteRow builds an aligned suite row before optional verb coloring.
func (f *Frontend) alignedSuiteRow(verb string, path string, counts string, duration string) string {
	pathWidth := f.resultPathWidth(path, counts, duration)
	row := formatAlignedSuiteRow(verb, path, counts, duration, pathWidth)
	return f.guardPermanentSuiteRow(row, verb, path, counts, duration, pathWidth)
}

// shortenedList renders failed package paths with the same display shortening as suite rows.
func (f *Frontend) shortenedList(importPaths []string) string {
	shortened := make([]string, 0, len(importPaths))
	for _, importPath := range importPaths {
		shortened = append(shortened, ShortenImportPath(importPath, f.modulePath))
	}
	return strings.Join(shortened, ", ")
}

// plannedSuitePathWidth budgets terminal permanent rows before any result prints.
func (f *Frontend) plannedSuitePathWidth(width int) int {
	pathWidth := min(width, suitePathColumnCap)
	if !f.terminal {
		return pathWidth
	}
	fixed := suiteVerbWidth + 1 + suiteDurationColumnWidth
	fixed += suiteCountsSeparatorWidth + suiteCountsReserveWidth
	return min(pathWidth, max(f.width-fixed, 1))
}

// resultPathWidth returns the planned path width reduced for actual row text.
func (f *Frontend) resultPathWidth(path string, counts string, duration string) int {
	pathWidth := min(f.pathWidth, suitePathColumnCap)
	if pathWidth == 0 {
		pathWidth = min(runeLen(path), suitePathColumnCap)
	}
	if !f.terminal {
		return pathWidth
	}
	return min(pathWidth, max(f.permanentRowWidth()-suiteRowFixedWidth(counts, duration), 0))
}

// guardPermanentSuiteRow is the hard half of the terminal no-wrap invariant.
//
// Terminal cursor arithmetic assumes permanent suite rows written above the
// live region occupy one visual row. The path is recoverable from context, so
// it is middle-truncated further before counts or duration are allowed to bend.
func (f *Frontend) guardPermanentSuiteRow(
	row string, verb string, path string, counts string, duration string, pathWidth int,
) string {
	if !f.terminal || runeLen(row) <= f.permanentRowWidth() {
		return row
	}
	for width := pathWidth - 1; width >= 0; width-- {
		row = formatAlignedSuiteRow(verb, path, counts, duration, width)
		if runeLen(row) <= f.permanentRowWidth() {
			return row
		}
	}
	return f.noWrapLine(formatAlignedSuiteRow(verb, path, counts, duration, 0))
}

// formatAlignedSuiteRow builds an aligned suite row before optional verb coloring.
func formatAlignedSuiteRow(verb string, path string, counts string, duration string, pathWidth int) string {
	path = truncateMiddle(path, pathWidth)
	row := fmt.Sprintf("%-*s %-*s%*s", suiteVerbWidth, verb,
		pathWidth, path, suiteDurationColumnWidth, duration)
	if counts != "" {
		row += strings.Repeat(" ", suiteCountsSeparatorWidth) + counts
	}
	return row
}

// suiteRowFixedWidth returns the permanent row width without the path column.
func suiteRowFixedWidth(counts string, duration string) int {
	width := suiteVerbWidth + 1 + max(suiteDurationColumnWidth, runeLen(duration))
	if counts != "" {
		width += suiteCountsSeparatorWidth + runeLen(counts)
	}
	return width
}

// renderFailureDetails renders retained output for failed tests with cargo-style spacing.
func (f *Frontend) renderFailureDetails(failures []TestResult) {
	fmt.Fprintf(f.out, "\n%s\n\n", f.paint(colorBold, "failures:"))
	for _, test := range failures {
		fmt.Fprintf(f.out, "---- %s stdout ----\n", test.Name)
		for _, line := range test.Output {
			if !isFrameworkLine(line) {
				fmt.Fprint(f.out, line)
			}
		}
		if test.DroppedOutput > 0 {
			fmt.Fprintf(f.out, "... %d output %s dropped ...\n",
				test.DroppedOutput, plural(test.DroppedOutput, "line"))
		}
		fmt.Fprintln(f.out)
	}
	fmt.Fprintf(f.out, "%s\n", f.paint(colorBold, "failures:"))
	for _, test := range failures {
		fmt.Fprintf(f.out, "    %s\n", test.Name)
	}
	fmt.Fprintln(f.out)
}

// renderPackageOutput renders package-level output kept from a failed suite.
func (f *Frontend) renderPackageOutput(lines []string) {
	for _, line := range lines {
		if isFrameworkLine(line) || strings.HasPrefix(line, "FAIL") {
			continue
		}
		fmt.Fprint(f.out, line)
	}
}

// renderPackageError renders a package-level runner error.
func (f *Frontend) renderPackageError(errText string) {
	if errText != "" {
		fmt.Fprintf(f.out, "%s\n", errText)
	}
}

// status renders one status line.
func (f *Frontend) status(color string, verb string, format string, args ...any) {
	fmt.Fprintf(f.out, "%s %s\n", f.paint(color, verb), fmt.Sprintf(format, args...))
}

// preparePermanentOutput clears an already drawn region before direct writes.
func (f *Frontend) preparePermanentOutput() {
	if !f.framing {
		f.erase()
	}
}

// progressLines returns the aggregate line plus capped running-suite rows.
func (f *Frontend) progressLines(now time.Time) []string {
	testsRun := f.passed + f.failed + f.ignored
	line := fmt.Sprintf("Testing %d/%d suites | %d tests: %d passed; %d failed; %d ignored | %s",
		f.done, f.total, testsRun, f.passed, f.failed, f.ignored,
		formatRegionHeaderDuration(now.Sub(f.startedAt)))
	lines := []string{f.progressHeader(line)}
	lines = append(lines, f.runningSuiteRows(now)...)
	return lines
}

// progressHeader colors the verb after no-wrap truncation keeps cell math simple.
func (f *Frontend) progressHeader(line string) string {
	line = truncateRunes(line, f.noWrapWidth())
	prefix := "Testing "
	if f.color && strings.HasPrefix(line, prefix) {
		return f.paint(colorCyanBold, prefix) + strings.TrimPrefix(line, prefix)
	}
	return line
}

// truncateRunes shortens display text without splitting multi-byte characters.
func truncateRunes(text string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	if width <= 0 {
		return ""
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

// erase clears the full sticky region when one is visible.
func (f *Frontend) erase() {
	if f.framing {
		return
	}
	if f.linesDrawn == 0 {
		return
	}
	fmt.Fprint(f.out, "\r")
	if f.linesDrawn > 1 {
		fmt.Fprintf(f.out, "\x1b[%dA", f.linesDrawn-1)
	}
	fmt.Fprint(f.out, eraseToEndOfScreen)
	f.linesDrawn = 0
}

// writeProgressRows appends no-wrap rows and returns the actual row count drawn.
func writeProgressRows(out *bytes.Buffer, rows []string) int {
	for index, line := range rows {
		if index > 0 {
			out.WriteString("\n")
		}
		out.WriteString(line)
	}
	return len(rows)
}

// eventCompletesRun reports events that must flush without waiting for a tick.
func eventCompletesRun(event Event) bool {
	return event.Kind == EventSummary || event.Kind == EventError
}

// paint applies ANSI color when terminal color output is enabled.
func (f *Frontend) paint(color string, text string) string {
	if !f.color {
		return text
	}
	return color + text + colorReset
}

// removeRunning removes one suite from the sticky progress region.
func (f *Frontend) removeRunning(importPath string) {
	for index, running := range f.running {
		if running.importPath == importPath {
			f.running = append(f.running[:index], f.running[index+1:]...)
			return
		}
	}
}

// runningSuiteRows returns capped rows sorted with the longest-running first.
func (f *Frontend) runningSuiteRows(now time.Time) []string {
	running := append([]runningSuite(nil), f.running...)
	sort.Slice(running, func(i, j int) bool {
		if running[i].startedAt.Equal(running[j].startedAt) {
			return running[i].importPath < running[j].importPath
		}
		return running[i].startedAt.Before(running[j].startedAt)
	})
	limit := min(len(running), runningSuiteRowsMax)
	if len(running) > runningSuiteRowsMax {
		limit = runningSuiteRowsMax - 1
	}
	rows := make([]string, 0, limit+1)
	visible := running[:limit]
	layout := f.runningSuiteLayout(visible, now)
	for _, row := range layout {
		rows = append(rows, f.formatRunningSuiteRow(row))
	}
	if overflow := len(running) - limit; overflow > 0 {
		rows = append(rows, f.noWrapLine(fmt.Sprintf("%s... and %d more", progressRowIndent, overflow)))
	}
	return rows
}

// runningRow is one precomputed row in a frame-local aligned layout.
type runningRow struct {
	name          string
	progress      string
	elapsed       string
	nameWidth     int
	progressWidth int
	elapsedWidth  int
}

// runningSuiteLayout computes frame-local columns before path truncation.
func (f *Frontend) runningSuiteLayout(suites []runningSuite, now time.Time) []runningRow {
	width := f.noWrapWidth()
	rows := make([]runningRow, 0, len(suites))
	available := max(width-len(progressRowIndent), 0)
	nameWidth, progressWidth, elapsedWidth := 0, 0, 0
	for _, suite := range suites {
		row := runningRow{
			name:     ShortenImportPath(suite.importPath, f.modulePath),
			progress: suiteProgressText(suite),
			elapsed:  formatRegionRowDuration(now.Sub(suite.startedAt)),
		}
		nameWidth = max(nameWidth, runeLen(row.name))
		progressWidth = max(progressWidth, runeLen(row.progress))
		elapsedWidth = max(elapsedWidth, runeLen(row.elapsed))
		rows = append(rows, row)
	}
	nameWidth = min(nameWidth, max(available-progressWidth-elapsedWidth-2, 0))
	for index := range rows {
		rows[index].nameWidth = nameWidth
		rows[index].progressWidth = progressWidth
		rows[index].elapsedWidth = elapsedWidth
	}
	return rows
}

// formatRunningSuiteRow renders one no-wrap row from a frame-local layout.
func (f *Frontend) formatRunningSuiteRow(row runningRow) string {
	width := f.noWrapWidth()
	if width <= len(progressRowIndent) {
		return truncateRunes(progressRowIndent, width)
	}
	if row.nameWidth <= 0 {
		return f.noWrapLine(progressRowIndent + row.progress + " " + row.elapsed)
	}
	name := truncateMiddle(row.name, row.nameWidth)
	namePad := max(1, row.nameWidth-runeLen(name)+1)
	progressPad := max(1, row.progressWidth-runeLen(row.progress)+1)
	line := progressRowIndent + name + strings.Repeat(" ", namePad)
	line += row.progress + strings.Repeat(" ", progressPad)
	return f.noWrapLine(line + row.elapsed)
}

// suiteProgressText formats known or unknown live per-suite counters.
func suiteProgressText(suite runningSuite) string {
	if suite.totalKnown {
		return fmt.Sprintf("(%d/%d)", suite.completed, suite.total)
	}
	return fmt.Sprintf("(%d/?)", suite.completed)
}

// formatPermanentDuration records completed timings with precise Go duration text.
func formatPermanentDuration(d time.Duration) string {
	if d < 0 {
		panic("formatPermanentDuration received negative duration")
	}
	return d.Round(10 * time.Millisecond).String()
}

// formatRegionHeaderDuration keeps the live header stable without looking blank.
func formatRegionHeaderDuration(d time.Duration) string {
	if d < 0 {
		panic("formatRegionHeaderDuration received negative duration")
	}
	return formatStableDuration(d)
}

// formatRegionRowDuration keeps running rows quiet until elapsed time is useful.
func formatRegionRowDuration(d time.Duration) string {
	if d < 0 {
		panic("formatRegionRowDuration received negative duration")
	}
	if d < time.Second {
		return ""
	}
	return formatStableDuration(d)
}

// formatStableDuration favors stable ticker display width over precision.
func formatStableDuration(d time.Duration) string {
	if d < 0 {
		panic("formatStableDuration received negative duration")
	}
	if d < time.Minute {
		tenths := int((d + 50*time.Millisecond) / (100 * time.Millisecond))
		tenths = min(tenths, 599)
		return fmt.Sprintf("%d.%ds", tenths/10, tenths%10)
	}
	if d < time.Hour {
		totalSeconds := int(d / time.Second)
		return fmt.Sprintf("%dm%02ds", totalSeconds/60, totalSeconds%60)
	}
	totalMinutes := int(d / time.Minute)
	return fmt.Sprintf("%dh%02dm", totalMinutes/60, totalMinutes%60)
}

// noWrapLine enforces the progress-region invariant that rendered lines never wrap.
func (f *Frontend) noWrapLine(line string) string {
	return truncateRunes(line, f.noWrapWidth())
}

// noWrapWidth leaves one terminal cell unused to avoid automatic line wrapping.
func (f *Frontend) noWrapWidth() int {
	return max(f.width-1, 0)
}

// permanentRowWidth returns the no-wrap target for completed suite rows.
func (f *Frontend) permanentRowWidth() int {
	if f.terminal {
		return f.noWrapWidth()
	}
	return f.width
}

// runeLen returns display width for the ASCII-plus-rune text rask renders here.
func runeLen(text string) int {
	return len([]rune(text))
}

// suiteCountsText renders non-zero suite counts in failure-first order.
func suiteCountsText(passed int, failed int, ignored int) string {
	parts := make([]string, 0, 3)
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", passed))
	}
	if ignored > 0 {
		parts = append(parts, fmt.Sprintf("%d ignored", ignored))
	}
	return strings.Join(parts, "; ")
}

// paintSuiteVerb replaces the leading plain verb with its colored form.
func paintSuiteVerb(row string, paintedVerb string, enabled bool) string {
	if !enabled {
		return row
	}
	return paintedVerb + row[suiteVerbWidth:]
}

// testVerdict returns the colored display text for a test outcome.
func (f *Frontend) testVerdict(outcome TestOutcome) string {
	switch outcome {
	case TestPassed:
		return f.paint(colorGreenBold, "ok")
	case TestFailed:
		return f.paint(colorRedBold, "FAILED")
	case TestIgnored:
		return f.paint(colorYellowBold, "ignored")
	default:
		assert.Unreachable("test outcome %q", outcome)
	}
	return ""
}

// failingTests returns tests that should render failure details.
func failingTests(tests []TestResult) []TestResult {
	failures := make([]TestResult, 0, len(tests))
	for _, test := range tests {
		if test.Outcome == TestFailed {
			failures = append(failures, test)
		}
	}
	return failures
}

// countTests returns pass, fail, and skip totals for rendered tests.
func countTests(tests []TestResult) (passed int, failed int, ignored int) {
	for _, test := range tests {
		switch test.Outcome {
		case TestPassed:
			passed++
		case TestFailed:
			failed++
		case TestIgnored:
			ignored++
		default:
			assert.Unreachable("test outcome %q", test.Outcome)
		}
	}
	return passed, failed, ignored
}

// isFrameworkLine reports whether line is generated by the Go test harness.
func isFrameworkLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(trimmed, "=== ") || strings.HasPrefix(trimmed, "--- ")
}

// ShortenImportPath removes modulePath from importPath for compact status text.
func ShortenImportPath(importPath string, modulePath string) string {
	if modulePath == "" || importPath == modulePath {
		return importPath[strings.LastIndex(importPath, "/")+1:]
	}
	return strings.TrimPrefix(importPath, modulePath+"/")
}

// truncateMiddle shortens paths without splitting runes or hiding the leaf.
func truncateMiddle(path string, widthMax int) string {
	runes := []rune(path)
	if len(runes) <= widthMax {
		return path
	}
	if widthMax <= 0 {
		return ""
	}
	if widthMax == 1 {
		return "…"
	}
	if widthMax == 2 {
		return "…" + string(runes[len(runes)-1:])
	}
	// Paths usually share their head and differ near the leaf, so the cut keeps
	// about one third of the post-ellipsis budget from the head and the rest tail.
	budget := widthMax - 1
	head := max(1, budget/3)
	tail := budget - head
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}

// plural returns word or word with "s" based on count.
func plural(count int, word string) string {
	if count == 1 {
		return word
	}
	return word + "s"
}

// terminalWidth returns the detected terminal width or a default.
func terminalWidth() int {
	if columns := os.Getenv("COLUMNS"); columns != "" {
		width, err := strconv.Atoi(columns)
		if err == nil && width >= terminalWidthMin {
			return width
		}
	}
	return sttyWidth()
}

// sttyWidth returns terminal width from stty or the default on failure.
func sttyWidth() int {
	output, err := exec.Command("stty", "-f", "/dev/tty", "size").Output()
	if err != nil {
		output, err = exec.Command("stty", "size").Output()
	}
	if err != nil {
		return terminalWidthDefault
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 {
		return terminalWidthDefault
	}
	width, err := strconv.Atoi(fields[1])
	if err != nil || width < terminalWidthMin {
		return terminalWidthDefault
	}
	return width
}

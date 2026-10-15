package testcmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/cache"
	"github.com/chronark/rask/internal/cli"
	appconfig "github.com/chronark/rask/internal/config"
	"github.com/chronark/rask/internal/discover"
	"github.com/chronark/rask/internal/fingerprint"
	"github.com/chronark/rask/internal/frontend"
	"github.com/chronark/rask/internal/gotool"
	"github.com/chronark/rask/internal/run"
	"github.com/chronark/rask/internal/setup"
)

const (
	// eventChannelCapacity preserves the frontend buffer owned by process setup.
	eventChannelCapacity = setup.EventChannelCapacity
)

const (
	// outcomeSkipped records a suite that never started after run cancellation.
	outcomeSkipped suiteOutcomeKind = iota

	// outcomeKilled records a suite killed by fail-fast or interruption.
	outcomeKilled

	// outcomeCached records a suite served from cache without execution.
	outcomeCached

	// outcomeRan records a suite whose runner result contributes test counts.
	outcomeRan
)

// testCommand owns one linear `rask test` run after process setup.
type testCommand struct {
	ctx          context.Context
	tool         gotool.Tool
	module       gotool.Module
	patternDir   string
	resultCache  suiteCache
	patterns     []string
	noCache      bool
	concurrency  int
	shard        shardOptions
	frontend     *frontend.Frontend
	executor     suiteExecutor
	events       chan<- frontend.Event
	frontendDone chan struct{}
	frontendCode int
	frontendErr  error
	startedAt    time.Time

	// runCtx scopes suite work so fail-fast and Ctrl+C are both one cancel.
	runCtx context.Context

	// cancelRun is safe to call from any worker because cancellation is idempotent.
	cancelRun context.CancelFunc

	// sem bounds simultaneous cache checks and child `go test` processes.
	sem chan struct{}

	// wg proves no worker can send events after run closes the channel.
	wg sync.WaitGroup
}

// testArguments holds parsed package patterns for the test command.
type testArguments struct {
	// patterns are package patterns passed to discovery.
	patterns []string

	// verbose reports whether passed and ignored per-test lines should render.
	verbose bool

	// noCache reports whether cache reads should be bypassed for this run.
	noCache bool

	// concurrency fixes suite parallelism when users need deterministic process fan-out.
	concurrency int
	shard       shardOptions
}

// suiteCache is the cache behavior the test command needs for suites.
type suiteCache interface {
	Get(ctx context.Context, digest string) (cache.Entry, bool)
	Put(ctx context.Context, digest string, entry cache.Entry) error
	Delete(ctx context.Context, digest string)
	Prune() error
	HasRemoteTier() bool
	RemoteStats() cache.RemoteStats
}

// suiteExecutor is the per-suite seam that keeps orchestration tests deterministic.
type suiteExecutor interface {
	Execute(ctx context.Context, task run.SuiteTask, hooks run.Hooks) run.SuiteResult
}

// suiteOutcomeKind keeps worker result folding explicit after all goroutines exit.
type suiteOutcomeKind int

// suiteOutcome records exactly one planned suite result slot.
type suiteOutcome struct {
	kind             suiteOutcomeKind
	entry            cache.Entry
	result           run.SuiteResult
	cacheWriteFailed bool
}

// Action parses package patterns and starts the test run.
func Action(ctx context.Context, command *cli.Command) error {
	environment := setup.FromContext(ctx)
	parsed, err := testArgumentsFromCommand(command)
	if err != nil {
		environment.ExitCode = 1
		return err
	}
	cfg, err := appconfig.Load(environment.Stderr)
	if err != nil {
		environment.ExitCode = 1
		return err
	}
	code, err := runTest(ctx, environment.Tool, cfg, parsed, environment.Frontend, environment.Events)
	environment.ExitCode = code
	return err
}

// Command returns the test command definition.
func Command() *cli.Command {
	return &cli.Command{
		Name:        "test",
		Usage:       "Run Go tests with cached suite results",
		AcceptsArgs: true,
		Aliases:     []string{"t"},
		Flags:       Flags(),
		Action:      Action,
	}
}

// Flags returns the root test flags.
func Flags() []cli.Flag {
	return []cli.Flag{
		cli.Bool("v", "render passing and ignored test lines"),
		cli.Bool("verbose", "render passing and ignored test lines"),
		cli.Bool("no-cache", "bypass cache reads and refresh passing suites"),
		cli.Int("concurrency", "suite workers; 0 or unset uses automatic sizing"),
		cli.String("shard", "run one package shard: INDEX/COUNT (one-based)"),
		cli.String("shard-timings", "balance shards using a shared, immutable JSON timing snapshot"),
	}
}

// runTest resolves process-edge dependencies before orchestration starts.
func runTest(
	ctx context.Context,
	tool gotool.Tool,
	cfg appconfig.Config,
	parsed testArguments,
	ui *frontend.Frontend,
	events chan<- frontend.Event,
) (int, error) {
	userDir, err := os.Getwd()
	if err != nil {
		return 1, fmt.Errorf("resolving working directory: %w", err)
	}
	module, err := tool.CurrentModule(ctx)
	if err != nil {
		return 1, err
	}
	resultCache, err := cache.Open(cache.Config{
		ModulePath: module.Path,
		DiskDir:    cfg.CacheDir,
		Remote:     cfg.Remote,
	})
	if err != nil {
		return 1, err
	}
	command := newTestCommand(ctx, tool, module, userDir, resultCache, parsed, ui, events)
	return command.run()
}

// testArgumentsFromCommand rejects unsafe fan-out before any goroutine starts.
func testArgumentsFromCommand(command *cli.Command) (testArguments, error) {
	parsed := testArguments{
		patterns:    append([]string(nil), command.Args()...),
		verbose:     command.Bool("v") || command.Bool("verbose"),
		noCache:     command.Bool("no-cache"),
		concurrency: command.Int("concurrency"),
	}
	if parsed.concurrency < 0 {
		return testArguments{}, fmt.Errorf("--concurrency must be positive")
	}
	if parsed.concurrency > run.WorkerCountMax {
		return testArguments{}, fmt.Errorf("--concurrency must be at most %d", run.WorkerCountMax)
	}
	shard, err := shardOptionsFromCommand(command)
	if err != nil {
		return testArguments{}, err
	}
	parsed.shard = shard
	if len(parsed.patterns) == 0 {
		parsed.patterns = []string{"./..."}
	}
	return parsed, nil
}

// newTestCommand wires command-owned state after config and cache setup.
func newTestCommand(
	ctx context.Context,
	tool gotool.Tool,
	module gotool.Module,
	userDir string,
	resultCache suiteCache,
	parsed testArguments,
	ui *frontend.Frontend,
	events chan<- frontend.Event,
) *testCommand {
	assert.NotNil(tool)
	assert.NotNil(resultCache)
	assert.NotNil(ui)
	ui.ConfigureRun(parsed.verbose, module.Path)
	return &testCommand{
		ctx: ctx, tool: tool, module: module, patternDir: userDir,
		resultCache: resultCache, patterns: parsed.patterns,
		noCache: parsed.noCache, concurrency: parsed.concurrency,
		shard:    parsed.shard,
		frontend: ui, executor: run.NewExecutor(tool, module.Dir),
		events: events, frontendDone: make(chan struct{}), startedAt: time.Now(),
	}
}

// run keeps cancellation and summary ownership in one goroutine.
func (c *testCommand) run() (int, error) {
	packages, err := discover.Packages(c.ctx, c.tool, c.module, c.patternDir, c.patterns)
	if err != nil && c.ctx.Err() == nil {
		return 1, err
	}
	suites, err := c.suites(packages, err)
	if err != nil {
		return 1, err
	}
	c.startFrontend()
	c.sendRunPlan(suites)
	outcomes := c.runSuites(suites)
	summary := c.foldSummary(suites, outcomes)
	if err := c.resultCache.Prune(); err != nil {
		summary.CacheErrors++
	}
	c.events <- frontend.Event{Kind: frontend.EventSummary, Summary: summary}
	close(c.events)
	<-c.frontendDone
	return c.finish(summary)
}

// suites applies interruption policy around batch fingerprinting.
func (c *testCommand) suites(packages []gotool.Package, previous error) ([]fingerprint.Suite, error) {
	if previous != nil {
		return nil, nil
	}
	packages, err := c.shard.selectPackages(packages)
	if err != nil {
		return nil, err
	}
	suites, err := fingerprint.Suites(c.ctx, c.tool, c.module, packages)
	if err != nil {
		if c.ctx.Err() != nil {
			return nil, nil
		}
		return nil, err
	}
	if c.shard.count != 0 {
		sort.Slice(suites, func(i, j int) bool {
			return c.shard.less(suites[i].Package.ImportPath, suites[j].Package.ImportPath)
		})
	}
	return suites, nil
}

// startFrontend starts rendering after planning inputs are known.
func (c *testCommand) startFrontend() {
	go func() {
		defer close(c.frontendDone)
		c.frontendCode, c.frontendErr = c.frontend.Run(context.Background())
	}()
}

// sendRunPlan tells the frontend total suites and widest selected import path.
func (c *testCommand) sendRunPlan(suites []fingerprint.Suite) {
	width := 0
	for _, suite := range suites {
		path := frontend.ShortenImportPath(suite.Package.ImportPath, c.module.Path)
		width = max(width, len([]rune(path)))
	}
	c.events <- frontend.Event{
		Kind: frontend.EventRunPlan,
		RunPlan: frontend.RunPlan{
			Suites:             len(suites),
			MaxImportPathWidth: width,
		},
	}
}

func (c *testCommand) runSuites(suites []fingerprint.Suite) []suiteOutcome {
	c.runCtx, c.cancelRun = context.WithCancel(c.ctx)
	defer c.cancelRun()
	c.sem = make(chan struct{}, c.workerCount())
	outcomes := make([]suiteOutcome, len(suites))
	for index := range suites {
		select {
		case c.sem <- struct{}{}:
		case <-c.runCtx.Done():
			c.wg.Wait()
			return outcomes
		}
		if c.runCtx.Err() != nil {
			<-c.sem
			break
		}
		c.wg.Add(1)
		go c.runSuiteWorker(suites[index], &outcomes[index])
	}
	c.wg.Wait()
	return outcomes
}

// workerCount resolves the automatic fan-out sentinel once per run.
func (c *testCommand) workerCount() int {
	if c.concurrency != 0 {
		return c.concurrency
	}
	return run.DefaultWorkerCount()
}

// runSuiteWorker writes only its assigned outcome slot, so no result lock is needed.
func (c *testCommand) runSuiteWorker(suite fingerprint.Suite, outcome *suiteOutcome) {
	defer c.wg.Done()
	defer func() { <-c.sem }()
	if c.runCtx.Err() != nil {
		*outcome = suiteOutcome{kind: outcomeSkipped}
		return
	}
	if c.tryCacheHit(suite, outcome) {
		return
	}
	result := c.executeSuite(suite)
	if result.Killed {
		*outcome = suiteOutcome{kind: outcomeKilled, result: result}
		return
	}
	// Worker sends are plain sends because the frontend drains until close, and
	// the orchestrator closes events only after wg.Wait proves no senders remain.
	c.events <- frontend.Event{Kind: frontend.EventSuiteResult, SuiteResult: frontendSuiteResult(result)}
	*outcome = c.recordRunOutcome(result)
}

// tryCacheHit sends cache-hit output from the worker that observed the hit.
func (c *testCommand) tryCacheHit(suite fingerprint.Suite, outcome *suiteOutcome) bool {
	if c.noCache {
		return false
	}
	entry, ok := c.resultCache.Get(c.runCtx, suite.Digest)
	if !ok {
		return false
	}
	*outcome = suiteOutcome{kind: outcomeCached, entry: entry}
	c.events <- frontend.Event{Kind: frontend.EventCached, Cached: frontend.CachedEvent{
		ImportPath: suite.Package.ImportPath,
		Passed:     entry.Passed,
		Ignored:    entry.Ignored,
	}}
	return true
}

// executeSuite adapts runner hooks into frontend events without buffering.
func (c *testCommand) executeSuite(suite fingerprint.Suite) run.SuiteResult {
	task := run.SuiteTask{Package: suite.Package, Digest: suite.Digest}
	return c.executor.Execute(c.runCtx, task, run.Hooks{
		Started: func() {
			c.events <- frontend.Event{Kind: frontend.EventStarted, ImportPath: suite.Package.ImportPath}
		},
		Progress: func(progress run.Progress) {
			c.events <- frontend.Event{
				Kind: frontend.EventTestProgress,
				TestProgress: frontend.TestProgress{
					ImportPath: suite.Package.ImportPath,
					Completed:  progress.Completed,
					Total:      progress.Total,
					TotalKnown: progress.TotalKnown,
				},
			}
		},
	})
}

// recordRunOutcome owns cache side effects for a completed runner result.
func (c *testCommand) recordRunOutcome(result run.SuiteResult) suiteOutcome {
	if result.Failed {
		c.resultCache.Delete(c.ctx, result.Digest)
		c.cancelRun()
		return suiteOutcome{kind: outcomeRan, result: result}
	}
	entry := cacheEntryFromResult(result)
	// Cache writes use the root context: fail-fast must not abort writes for
	// suites that already completed, but Ctrl+C still should.
	err := c.resultCache.Put(c.ctx, result.Digest, entry)
	return suiteOutcome{kind: outcomeRan, result: result, cacheWriteFailed: err != nil}
}

// cacheEntryFromResult stores only the passing summary needed by warm runs.
func cacheEntryFromResult(result run.SuiteResult) cache.Entry {
	passed, _, ignored := countRunnerTests(result.Tests)
	return cache.Entry{
		ImportPath:     result.Package.ImportPath,
		Passed:         passed,
		Ignored:        ignored,
		ElapsedSeconds: result.ElapsedSeconds,
		CreatedAt:      time.Now(),
	}
}

// foldSummary is single-threaded so outcome counters cannot race with workers.
func (c *testCommand) foldSummary(
	suites []fingerprint.Suite,
	outcomes []suiteOutcome,
) frontend.Summary {
	summary := frontend.Summary{Suites: len(suites), Elapsed: time.Since(c.startedAt)}
	for _, outcome := range outcomes {
		outcome.foldInto(&summary)
	}
	summary.Tests = summary.Passed + summary.Failed + summary.Ignored
	summary.Interrupted = c.ctx.Err() != nil
	summary.RemoteConfigured = c.resultCache.HasRemoteTier()
	stats := c.resultCache.RemoteStats()
	summary.RemoteCached = stats.Reads
	summary.RemoteReads = stats.Reads
	summary.RemoteWrites = stats.Writes
	sort.Strings(summary.FailedSuites)
	return summary
}

// foldInto converts one indexed outcome into user-visible summary counters.
func (o suiteOutcome) foldInto(summary *frontend.Summary) {
	switch o.kind {
	case outcomeSkipped, outcomeKilled:
		summary.Skipped++
	case outcomeCached:
		summary.Cached++
		summary.Passed += o.entry.Passed
		summary.Ignored += o.entry.Ignored
	case outcomeRan:
		o.foldRun(summary)
	default:
		assert.Unreachable("suite outcome kind %d", o.kind)
	}
}

// foldRun keeps executed-suite counting beside the outcome that owns the result.
func (o suiteOutcome) foldRun(summary *frontend.Summary) {
	passed, failed, ignored := countRunnerTests(o.result.Tests)
	summary.Passed += passed
	summary.Failed += failed
	summary.Ignored += ignored
	if o.result.Failed {
		summary.FailedSuites = append(summary.FailedSuites, o.result.Package.ImportPath)
	}
	if o.cacheWriteFailed {
		summary.CacheErrors++
	}
}

// finish applies the historical command exit-code priority.
func (c *testCommand) finish(summary frontend.Summary) (int, error) {
	if c.frontendErr != nil {
		return 1, c.frontendErr
	}
	if c.frontendCode != 0 {
		return c.frontendCode, nil
	}
	if len(summary.FailedSuites) > 0 {
		return 1, nil
	}
	return 0, nil
}

// countRunnerTests returns pass, fail, and skip totals for folded runner tests.
func countRunnerTests(tests []run.TestResult) (passed int, failed int, ignored int) {
	for _, test := range tests {
		switch test.Outcome {
		case run.TestPassed:
			passed++
		case run.TestFailed:
			failed++
		case run.TestIgnored:
			ignored++
		default:
			assert.Unreachable("test outcome %q", test.Outcome)
		}
	}
	return passed, failed, ignored
}

// frontendSuiteResult adapts a runner result to the frontend's rendering shape.
func frontendSuiteResult(result run.SuiteResult) frontend.SuiteResult {
	return frontend.SuiteResult{
		ImportPath: result.Package.ImportPath, Tests: frontendTests(result.Tests),
		PackageOutput: result.PackageOutput, ElapsedSeconds: result.ElapsedSeconds,
		Failed: result.Failed, Err: result.Err,
	}
}

// frontendTests adapts runner test results to the frontend's rendering shape.
func frontendTests(tests []run.TestResult) []frontend.TestResult {
	out := make([]frontend.TestResult, 0, len(tests))
	for _, test := range tests {
		out = append(out, frontend.TestResult{
			Name: test.Name, Outcome: frontend.TestOutcome(test.Outcome),
			Output: test.Output, DroppedOutput: test.DroppedOutput,
		})
	}
	return out
}

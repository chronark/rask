# Architecture

rask is a small Go test runner with content-addressed suite caching. It mirrors what `go list` and `go test` already do, then keeps the command core linear: discover the package universe, fingerprint suites, plan rendering, run bounded suite workers, fold one summary.

A run is a straight line down, two cache questions, and a frontend at the end.

```
                            rask
                             │
                       load config once       RASK_CONFIG, cache dir,
                             │                 remote-cache env
                             ▼
                         open cache           disk dir + remote config
                             │
                         discover              selected packages + dependencies
                             │
                        fingerprint            sources + dependency closure +
                             │                 toolchain → one digest per suite
                             │
                   ╭── seen it pass? ──╮       the cache remembers
               yes │                   │ no
                   ▼                   ▼
                Cached            run go test      
               (skipped)               │           
                   │                   │
                   │         ╭── did it pass? ──╮
                   │     yes │                  │ no
                   │         ▼                  ▼
                   │  write to cache        fail fast          
                   │         │                  │              
                   │         │                  │
                   ╰─────────┴────────┬─────────╯
                                      ▼
                                  frontend         print progress
                                      │
                                      ▼
                                 exit 0 · 1
```

## Test flow

`cmd/test` owns orchestration. At the command edge it loads process configuration once, resolves the current module, opens the cache with an explicit disk directory and remote tier, then runs one ordered flow:

1. `discover.Packages(ctx, ...)` returns selected packages and their local dependency closure in one batch.
2. When `--shard` is set, `cmd/test` assigns canonical import paths to shards and marks only this shard's suites selected, retaining the full dependency graph. `fingerprint.Suites(ctx, ...)` returns selected suites and digests in one batch.
3. The frontend goroutine starts and receives one run plan before workers start.
4. A semaphore admits suites in plan order before launching worker goroutines. Unsharded plans keep the randomized fingerprint order; shard plans use import-path order or longest-first snapshot order.
5. Each worker checks the cache, executes one stale suite, writes or deletes cache state, and stores one indexed outcome.
6. After `WaitGroup.Wait`, the command prunes the cache, folds outcomes into the summary, sends `EventSummary`, closes the event channel, and uses the frontend exit code.

Discovery and fingerprinting are batch-shaped because digests need the whole local graph. Running is suite-shaped because cache hits, `go test -json`, and cache writes are independent after each digest exists.

## Events and cancellation

There is one event channel into `frontend.Frontend.Run`. The orchestrator sends the run plan and summary; suite workers send cached, started, progress, and completed-suite events. The frontend owns stdout and stderr, consumes until the channel closes, and returns the final rendered exit code. The orchestrator is the only close owner and closes the event channel only after every worker has exited, so worker sends cannot race with close.

The command creates one run-scoped child context after planning. Fail-fast and Ctrl+C both cancel that run context. A failed suite cancels the run context after deleting its stale cache entry; queued workers that have not started count as skipped, and in-flight executor kills count as skipped. Passing suite cache writes and failing suite deletes use the root context so fail-fast does not abort completed cache side effects, while Ctrl+C still does.

## Packages

| Package | Responsibility |
| --- | --- |
| `main.go` | Executable entrypoint that assembles the root CLI command. |
| `cmd/test` | Root test action, linear orchestration, bounded suite workers, cache policy, exit codes. |
| `cmd/clean` | Cache-cleaning command. |
| `cmd/why` | Cache-key tracing command; adapts fingerprint traces into rendered dependency graphs. |
| `internal/cli` | Trimmed command parser, boolean flags, generated help. |
| `internal/config` | Process-edge config loader for `RASK_CONFIG`, `RASK_CACHE_DIR`, remote-cache env overlays, and `~/.rask.json`. |
| `internal/setup` | Process-bound command dependencies, signal context, frontend setup. |
| `internal/gotool` | Go tool boundary, system implementation, `go list` data shapes. |
| `internal/discover` | Batch package listing, pattern selection, load-error checks. |
| `internal/fingerprint` | Batch build graph ordering and suite digest computation. |
| `internal/cache` | Disk cache store, optional remote tier wiring, entry validation, prune policy. |
| `internal/run` | Single-suite execution, progress hooks, `go test -json` stream folding. |
| `internal/s3` | Minimal stdlib-only SigV4 S3 client for cache `Get`, `Put`, and `Delete`; not a general SDK. |
| `internal/frontend` | Event consumer, suite blocks, sticky TTY progress region, final summary. |
| `internal/assert` | Always-on assertions for programmer errors. |

## Discovery

Discovery first resolves the requested patterns with `go list -json`. A second `go list -deps -json` starts from those packages and their `TestImports` and `XTestImports`. This includes transitive build dependencies of test-only helpers without loading the helpers' own test dependencies or synthetic test-package variants. Rask does not add a whole-module `./...` scan to a narrow selection.

When `GOWORK` names an active workspace, discovery reads every module reported by `go list -m -json`. It also follows local filesystem `replace` directives in each known root's `go.mod`. Only dependency packages inside these roots enter the fingerprint graph; packages in nested roots belong to the deepest matching root. Dependencies are not selected as suites. Module files and environment values remain part of the shared cache salt.

Package `Error` and `Incomplete` fields fail the run even when `go list` exits zero. Syntax parsing adds a second guard for discovered local files. Errors in unrelated packages do not block a narrow selection; selecting `./...` still validates the whole selected tree.

## Fingerprints

Fingerprinting uses two phases. Build digests are ordered with iterative Kahn sorting over `Imports` only. Each build digest covers the salt, import path, non-test files, embed files, and build digests of discovered build imports.

Suite digests are terminal values. Each suite digest covers the salt, import path, test files, test embed files, `testdata`, its own build digest, and build digests of discovered test-only imports. `TestImports` and `XTestImports` are never graph edges, which keeps external-test import cycles finite. Membership in the local dependency graph is discovery closure membership, not import-path prefix matching against the main module path.

The salt and retention details are in [Caching](caching.md).

## Runner

The worker count is `min(GOMAXPROCS-1, 32)`, with a minimum of one. Each stale suite runs:

```bash
go test -json -count=1 -failfast -shuffle=on <package>
```

There is no preliminary test listing or extra test-binary invocation. Test names and outcomes come from the JSON stream. Running progress shows an unknown total because tests that have not started cannot be counted, including tests omitted by selection or fail-fast.

The JSON stream is folded into test outcomes, live completed-test progress, and bounded output. A test still in `run` when the suite completes or crashes is failed. Package output and build output are kept for diagnostics. Scanner lines are capped at 4 MiB, and the remaining stdout is always drained before `Wait` so a stopped scanner cannot deadlock the child.

Each suite also has a five-minute no-output watchdog. It kills a child process that stalls before `go test -timeout` can apply, including compile-phase and lock-wait hangs.

On the first failed suite, orchestration cancels the run context. In-flight test processes are killed by `exec.CommandContext`; queued or unsent stale suites are counted as skipped and not printed.

## Testing tiers

The test suite has three tiers. Hermetic unit and golden tests cover pure package behavior and rendering without touching subprocesses or the user's machine state. Seam tests use fakes at package boundaries such as the Go tool, executor, cache, and frontend to exercise orchestration behavior that would be slow or brittle through a real process tree.

The `e2e` package is the outer tier. It runs as part of ordinary `go test ./...`. Those tests build the real `rask` binary once, scaffold temporary Go modules, run real subprocesses through the real Go toolchain, and isolate every run with a temporary `RASK_CACHE_DIR`.

Output rendering details live in [Output](output.md). Commands and flags live in [CLI reference](cli.md).

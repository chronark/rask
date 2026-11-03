# CLI reference

Testing is the default action. `rask`, `rask test`, and `rask t` all run test suites.

## `rask`

```text
rask [options] [command] [arguments...]
```

With no package patterns, rask uses `./...`.

```sh
rask
rask ./pkg/...
rask -v ./pkg/... --no-cache
rask --concurrency 4 ./...
```

Options:

| Flag | Meaning |
| --- | --- |
| `-v` | Render passing and ignored test lines. |
| `--verbose` | Render passing and ignored test lines. |
| `--no-cache` | Bypass cache reads for this run and refresh passing suites. |
| `--concurrency` | Set how many stale suites run at once. `0` or unset uses automatic sizing. |
| `--x-junit-file <path>` | Write executed test results as JUnit XML. Experimental: this flag and its report format may change or be removed. |
| `--version` | Print the rask version and exit before running tests. |

rask always runs whole suites, always fails fast, and always randomizes order. Suites are scheduled in a randomized dependency-respecting order, and each stale suite runs `go test` with `-shuffle=on`. `--concurrency` controls how many suites are in flight at once — each in-flight suite is one bounded worker doing its cache check and, when stale, one suite-level `go test` process; it does not change intra-suite Go test behavior. When `--concurrency` is unset or `0`, rask uses `min(32, max(1, GOMAXPROCS-1))`. Explicit values must be between `1` and `32`. Failed suite output includes Go's `-test.shuffle <seed>` line when Go emits one, so a shuffled failure can be replayed directly with `go test -shuffle=<seed> <pkg>`. Filtered or customized runs such as `-run`, benchmarks, fuzzing, and coverage belong to plain `go test`. Unknown test flags are rejected with that hint.

### Experimental JUnit output

Use `--x-junit-file` to write a report without changing terminal output or caching:

```bash
rask --x-junit-file test-results/go.xml ./...
```

The path is relative to your working directory. Rask creates missing parent
directories and replaces an existing report. A report-write error makes the
command fail.

Each executed test and subtest has its Go name, package import path as
`classname`, and duration when Go reports one. Failed and skipped tests include
their captured output. Package failures without a failed test appear as an
errored `[package]` entry.

Cached suites and suites not dispatched by the scheduler are omitted. A fully cached run
writes an empty report instead of reporting cached passes as fresh executions.
The report includes available results after a test failure or interruption.
Tests interrupted before a result appear as errors without an invented duration.
Errors before test execution, such as package discovery errors, do not produce
a report.

### Experimental Axiom tracing

Set `RASK_AXIOM_TRACE_TOKEN` in your environment to send test timing spans
directly to Axiom through OTLP/HTTP. Rask uses `RASK_AXIOM_TOKEN` as a fallback
when `RASK_AXIOM_TRACE_TOKEN` is empty. Use an ingest-only token for the selected
dataset. Without a token, tracing stays disabled.

The dataset defaults to `rask-traces`. Set `RASK_AXIOM_DATASET` to override it.

```bash
rask --no-cache ./pkg/testutil/containers
```

The default endpoint is
`https://us-east-1.aws.edge.axiom.co/v1/traces`. Set `RASK_AXIOM_ENDPOINT` to
the full traces URL for another Axiom edge deployment or a local capture server.
Use `https` for remote endpoints. Rask does not start a collector or receiver.

Each executed package has its own trace. Each observed test and subtest has a
span with its outcome, Go-reported duration, and paused duration. Repeated
executions have distinct span IDs. Cached tests produce no execution spans.
Test spans are direct children of the package span because Go test names do not
reliably encode the subtest parent relationship.

Instrumented tests receive the package context in `RASK_TRACEPARENT` only during
execution, not during test enumeration. A helper can allocate a test span ID in
that trace and announce its W3C traceparent with
`t.Attr("rask.test.traceparent.v1", traceparent)`. Rask uses that ID for the
test span. The helper uses it as a non-recording remote parent for operation
spans, not as another recording test span. This protocol requires Go's
structured test attributes and is experimental.

Test helpers can use a private provider and export directly to the same dataset
without changing application providers. The child test processes inherit the
Axiom credentials. Rask does not export
stdout, SQL, or DSNs, but package and test names are exported.

This is a timing aid, not a profiler or a CI speedup by itself. Go JSON timestamps
are observed stream times and can differ from operation timestamps. Package
spans start after enumeration, so they exclude discovery and enumeration costs.
Bounded queues, network failures, and killed processes can lose spans. Rask
limits its final export wait to two seconds and does not change test results
when export fails. The containers helper flushes for at most one second per
instrumented test, which adds some overhead. Package teardown is not instrumented.

## `rask why`

```text
rask why <pattern> [patterns...]
```

```sh
rask why ./internal/cache
rask why ./...
```

`why` answers "why did this suite rerun instead of hitting the cache?" by tracing every input behind each selected suite's cache key. For each matched package it prints the full cache key, the cache verdict (`cached` with the stored counts and age, or `stale`), the module metadata behind the shared salt (`go env`, `go.work`, each local module root's `go.mod` and `go.sum`), the suite's hashed test files and `testdata` fixtures, the package's own build inputs with their build digest, and the transitive in-closure dependency graph as a tree with one abbreviated build digest per package. Test-only imports are listed separately, matching how fingerprinting hashes them. Packages already expanded earlier in a suite block render as `shown above` instead of repeating their subtree. See [Output](output.md#why-reports) for a sample report.

Digests come from the same hashing code path as `rask test`, so a traced cache key always equals the key a test run would compute. To find which dependency invalidated a suite, run `rask why` before and after the unexpected rerun and compare which build digest moved. Selected packages without test files are reported as having nothing to run or cache. At least one package pattern is required, and patterns must stay inside the current module.

## `rask clean`

```text
rask clean [options]
```

```sh
rask clean
rask clean --remote
rask clean --all
rask clean --all --remote
```

Options:

| Flag | Meaning |
| --- | --- |
| `--all` | Remove every rask-owned cache shard under the local cache root. |
| `--remote` | Also purge configured remote cache objects after the local clean succeeds. |

Without `--all`, `clean` removes only the current module's local cache shard. With `--remote`, it also deletes that module's objects from the configured remote cache. With both `--all` and `--remote`, it deletes every object under rask's remote cache format prefix. Passing `--remote` without a configured remote cache fails with `no remote cache configured`.

## Environment

| Variable | Meaning | Default |
| --- | --- | --- |
| `RASK_CONFIG` | Override the config file path. | `~/.rask.json` |
| `RASK_CACHE_DIR` | Root directory of the local disk cache. | user cache dir + `/rask` |
| `RASK_REMOTE_CACHE_BUCKET` | Optional S3-compatible bucket. Setting it opts into the remote cache. | unset |
| `RASK_REMOTE_CACHE_PREFIX` | Optional object key prefix inside the bucket. | empty |
| `RASK_REMOTE_CACHE_ENDPOINT` | S3-compatible endpoint, for example Cloudflare R2. | `https://s3.<region>.amazonaws.com` |
| `RASK_REMOTE_CACHE_REGION` | SigV4 signing region. | `auto` when remote is configured |
| `RASK_REMOTE_CACHE_ACCESS_KEY_ID` | S3-compatible access key. | config file or unset |
| `RASK_REMOTE_CACHE_SECRET_ACCESS_KEY` | S3-compatible secret key. | config file or unset |
| `NO_COLOR` | Disable colors and the sticky progress region. | unset |

Configuration file schema:

```json
{
  "remote_cache": {
    "bucket": "bucket",
    "prefix": "prefix",
    "region": "auto",
    "endpoint": "https://<account>.r2.cloudflarestorage.com",
    "access_key_id": "...",
    "secret_access_key": "..."
  }
}
```

Commands load configuration once at startup.

Global help is available with `--help`, `-h`, or `help`.

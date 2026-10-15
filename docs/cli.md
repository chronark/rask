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
| `--shard INDEX/COUNT` | Run one deterministic package shard. Indices start at 1; empty shards succeed. |
| `--shard-timings FILE` | Balance `--shard` using a shared immutable JSON timing snapshot. |
| `--version` | Print the rask version and exit before running tests. |

rask always runs whole suites and fails fast within each process. Without `--shard`, suites are scheduled in a randomized dependency-respecting order. Shards use canonical import-path order or longest-first snapshot order; see [Package sharding](../README.md#package-sharding) for the snapshot format and worker requirements. Each stale suite still runs `go test` with `-shuffle=on`. `--concurrency` controls how many suites are in flight at once — each in-flight suite is one bounded worker doing its cache check and, when stale, one suite-level `go test` process; it does not change intra-suite Go test behavior. When `--concurrency` is unset or `0`, rask uses `min(32, max(1, GOMAXPROCS-1))`. Explicit values must be between `1` and `32`. Failed suite output includes Go's `-test.shuffle <seed>` line when Go emits one, so a shuffled failure can be replayed directly with `go test -shuffle=<seed> <pkg>`. Filtered or customized runs such as `-run`, benchmarks, fuzzing, and coverage belong to plain `go test`. Unknown test flags are rejected with that hint.

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

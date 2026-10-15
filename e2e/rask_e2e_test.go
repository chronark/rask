package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// interruptPackageCount keeps enough cold-cache misses to expose serial I/O.
	interruptPackageCount = 10

	// interruptRemoteDelay is long enough for sequential misses to dominate exit.
	interruptRemoteDelay = 500 * time.Millisecond

	// interruptSignalDelay lets the binary reach cache dispatch before SIGINT.
	interruptSignalDelay = time.Second

	// interruptExitMax leaves CI slack while still catching sequential remote waits.
	interruptExitMax = 4 * time.Second
)

func TestColdThenWarm(t *testing.T) {
	module := scaffoldModule(t, demoPackages(true)...)
	cacheDir := t.TempDir()
	cold := runRask(t, module.dir, cacheEnv(cacheDir), "./...")
	assertCode(t, cold, 0)
	assertSuites(t, cold.stdout, "Passed", "bystander", "dep", "dependent", "slow")
	warm := runRask(t, module.dir, cacheEnv(cacheDir), "./...")
	assertCode(t, warm, 0)
	if warm.duration >= time.Second {
		t.Fatalf("warm run took %s, want <1s\n%s", warm.duration, warm.stdout)
	}
	assertSuites(t, warm.stdout, "Cached", "bystander", "dep", "dependent", "slow")
	assertContains(t, warm.stdout, "4 cached")
}

func TestJSONExecutionWithoutPreflight(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flags    string
		code     int
		contains []string
		absent   []string
	}{
		{name: "selected", flags: "-run=^TestSelected$", contains: []string{"TestSelected", "2 passed"}, absent: []string{"TestSkipped", "ExampleValue", "FuzzValue"}},
		{name: "missing", flags: "-run=^TestMissing$", contains: []string{"0 passed"}, absent: []string{"TestSelected", "TestSkipped"}},
		{name: "examples and fuzz", flags: "-run=^(TestSelected|TestSkipped|ExampleValue|FuzzValue)$", contains: []string{"TestSelected", "TestSkipped", "ExampleValue", "FuzzValue", "1 ignored"}},
		{name: "failure", flags: "-run=^TestFailure$", code: 1, contains: []string{"TestFailure", "intentional failure"}},
		{name: "timeout", flags: "-run=^TestTimeout$ -timeout=50ms", code: 1, contains: []string{"TestTimeout", "test timed out"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := scaffoldModule(t, packageSpec{name: "suite", test: testPass})
			writeFile(t, module.dir, "suite/suite_test.go", `package suite

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	f, err := os.OpenFile("invocations", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil { panic(err) }
	if _, err := f.WriteString("run\n"); err != nil { panic(err) }
	if err := f.Close(); err != nil { panic(err) }
	os.Exit(m.Run())
}

func TestSelected(t *testing.T) { t.Run("child", func(t *testing.T) {}) }
func TestSkipped(t *testing.T) { t.Skip("intentional skip") }
func TestFailure(t *testing.T) { t.Fatal("intentional failure") }
func TestTimeout(t *testing.T) { time.Sleep(time.Second) }
func ExampleValue() {
	fmt.Println(Value())
	// Output: 1
}
func FuzzValue(f *testing.F) {
	f.Add(1)
	f.Fuzz(func(t *testing.T, n int) {})
}
`)
			env := cacheEnv(t.TempDir())
			env["GOFLAGS"] = tc.flags
			cold := runRask(t, module.dir, env, "-v", "./suite")
			assertCode(t, cold, tc.code)
			for _, want := range tc.contains {
				assertContains(t, cold.stdout+cold.stderr, want)
			}
			for _, unwanted := range tc.absent {
				assertNotContains(t, cold.stdout+cold.stderr, unwanted)
			}
			second := runRask(t, module.dir, env, "-v", "./suite")
			assertCode(t, second, tc.code)
			wantInvocations := "run\n"
			if tc.code == 0 {
				assertSuites(t, second.stdout, "Cached", "suite")
			} else {
				wantInvocations += "run\n"
				assertNotContains(t, second.stdout, "Cached")
			}
			if tc.name == "selected" {
				env["GOFLAGS"] = "-run=^TestSkipped$"
				changed := runRask(t, module.dir, env, "-v", "./suite")
				assertCode(t, changed, 0)
				assertSuites(t, changed.stdout, "Passed", "suite")
				assertContains(t, changed.stdout, "1 ignored")
				assertNotContains(t, changed.stdout, "TestSelected")
				wantInvocations += "run\n"
			}
			got, err := os.ReadFile(filepath.Join(module.dir, "suite", "invocations"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != wantInvocations {
				t.Fatalf("TestMain invocations = %q, want %q", got, wantInvocations)
			}
		})
	}
}

func TestEditInvalidation(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	cacheDir := t.TempDir()
	assertCode(t, runRask(t, module.dir, cacheEnv(cacheDir), "./..."), 0)
	editDepSource(t, module)
	result := runRask(t, module.dir, cacheEnv(cacheDir), "./...")
	assertCode(t, result, 0)
	assertSuites(t, result.stdout, "Passed", "dep", "dependent")
	assertSuites(t, result.stdout, "Cached", "bystander")
	assertNotContains(t, result.stdout, "Passed bystander")
}

func TestNarrowSelectionIgnoresUnrelatedPackages(t *testing.T) {
	module := scaffoldModule(t,
		packageSpec{name: "dep", test: testNone},
		packageSpec{name: "middle", imports: []string{"dep"}, test: testNone},
		packageSpec{name: "dependent", imports: []string{"middle"}, test: testPass},
	)
	writeFile(t, module.dir, "unrelated/broken.go", "package unrelated\nimport _ \"example.invalid/unavailable\"\n")
	env := cacheEnv(t.TempDir())
	env["GOPROXY"] = "off"
	env["GOMODCACHE"] = t.TempDir()
	cold := runRask(t, module.dir, env, "./dependent")
	assertCode(t, cold, 0)
	assertSuites(t, cold.stdout, "Passed", "dependent")
	assertNotContains(t, cold.stdout, "Passed dep ")
	assertSuites(t, runRask(t, filepath.Join(module.dir, "dependent"), env, "./...").stdout, "Cached", "dependent")
	writeFile(t, module.dir, "unrelated/broken.go", "package unrelated\nfunc Broken( {\n")
	assertSuites(t, runRask(t, module.dir, env, module.path+"/dependent").stdout, "Cached", "dependent")
	full := runRask(t, module.dir, env, "./...")
	assertCode(t, full, 1)
	assertContains(t, full.stderr, "unrelated")
	editDepSource(t, module)
	changed := runRask(t, module.dir, env, "./dependent")
	assertCode(t, changed, 0)
	assertSuites(t, changed.stdout, "Passed", "dependent")
	writeFile(t, module.dir, "dep/dep.go", "package dep\nfunc Value() int { return -10 }\n")
	failed := runRask(t, module.dir, env, "./dependent")
	assertCode(t, failed, 1)
	assertContains(t, failed.stdout+failed.stderr, "bad value")
	assertNotContains(t, failed.stdout, "Cached")
}

func TestNarrowSelectionInvalidatesLocalTestDependencies(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		t.Run(fmt.Sprintf("workspace=%t", workspace), func(t *testing.T) {
			root := t.TempDir()
			appDir := filepath.Join(root, "app")
			goMod := "module example.test/app\n\ngo 1.25\nrequire example.test/dependency v0.0.0\n"
			env := cacheEnv(t.TempDir())
			env["GOPROXY"] = "off"
			if workspace {
				writeFile(t, root, "go.work", "go 1.27.1\nuse (\n./app\n./dependency\n)\n")
				env["GOWORK"] = filepath.Join(root, "go.work")
			} else {
				goMod += "replace example.test/dependency => ../dependency\n"
			}
			writeFile(t, appDir, "go.mod", goMod)
			writeFile(t, appDir, "api/api.go", "package api\nfunc Value() int { return 1 }\n")
			writeFile(t, appDir, "api/api_test.go", `package api_test
import (
    "testing"
    "example.test/app/helper"
)
func TestValue(t *testing.T) {
    if helper.Value() != 1 { t.Fatal("dependency changed") }
}
`)
			writeFile(t, appDir, "helper/helper.go", `package helper
import (
    "example.test/app/api"
    "example.test/dependency"
)
func Value() int { return api.Value() + dependency.Value() - 1 }
`)
			writeFile(t, root, "dependency/go.mod", "module example.test/dependency\n\ngo 1.25\n")
			writeFile(t, root, "dependency/dep.go", "package dependency\nfunc Value() int { return 1 }\n")
			writeFile(t, root, "dependency/unrelated/bad.go", "package unrelated\nimport _ \"example.invalid/unavailable\"\n")
			cold := runRask(t, appDir, env, "./api")
			assertCode(t, cold, 0)
			assertSuites(t, cold.stdout, "Passed", "api")
			warm := runRask(t, appDir, env, "./api")
			assertCode(t, warm, 0)
			assertSuites(t, warm.stdout, "Cached", "api")
			writeFile(t, root, "dependency/dep.go", "package dependency\nfunc Value() int { return 2 }\n")
			changed := runRask(t, appDir, env, "./api")
			assertCode(t, changed, 1)
			assertContains(t, changed.stdout+changed.stderr, "dependency changed")
			assertNotContains(t, changed.stdout, "Cached")
		})
	}
}

func TestFailFast(t *testing.T) {
	packages := append(demoPackages(false), packageSpec{name: "failer", test: testFail})
	packages = append(packages, packageSpec{name: "slow", test: testPass, sleep: 2 * time.Second})
	module := scaffoldModule(t, packages...)
	result := runRask(t, module.dir, cacheEnv(t.TempDir()), "./...")
	assertCode(t, result, 1)
	combined := result.stdout + result.stderr
	assertContains(t, combined, "error: test failed in failer")
	assertContains(t, combined, "skipped")
	waitNoGoTestProcesses(t, module.path)
}

func TestBrokenPackage(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "broken", test: testPass})
	writeFile(t, module.dir, filepath.Join("broken", "broken.go"), "package broken\n\nfunc Broken( {\n")
	result := runRask(t, module.dir, cacheEnv(t.TempDir()), "./...")
	assertCode(t, result, 1)
	assertContains(t, result.stderr, "broken")
	assertNotContains(t, result.stdout, "Passed")
	assertNotContains(t, result.stdout, "Finished")
}

func TestNoCacheFlag(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	cacheDir := t.TempDir()
	assertCode(t, runRask(t, module.dir, cacheEnv(cacheDir), "./..."), 0)
	assertSuites(t, runRask(t, module.dir, cacheEnv(cacheDir), "./...").stdout, "Cached", "bystander", "dep", "dependent")
	result := runRask(t, module.dir, cacheEnv(cacheDir), "--no-cache", "./...")
	assertCode(t, result, 0)
	assertSuites(t, result.stdout, "Passed", "bystander", "dep", "dependent")
	assertNotContains(t, result.stdout, "Cached ")
	third := runRask(t, module.dir, cacheEnv(cacheDir), "./...")
	assertSuites(t, third.stdout, "Cached", "bystander", "dep", "dependent")
}

func TestConcurrencyFlag(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "dep", test: testPass})
	success := runRask(t, module.dir, cacheEnv(t.TempDir()), "--concurrency", "1", "./...")
	assertCode(t, success, 0)
	assertSuites(t, success.stdout, "Passed", "dep")
	failure := runRask(t, module.dir, cacheEnv(t.TempDir()), "--concurrency", "-1")
	assertCode(t, failure, 1)
	assertContains(t, failure.stderr, "--concurrency must be positive")
}

func TestCleanCommand(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	cacheDir := t.TempDir()
	assertCode(t, runRask(t, module.dir, cacheEnv(cacheDir), "./..."), 0)
	assertSuites(t, runRask(t, module.dir, cacheEnv(cacheDir), "./...").stdout, "Cached", "dep")
	clean := runRask(t, module.dir, cacheEnv(cacheDir), "clean")
	assertCode(t, clean, 0)
	assertContains(t, clean.stdout, "Removed "+module.path)
	assertSuites(t, runRask(t, module.dir, cacheEnv(cacheDir), "./...").stdout, "Passed", "dep")
	assertForeignCacheRefused(t, module)
}

func TestWhyCommand(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	cacheDir := t.TempDir()
	cold := runRask(t, module.dir, cacheEnv(cacheDir), "why", "./dependent")
	assertCode(t, cold, 0)
	assertContains(t, cold.stdout, "Suite "+module.path+"/dependent")
	assertContains(t, cold.stdout, "stale")
	assertContains(t, cold.stdout, "└─ dep ")
	assertContains(t, cold.stdout, "go.mod")
	assertNotContains(t, cold.stdout, "Suite "+module.path+"/bystander")
	assertCode(t, runRask(t, module.dir, cacheEnv(cacheDir), "./..."), 0)
	warm := runRask(t, module.dir, cacheEnv(cacheDir), "why", "./dependent")
	assertCode(t, warm, 0)
	assertContains(t, warm.stdout, "cached")
	assertNotContains(t, warm.stdout, "stale")
	missing := runRask(t, module.dir, cacheEnv(cacheDir), "why")
	assertCode(t, missing, 1)
	assertContains(t, missing.stderr, "why needs a package pattern")
}

func TestTiered(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	remoteEnv, stop := startFakeS3(t)
	assertTieredScenario(t, module, remoteEnv, func(env map[string]string) {
		env["RASK_REMOTE_CACHE_ACCESS_KEY_ID"] = "wrong"
	}, stop)
}

func TestCleanRemotePurgesFakeS3(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	remoteEnv, _ := startFakeS3(t)
	machineA := envWithCache(t.TempDir(), remoteEnv)
	assertCode(t, runRask(t, module.dir, machineA, "./..."), 0)
	clean := runRask(t, module.dir, machineA, "clean", "--remote")
	assertCode(t, clean, 0)
	assertContains(t, clean.stdout, "Removed "+module.path)
	assertContains(t, clean.stdout, "remote objects")
	cold := runRask(t, module.dir, envWithCache(t.TempDir(), remoteEnv), "./...")
	assertCode(t, cold, 0)
	assertSuites(t, cold.stdout, "Passed", "bystander", "dep", "dependent")
	assertNotContains(t, cold.stdout, "Cached ")
	assertContains(t, cold.stdout, "remote: 0 reads")
	noRemote := runRask(t, module.dir, cacheEnv(t.TempDir()), "clean", "--remote")
	assertCode(t, noRemote, 1)
	assertContains(t, noRemote.stderr, "no remote cache configured")
}

func TestTieredMinIO(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	remoteEnv := startMinio(t)
	assertTieredScenario(t, module, remoteEnv, func(env map[string]string) {
		env["RASK_REMOTE_CACHE_SECRET_ACCESS_KEY"] = "wrong"
	}, func() {})
}

func TestTieredExternalS3(t *testing.T) {
	bucket := os.Getenv("RASK_E2E_S3_BUCKET")
	if bucket == "" {
		t.Skip("RASK_E2E_S3_BUCKET is unset; skipping real S3-compatible e2e")
	}
	required := []string{"RASK_E2E_S3_ACCESS_KEY_ID", "RASK_E2E_S3_SECRET_ACCESS_KEY"}
	for _, name := range required {
		if os.Getenv(name) == "" {
			t.Skipf("%s is unset; skipping real S3-compatible e2e", name)
		}
	}
	module := scaffoldModule(t, demoPackages(false)...)
	remoteEnv := externalS3Env(bucket)
	assertTieredScenario(t, module, remoteEnv, func(env map[string]string) {
		env["RASK_REMOTE_CACHE_SECRET_ACCESS_KEY"] = "wrong"
	}, func() {})
}

// TestInterruptDuringSlowRemoteCacheLookups protects user-visible SIGINT latency.
func TestInterruptDuringSlowRemoteCacheLookups(t *testing.T) {
	module := scaffoldModule(t, interruptPackages()...)
	remoteEnv, _ := startDelayedFakeS3(t, interruptRemoteDelay)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, raskBinary, "./...")
	command.Dir = module.dir
	command.Env = baseEnv(envWithCache(t.TempDir(), remoteEnv))
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start rask: %v", err)
	}
	time.Sleep(interruptSignalDelay)
	startedAt := time.Now()
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal rask: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	err := command.Wait()
	elapsed := time.Since(startedAt)
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("rask timed out\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 130 {
		t.Fatalf("exit=%v err=%v, want 130\nstdout:\n%s\nstderr:\n%s",
			exitCodeForError(err), err, stdout.String(), stderr.String())
	}
	if elapsed > interruptExitMax {
		t.Fatalf("SIGINT exit took %s, want under %s\nstdout:\n%s\nstderr:\n%s",
			elapsed, interruptExitMax, stdout.String(), stderr.String())
	}
}

func externalS3Env(bucket string) map[string]string {
	env := map[string]string{
		"RASK_REMOTE_CACHE_BUCKET":            bucket,
		"RASK_REMOTE_CACHE_ACCESS_KEY_ID":     os.Getenv("RASK_E2E_S3_ACCESS_KEY_ID"),
		"RASK_REMOTE_CACHE_SECRET_ACCESS_KEY": os.Getenv("RASK_E2E_S3_SECRET_ACCESS_KEY"),
	}
	copyE2ES3Env(env, "PREFIX", "ENDPOINT", "REGION")
	return env
}

// interruptPackages creates enough independent misses to expose serial remotes.
func interruptPackages() []packageSpec {
	packages := make([]packageSpec, 0, interruptPackageCount)
	for index := range interruptPackageCount {
		packages = append(packages, packageSpec{name: fmt.Sprintf("pkg%02d", index), test: testPass})
	}
	return packages
}

// exitCodeForError keeps failure output useful when Wait returns a different error.
func exitCodeForError(err error) int {
	exit, ok := err.(*exec.ExitError)
	if !ok {
		return -1
	}
	return exit.ExitCode()
}

func assertTieredScenario(
	t *testing.T,
	module moduleFixture,
	remoteEnv map[string]string,
	breakCredentials func(map[string]string),
	stop func(),
) {
	t.Helper()
	machineA := envWithCache(t.TempDir(), remoteEnv)
	assertCode(t, runRask(t, module.dir, machineA, "./..."), 0)
	machineB := envWithCache(t.TempDir(), remoteEnv)
	machineBRemote := runRask(t, module.dir, machineB, "./...")
	assertCode(t, machineBRemote, 0)
	assertSuites(t, machineBRemote.stdout, "Cached", "bystander", "dep", "dependent")
	assertContains(t, machineBRemote.stdout, "3 cached (3 remote) | remote: 3 reads, 0 writes")
	badEnv := copyMap(remoteEnv)
	breakCredentials(badEnv)
	result := runRask(t, module.dir, envWithCache(t.TempDir(), badEnv), "./...")
	assertCode(t, result, 0)
	assertSuites(t, result.stdout, "Passed", "bystander", "dep", "dependent")
	assertNotContains(t, result.stdout, "Cached ")
	stop()
	assertSuites(t, runRask(t, module.dir, machineB, "./...").stdout, "Cached", "bystander", "dep", "dependent")
}

func TestStallWatchdog(t *testing.T) {
	t.Skip("rask's subprocess stall watchdog is intentionally fixed at 5m; e2e keeps this gap documented instead of adding a test-only flag or a slow suite.")
}

func TestVerbose(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "dep", test: testPass})
	quiet := runRask(t, module.dir, cacheEnv(t.TempDir()), "./...")
	assertCode(t, quiet, 0)
	assertNotContains(t, quiet.stdout, "test TestDep ... ok")
	verbose := runRask(t, module.dir, cacheEnv(t.TempDir()), "-v", "./...")
	assertCode(t, verbose, 0)
	assertContains(t, verbose.stdout, "test TestDep ... ok")
	failing := scaffoldModule(t, packageSpec{name: "failer", test: testFail})
	failed := runRask(t, failing.dir, cacheEnv(t.TempDir()), "./...")
	assertCode(t, failed, 1)
	assertContains(t, failed.stdout, "test TestFailer ... FAILED")
}

func assertForeignCacheRefused(t *testing.T, module moduleFixture) {
	t.Helper()
	foreign := t.TempDir()
	tag := filepath.Join(foreign, "CACHEDIR.TAG")
	sentinel := filepath.Join(foreign, "sentinel")
	if err := os.WriteFile(tag, []byte("Signature: foreign\n"), 0o644); err != nil {
		t.Fatalf("write foreign tag: %v", err)
	}
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	result := runRask(t, module.dir, cacheEnv(foreign), "clean")
	assertCode(t, result, 1)
	assertContains(t, result.stderr, "non-rask CACHEDIR.TAG")
	assertFileContains(t, tag, "Signature: foreign\n")
	assertFileContains(t, sentinel, "keep")
}

func envWithCache(cacheDir string, extra map[string]string) map[string]string {
	env := cacheEnv(cacheDir)
	for key, value := range extra {
		env[key] = value
	}
	return env
}

func copyMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func copyE2ES3Env(target map[string]string, names ...string) {
	for _, name := range names {
		key := "RASK_E2E_S3_" + name
		if value := os.Getenv(key); value != "" {
			target["RASK_REMOTE_CACHE_"+name] = value
		}
	}
}

func waitNoGoTestProcesses(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processes := goTestProcesses(t, marker); processes == "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("orphaned go test process remains:\n%s", goTestProcesses(t, marker))
}

func goTestProcesses(t *testing.T, marker string) string {
	t.Helper()
	output, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("process listing unavailable in this sandbox: %v", err)
		}
		t.Fatalf("list processes: %v", err)
	}
	var matches []string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "go test") && strings.Contains(line, marker) {
			matches = append(matches, strings.TrimSpace(line))
		}
	}
	return strings.Join(matches, "\n")
}

func assertSuites(t *testing.T, output string, verb string, names ...string) {
	t.Helper()
	for _, name := range names {
		assertContains(t, output, fmt.Sprintf("%s %s", verb, name))
	}
}

func assertCode(t *testing.T, result raskResult, want int) {
	t.Helper()
	if result.code != want {
		t.Fatalf("exit code=%d, want %d\nstdout:\n%s\nstderr:\n%s", result.code, want, result.stdout, result.stderr)
	}
}

func assertContains(t *testing.T, text string, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("missing %q in:\n%s", want, text)
	}
}

func assertNotContains(t *testing.T, text string, unwanted string) {
	t.Helper()
	if strings.Contains(text, unwanted) {
		t.Fatalf("unexpected %q in:\n%s", unwanted, text)
	}
}

func assertFileContains(t *testing.T, path string, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(body) != want {
		t.Fatalf("%s = %q, want %q", path, body, want)
	}
}

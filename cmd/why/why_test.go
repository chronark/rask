package why

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chronark/rask/internal/cache"
	"github.com/chronark/rask/internal/cli"
	"github.com/chronark/rask/internal/frontend"
	"github.com/chronark/rask/internal/gotool"
	"github.com/chronark/rask/internal/setup"
)

const (
	// whyEventChannelCapacity matches the production render buffer for tests.
	whyEventChannelCapacity = 256
)

// TestWhyRequiresPattern protects the explicit-pattern contract.
func TestWhyRequiresPattern(t *testing.T) {
	setWhyEnv(t)
	code, _, errOut := executeWhy(t, &whyTool{}, "why")
	if code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
	if !strings.Contains(errOut, "why needs a package pattern") {
		t.Fatalf("stderr=%q, want pattern hint", errOut)
	}
}

// TestWhyTracesSuiteInputs protects discovery, tracing, and cache-status wiring.
func TestWhyTracesSuiteInputs(t *testing.T) {
	setWhyEnv(t)
	tool := whyToolFixture(t)
	fake := &fakeSuiteCache{}
	withOpenCacheSeam(t, func(cfg cache.Config) (suiteCache, error) {
		if cfg.ModulePath != tool.module.Path {
			t.Fatalf("module path=%q", cfg.ModulePath)
		}
		return fake, nil
	})
	code, out, errOut := executeWhy(t, tool, "why", "./...")
	if code != 0 {
		t.Fatalf("code=%d, want 0\nstderr:%s", code, errOut)
	}
	for _, want := range []string{
		"Suite " + tool.module.Path + "/dep",
		"Suite " + tool.module.Path + "/dependent",
		"status     stale",
		"└─ dep ",
		filepath.Join(tool.module.Dir, "go.mod"),
		"dependent_test.go",
		"Traced 2 suites: 2 stale",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if len(fake.digests) != 2 {
		t.Fatalf("cache checked %d digests, want 2", len(fake.digests))
	}
}

// TestWhyReportsCachedSuites protects the cached verdict path.
func TestWhyReportsCachedSuites(t *testing.T) {
	setWhyEnv(t)
	tool := whyToolFixture(t)
	withOpenCacheSeam(t, func(cache.Config) (suiteCache, error) {
		return &fakeSuiteCache{cached: true}, nil
	})
	code, out, errOut := executeWhy(t, tool, "why", "./...")
	if code != 0 {
		t.Fatalf("code=%d, want 0\nstderr:%s", code, errOut)
	}
	if !strings.Contains(out, "status     cached") || strings.Contains(out, "stale") {
		t.Fatalf("stdout=%q, want cached verdicts only", out)
	}
}

// withOpenCacheSeam swaps cache construction for one focused command test.
func withOpenCacheSeam(t *testing.T, open func(cache.Config) (suiteCache, error)) {
	t.Helper()
	oldOpen := openCache
	openCache = open
	t.Cleanup(func() { openCache = oldOpen })
}

// executeWhy runs the why command against a fake process environment.
func executeWhy(t *testing.T, tool gotool.Tool, arguments ...string) (int, string, string) {
	t.Helper()
	oldEnvironment := setup.BuildEnvironment
	out := new(strings.Builder)
	errOut := new(strings.Builder)
	setup.BuildEnvironment = func() *setup.Environment {
		events := make(chan frontend.Event, whyEventChannelCapacity)
		return &setup.Environment{
			Tool: tool, Stdout: out, Stderr: errOut,
			Frontend: frontend.New(out, errOut, false, events),
			Events:   events,
		}
	}
	t.Cleanup(func() { setup.BuildEnvironment = oldEnvironment })
	root := &cli.Command{Name: "rask", Commands: []*cli.Command{Command()}}
	code := setup.Execute(root, append([]string{"rask"}, arguments...))
	return code, out.String(), errOut.String()
}

func setWhyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RASK_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	t.Setenv("RASK_REMOTE_CACHE_BUCKET", "")
}

// whyToolFixture creates a real module tree served by a fake go tool.
func whyToolFixture(t *testing.T) *whyTool {
	t.Helper()
	dir := t.TempDir()
	module := gotool.Module{Path: "example.com/m", Dir: dir}
	writeWhyFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.26\n")
	dep := whyPackage(module, "dep", nil)
	dependent := whyPackage(module, "dependent", []string{dep.ImportPath})
	for _, p := range []gotool.Package{dep, dependent} {
		writeWhyFile(t, p.Dir, p.GoFiles[0], "package "+p.Name+"\n")
		writeWhyFile(t, p.Dir, p.TestGoFiles[0], "package "+p.Name+"\n")
	}
	return &whyTool{module: module, packages: []gotool.Package{dep, dependent}}
}

// whyPackage returns one fixture package rooted inside the fake module.
func whyPackage(module gotool.Module, name string, imports []string) gotool.Package {
	return gotool.Package{
		Dir: filepath.Join(module.Dir, name), ImportPath: module.Path + "/" + name,
		Name: name, GoFiles: []string{name + ".go"}, TestGoFiles: []string{name + "_test.go"},
		Imports: imports,
	}
}

func writeWhyFile(t *testing.T, dir string, name string, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// whyTool serves a fixed package universe for command tests.
type whyTool struct {
	module   gotool.Module
	packages []gotool.Package
}

func (t *whyTool) CurrentModule(context.Context) (gotool.Module, error) {
	return t.module, nil
}

func (t *whyTool) ListPackages(context.Context, string, []string) ([]gotool.Package, error) {
	return append([]gotool.Package(nil), t.packages...), nil
}

func (t *whyTool) ListDependencies(context.Context, string, []string) ([]gotool.Package, error) {
	return append([]gotool.Package(nil), t.packages...), nil
}

func (t *whyTool) ListModules(context.Context, string) ([]gotool.Module, error) {
	return []gotool.Module{t.module}, nil
}

func (t *whyTool) Environment(context.Context, string) ([]byte, error) {
	return []byte("GOARCH='arm64'\nGOOS='darwin'\nGOVERSION='go1.26'\nGOWORK=''\n"), nil
}

func (t *whyTool) ListTests(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (t *whyTool) StartTests(context.Context, string, string) (gotool.TestProcess, error) {
	return nil, fmt.Errorf("unexpected StartTests")
}

// fakeSuiteCache records digest lookups and returns one fixed verdict.
type fakeSuiteCache struct {
	cached  bool
	digests []string
}

func (c *fakeSuiteCache) Get(_ context.Context, digest string) (cache.Entry, bool) {
	c.digests = append(c.digests, digest)
	return cache.Entry{}, c.cached
}

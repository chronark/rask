package clean

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chronark/rask/internal/cache"
	"github.com/chronark/rask/internal/cli"
	appconfig "github.com/chronark/rask/internal/config"
	"github.com/chronark/rask/internal/frontend"
	"github.com/chronark/rask/internal/gotool"
	"github.com/chronark/rask/internal/setup"
)

const (
	// cleanEventChannelCapacity matches the production render buffer for tests.
	cleanEventChannelCapacity = 256
)

// TestCleanRemoteRequiresConfig protects the explicit remote opt-in error.
func TestCleanRemoteRequiresConfig(t *testing.T) {
	setCleanEnv(t)
	code, _, errOut := executeClean(t, cleanToolForTest(), "clean", "--remote")
	if code != 1 {
		t.Fatalf("code=%d, want 1", code)
	}
	if !strings.Contains(errOut, "no remote cache configured") {
		t.Fatalf("stderr=%q, want no remote error", errOut)
	}
}

// TestCleanRemoteHint protects the actionable no-purge hint.
func TestCleanRemoteHint(t *testing.T) {
	setCleanEnv(t)
	setRemoteEnv(t)
	fake := &fakeModuleCache{remote: true}
	withCleanSeams(t, cleanSeams{open: func(cfg cache.Config) (moduleCache, error) {
		if cfg.ModulePath != "example.com/m" {
			t.Fatalf("module path=%q", cfg.ModulePath)
		}
		return fake, nil
	}})
	code, out, errOut := executeClean(t, cleanToolForTest(), "clean")
	if code != 0 {
		t.Fatalf("code=%d, want 0\nstderr:%s", code, errOut)
	}
	if !fake.removed || strings.Contains(out, "remote object") {
		t.Fatalf("out=%q removed=%v, want local-only clean", out, fake.removed)
	}
	if !strings.Contains(errOut, remoteHint) {
		t.Fatalf("stderr=%q, want remote hint", errOut)
	}
}

// TestCleanRemotePurgesModule protects module-scoped remote purge wiring.
func TestCleanRemotePurgesModule(t *testing.T) {
	setCleanEnv(t)
	setRemoteEnv(t)
	fake := &fakeModuleCache{remote: true, purgeCount: 12}
	withCleanSeams(t, cleanSeams{open: func(cache.Config) (moduleCache, error) {
		return fake, nil
	}})
	code, out, errOut := executeClean(t, cleanToolForTest(), "clean", "--remote")
	if code != 0 {
		t.Fatalf("code=%d, want 0\nstderr:%s", code, errOut)
	}
	if !fake.removed || !fake.purged {
		t.Fatalf("removed=%v purged=%v, want both", fake.removed, fake.purged)
	}
	if !strings.Contains(out, "Removed example.com/m") ||
		!strings.Contains(out, "Removed 12 remote objects") {
		t.Fatalf("stdout=%q, want local and remote counts", out)
	}
}

// TestCleanAllRemotePurgesWholeStore protects clean --all --remote wiring.
func TestCleanAllRemotePurgesWholeStore(t *testing.T) {
	setCleanEnv(t)
	setRemoteEnv(t)
	seams := cleanSeams{
		cleanAll: func(path string) error {
			if path == "" {
				t.Fatalf("empty cache path")
			}
			return nil
		},
		purge: func(_ context.Context, remote *appconfig.Remote) (int, error) {
			if remote == nil || remote.Bucket != "rask-cmd" {
				t.Fatalf("remote=%+v, want configured remote", remote)
			}
			return 3, nil
		},
	}
	withCleanSeams(t, seams)
	code, out, errOut := executeClean(t, cleanToolForTest(), "clean", "--all", "--remote")
	if code != 0 {
		t.Fatalf("code=%d, want 0\nstderr:%s", code, errOut)
	}
	if !strings.Contains(out, "Removed 3 remote objects") {
		t.Fatalf("stdout=%q, want remote count", out)
	}
}

// cleanSeams groups command dependencies swapped by focused command tests.
type cleanSeams struct {
	cleanAll func(string) error
	open     func(cache.Config) (moduleCache, error)
	purge    func(context.Context, *appconfig.Remote) (int, error)
}

func withCleanSeams(t *testing.T, seams cleanSeams) {
	t.Helper()
	oldCleanAll, oldOpen, oldPurge := cleanAllCache, openCache, purgeRemote
	if seams.cleanAll != nil {
		cleanAllCache = seams.cleanAll
	}
	if seams.open != nil {
		openCache = seams.open
	}
	if seams.purge != nil {
		purgeRemote = seams.purge
	}
	t.Cleanup(func() {
		cleanAllCache, openCache, purgeRemote = oldCleanAll, oldOpen, oldPurge
	})
}

func executeClean(t *testing.T, tool gotool.Tool, arguments ...string) (int, string, string) {
	t.Helper()
	oldEnvironment := setup.BuildEnvironment
	out := new(strings.Builder)
	errOut := new(strings.Builder)
	setup.BuildEnvironment = func() *setup.Environment {
		events := make(chan frontend.Event, cleanEventChannelCapacity)
		return &setup.Environment{
			Tool: tool, Stdout: out, Stderr: errOut,
			Frontend: frontend.New(out, errOut, false, events),
			Events:   events,
		}
	}
	t.Cleanup(func() { setup.BuildEnvironment = oldEnvironment })
	code := setup.Execute(cleanRootForTest(), append([]string{"rask"}, arguments...))
	return code, out.String(), errOut.String()
}

func cleanRootForTest() *cli.Command {
	return &cli.Command{Name: "rask", Commands: []*cli.Command{Command()}}
}

func setCleanEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RASK_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("RASK_CACHE_DIR", t.TempDir())
	for _, name := range []string{
		"RASK_REMOTE_CACHE_BUCKET",
		"RASK_REMOTE_CACHE_PREFIX",
		"RASK_REMOTE_CACHE_ENDPOINT",
		"RASK_REMOTE_CACHE_REGION",
		"RASK_REMOTE_CACHE_ACCESS_KEY_ID",
		"RASK_REMOTE_CACHE_SECRET_ACCESS_KEY",
	} {
		t.Setenv(name, "")
	}
}

func setRemoteEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RASK_REMOTE_CACHE_BUCKET", "rask-cmd")
	t.Setenv("RASK_REMOTE_CACHE_PREFIX", "prefix")
	t.Setenv("RASK_REMOTE_CACHE_ENDPOINT", "http://rask.test")
	t.Setenv("RASK_REMOTE_CACHE_REGION", "us-east-1")
	t.Setenv("RASK_REMOTE_CACHE_ACCESS_KEY_ID", "access")
	t.Setenv("RASK_REMOTE_CACHE_SECRET_ACCESS_KEY", "secret")
}

func cleanToolForTest() cleanTool {
	return cleanTool{module: gotool.Module{Path: "example.com/m", Dir: "/tmp/module"}}
}

type cleanTool struct {
	module gotool.Module
}

func (t cleanTool) CurrentModule(context.Context) (gotool.Module, error) {
	return t.module, nil
}

func (t cleanTool) ListPackages(context.Context, string, []string) ([]gotool.Package, error) {
	return nil, nil
}

func (t cleanTool) ListDependencies(context.Context, string, []string) ([]gotool.Package, error) {
	return nil, nil
}

func (t cleanTool) ListModules(context.Context, string) ([]gotool.Module, error) {
	return nil, nil
}

func (t cleanTool) Environment(context.Context, string) ([]byte, error) {
	return nil, nil
}

func (t cleanTool) ListTests(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (t cleanTool) StartTests(context.Context, string, string) (gotool.TestProcess, error) {
	return nil, fmt.Errorf("unexpected StartTests")
}

type fakeModuleCache struct {
	remote     bool
	removed    bool
	purged     bool
	purgeCount int
}

func (c *fakeModuleCache) RemoveModule() error {
	c.removed = true
	return nil
}

func (c *fakeModuleCache) HasRemoteTier() bool {
	return c.remote
}

func (c *fakeModuleCache) PurgeRemoteModule(context.Context) (int, error) {
	c.purged = true
	return c.purgeCount, nil
}

type cleanProcess struct{}

func (p cleanProcess) Events() io.Reader { return strings.NewReader("") }

func (p cleanProcess) Wait() error { return nil }

func (p cleanProcess) Stderr() string { return "" }

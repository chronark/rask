package discover

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chronark/rask/internal/gotool"
)

// TestDiscoverListsWholeModuleOnceForDefaultPattern protects the ./... single-list optimization.
func TestDiscoverListsWholeModuleOnceForDefaultPattern(t *testing.T) {
	module := moduleFixtureRoot(t)
	tool := &fakeTool{all: []gotool.Package{packageFixture(module, "app")}}
	packages, err := Packages(context.Background(), tool, module, module.Dir, []string{"./..."})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if tool.calls["./..."] != 1 {
		t.Fatalf("go list ./... calls = %d, want 1", tool.calls["./..."])
	}
	if len(packages) != 1 || !packages[0].Selected {
		t.Fatalf("packages = %+v, want one selected package", packages)
	}
}

// TestDiscoverMarksSelectedWithinFullClosure protects pattern selection after full-closure listing.
func TestDiscoverMarksSelectedWithinFullClosure(t *testing.T) {
	module := moduleFixtureRoot(t)
	app := packageFixture(module, "app")
	app.Imports = []string{module.Path + "/lib"}
	app.Selected = false
	lib := packageFixture(module, "lib")
	tool := &fakeTool{selected: []gotool.Package{app}, all: []gotool.Package{lib, app}}
	packages, err := Packages(context.Background(), tool, module, module.Dir, []string{"./app"})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	byPath := packagesByPath(packages)
	if byPath[module.Path+"/lib"].Selected {
		t.Fatalf("dependency package must not be selected")
	}
	if !byPath[module.Path+"/app"].Selected {
		t.Fatalf("pattern package must be selected")
	}
}

// TestDiscoverRejectsLoadAndSyntaxErrors protects go-list Error handling and local parse guards.
func TestDiscoverRejectsLoadAndSyntaxErrors(t *testing.T) {
	module := moduleFixtureRoot(t)
	broken := packageFixture(module, "broken")
	broken.Error = &gotool.PackageError{Err: "expected '}', found EOF"}
	_, err := Packages(context.Background(), &fakeTool{all: []gotool.Package{broken}}, module, module.Dir, []string{"./..."})
	if err == nil || !strings.Contains(err.Error(), module.Path+"/broken") {
		t.Fatalf("err = %v, want package load error", err)
	}
	writeFile(t, module.Dir, "bad.go", "package bad\nfunc x() {")
	bad := gotool.Package{Dir: module.Dir, ImportPath: module.Path, GoFiles: []string{"bad.go"}}
	_, err = Packages(context.Background(), &fakeTool{all: []gotool.Package{bad}}, module, module.Dir, []string{"./..."})
	if err == nil || !strings.Contains(err.Error(), "package "+module.Path+" is broken") {
		t.Fatalf("err = %v, want syntax error", err)
	}
}

// TestDiscoverRejectsEmptyAndOutsideSelections protects zero-suite success cases.
func TestDiscoverRejectsEmptyAndOutsideSelections(t *testing.T) {
	module := moduleFixtureRoot(t)
	_, err := Packages(context.Background(), &fakeTool{}, module, module.Dir, []string{"./missing"})
	if err == nil || !strings.Contains(err.Error(), "matched no packages") {
		t.Fatalf("err = %v, want empty pattern error", err)
	}
	outside := gotool.Package{
		Dir: filepath.Join(t.TempDir(), "other"), ImportPath: "example.com/other/pkg", Name: "pkg",
		GoFiles: []string{"pkg.go"}, Selected: true,
	}
	tool := &fakeTool{selected: []gotool.Package{outside}, all: []gotool.Package{packageFixture(module, "app")}}
	_, err = Packages(context.Background(), tool, module, module.Dir, []string{"../other"})
	if err == nil || !strings.Contains(err.Error(), "outside module "+module.Path) {
		t.Fatalf("err = %v, want outside module error", err)
	}
}

// TestDiscoverListsPatternsFromUserDir protects relative pattern resolution.
func TestDiscoverListsPatternsFromUserDir(t *testing.T) {
	module := moduleFixtureRoot(t)
	userDir := filepath.Join(module.Dir, "worker")
	app := packageFixture(module, "worker/api")
	tool := &fakeTool{selected: []gotool.Package{app}, all: []gotool.Package{app}}
	if _, err := Packages(context.Background(), tool, module, userDir, []string{"./api"}); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got := tool.dirs["./api"]; got != userDir {
		t.Fatalf("pattern dir = %q, want %q", got, userDir)
	}
	if tool.calls["./..."] != 0 {
		t.Fatal("narrow selection must not list the whole module")
	}
}

// TestDiscoverMergesLocalReplacePackagesUnselected protects local dependency hashing inputs.
func TestDiscoverMergesLocalReplacePackagesUnselected(t *testing.T) {
	module := moduleFixtureRootWithGoMod(t, "module example.com/m\n\ngo 1.26\n\nreplace example.com/dep => ../dep\n")
	dep := moduleFixtureRootAt(t, filepath.Join(filepath.Dir(module.Dir), "dep"), "example.com/dep")
	app := packageFixture(module, "app")
	utility := packageFixture(dep, "util")
	tool := &fakeTool{allByDir: map[string][]gotool.Package{
		module.Dir: {app},
		dep.Dir:    {utility},
	}}
	packages, err := Packages(context.Background(), tool, module, module.Dir, []string{"./..."})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	byPath := packagesByPath(packages)
	if !byPath[app.ImportPath].Selected {
		t.Fatalf("main package must stay selected")
	}
	if byPath[utility.ImportPath].Selected {
		t.Fatalf("local replace package must not be selected")
	}
}

// TestPackagesShufflesReturnOrder protects inter-suite package randomization.
func TestPackagesShufflesReturnOrder(t *testing.T) {
	module := moduleFixtureRoot(t)
	packages := []gotool.Package{
		packageFixture(module, "a"), packageFixture(module, "b"), packageFixture(module, "c"),
		packageFixture(module, "d"), packageFixture(module, "e"), packageFixture(module, "f"),
	}
	first := discoverOrderForTest(t, module, packages, rand.New(rand.NewPCG(1, 101)))
	second := discoverOrderForTest(t, module, packages, rand.New(rand.NewPCG(2, 102)))
	if strings.Join(first, ",") == strings.Join(second, ",") {
		t.Fatalf("orders matched: %v", first)
	}
	if sameSet(first, second) != nil {
		t.Fatalf("orders contain different packages: %v vs %v", first, second)
	}
}

// TestDiscoverRejectsBrokenAdditionalRoot protects clear local-root failures.
func TestDiscoverRejectsBrokenAdditionalRoot(t *testing.T) {
	module := moduleFixtureRootWithGoMod(t, "module example.com/m\n\ngo 1.26\n\nreplace example.com/dep => ../missing\n")
	app := packageFixture(module, "app")
	_, err := Packages(context.Background(), &fakeTool{all: []gotool.Package{app}}, module, module.Dir, []string{"./..."})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err = %v, want missing root named", err)
	}
}

func TestDiscoverRejectsDependencyListFailure(t *testing.T) {
	module := moduleFixtureRoot(t)
	app := packageFixture(module, "app")
	tool := &fakeTool{
		all:           []gotool.Package{app},
		dependencyErr: fmt.Errorf("dependency broken"),
	}
	_, err := Packages(context.Background(), tool, module, module.Dir, []string{"./..."})
	if err == nil || !strings.Contains(err.Error(), "dependency broken") {
		t.Fatalf("err = %v, want dependency error", err)
	}
}

// TestLocalReplaceRootsParsesSupportedForms protects the minimal go.mod scan.
func TestLocalReplaceRootsParsesSupportedForms(t *testing.T) {
	module := moduleFixtureRootWithGoMod(t, `module example.com/m

go 1.26

replace example.com/single => ../single
replace example.com/version => example.com/version v1.2.3
replace (
	example.com/block => ./block
	example.com/quoted => "../quoted"
)
`)
	roots, err := LocalReplaceRoots(module.Dir)
	if err != nil {
		t.Fatalf("local replacements: %v", err)
	}
	want := []string{
		filepath.Clean(filepath.Join(module.Dir, "../single")),
		filepath.Clean(filepath.Join(module.Dir, "block")),
		filepath.Clean(filepath.Join(module.Dir, "../quoted")),
	}
	if fmt.Sprint(roots) != fmt.Sprint(want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}
}

// moduleFixtureRoot creates a minimal module directory for discovery tests.
func moduleFixtureRoot(t *testing.T) gotool.Module {
	t.Helper()
	return moduleFixtureRootWithGoMod(t, "module example.com/m\n\ngo 1.26\n")
}

// moduleFixtureRootWithGoMod creates a module fixture with custom go.mod content.
func moduleFixtureRootWithGoMod(t *testing.T, goMod string) gotool.Module {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", goMod)
	return gotool.Module{Path: "example.com/m", Dir: dir}
}

// moduleFixtureRootAt creates a minimal module directory at dir.
func moduleFixtureRootAt(t *testing.T, dir string, modulePath string) gotool.Module {
	t.Helper()
	writeFile(t, dir, "go.mod", "module "+modulePath+"\n\ngo 1.26\n")
	return gotool.Module{Path: modulePath, Dir: dir}
}

// packageFixture returns a selected package fixture.
func packageFixture(module gotool.Module, name string) gotool.Package {
	dir := filepath.Join(module.Dir, name)
	return gotool.Package{
		Dir: dir, ImportPath: module.Path + "/" + name, Name: name,
		GoFiles: []string{name + ".go"}, TestGoFiles: []string{name + "_test.go"},
		Selected: true,
	}
}

// writeFile creates a test file and fails the test on filesystem errors.
func writeFile(t *testing.T, dir string, name string, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// packagesByPath indexes package fixtures by import path.
func packagesByPath(packages []gotool.Package) map[string]gotool.Package {
	byPath := make(map[string]gotool.Package, len(packages))
	for _, p := range packages {
		byPath[p.ImportPath] = p
	}
	return byPath
}

// discoverOrderForTest proves shuffle behavior through the seeded package API.
func discoverOrderForTest(
	t *testing.T,
	module gotool.Module,
	packages []gotool.Package,
	rand *rand.Rand,
) []string {
	t.Helper()
	tool := &fakeTool{all: append([]gotool.Package(nil), packages...)}
	discovered, err := packagesWithRand(
		context.Background(),
		tool,
		module,
		module.Dir,
		[]string{"./..."},
		rand,
	)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	order := make([]string, 0, len(discovered))
	for _, p := range discovered {
		order = append(order, p.ImportPath)
	}
	return order
}

// sameSet reports a missing item when two string slices differ as sets.
func sameSet(first []string, second []string) error {
	counts := make(map[string]int, len(first))
	for _, item := range first {
		counts[item]++
	}
	for _, item := range second {
		counts[item]--
	}
	for item, count := range counts {
		if count != 0 {
			return fmt.Errorf("%s count delta %d", item, count)
		}
	}
	return nil
}

// fakeTool records package-listing calls for discovery tests.
type fakeTool struct {
	selected      []gotool.Package
	all           []gotool.Package
	allByDir      map[string][]gotool.Package
	listErrByDir  map[string]error
	dependencyErr error
	calls         map[string]int
	dirs          map[string]string
}

// CurrentModule fails so tests catch accidental module lookups.
func (f *fakeTool) CurrentModule(context.Context) (gotool.Module, error) {
	return gotool.Module{}, fmt.Errorf("unexpected CurrentModule call")
}

// ListPackages preserves the distinction between pattern and closure walks.
func (f *fakeTool) ListPackages(
	_ context.Context,
	dir string,
	patterns []string,
) ([]gotool.Package, error) {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	if f.dirs == nil {
		f.dirs = make(map[string]string)
	}
	key := strings.Join(patterns, " ")
	f.calls[key]++
	f.dirs[key] = dir
	if len(patterns) == 1 && patterns[0] == "./..." {
		if err := f.listErrByDir[dir]; err != nil {
			return nil, err
		}
		if f.allByDir != nil {
			return f.allByDir[dir], nil
		}
		return f.all, nil
	}
	return f.selected, nil
}

func (f *fakeTool) ListDependencies(context.Context, string, []string) ([]gotool.Package, error) {
	if f.dependencyErr != nil {
		return nil, f.dependencyErr
	}
	if f.allByDir != nil {
		var packages []gotool.Package
		for _, values := range f.allByDir {
			packages = append(packages, values...)
		}
		return packages, nil
	}
	return f.all, nil
}

// ListModules fails so workspace coverage must opt in explicitly.
func (f *fakeTool) ListModules(context.Context, string) ([]gotool.Module, error) {
	return nil, fmt.Errorf("unexpected ListModules call")
}

// Environment disables go.work so root tests control every extra module.
func (f *fakeTool) Environment(context.Context, string) ([]byte, error) {
	return []byte("GOARCH='amd64'\nGOOS='linux'\nGOVERSION='go1.26'\nGOWORK=''\n"), nil
}

// ListTests fails because package discovery must not inspect test names.
func (f *fakeTool) ListTests(context.Context, string, string) ([]string, error) {
	return nil, fmt.Errorf("unexpected ListTests call")
}

// StartTests fails because discovery must stay before process execution.
func (f *fakeTool) StartTests(context.Context, string, string) (gotool.TestProcess, error) {
	return nil, fmt.Errorf("unexpected StartTests call")
}

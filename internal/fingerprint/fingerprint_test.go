package fingerprint

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

// TestFingerprintInvariants protects deterministic invalidation and dependency reachability.
func TestFingerprintInvariants(t *testing.T) {
	module, packages := fingerprintFixture(t)
	before := fingerprintsForTest(t, module, packages, nil)
	again := fingerprintsForTest(t, module, packages, nil)
	if fmt.Sprint(before) != fmt.Sprint(again) {
		t.Fatalf("fingerprints changed without input changes")
	}
	writeFile(t, filepath.Join(module.Dir, "dependency"), "dependency.go", "package dependency\n// edit\n")
	afterDependency := fingerprintsForTest(t, module, packages, nil)
	if before[module.Path+"/dependent"] == afterDependency[module.Path+"/dependent"] {
		t.Fatalf("dependency edit must invalidate dependent")
	}
	if before[module.Path+"/bystander"] != afterDependency[module.Path+"/bystander"] {
		t.Fatalf("dependency edit must not invalidate bystander")
	}
	writeFile(t, filepath.Join(module.Dir, "helper"), "helper.go", "package helper\n// edit\n")
	afterTestImport := fingerprintsForTest(t, module, packages, nil)
	if before[module.Path+"/dependent"] == afterTestImport[module.Path+"/dependent"] {
		t.Fatalf("test-only import edit must invalidate suite")
	}
	changedEnv := fingerprintsForTest(t, module, packages, []byte("go1.99\nlinux\namd64\n"))
	if before[module.Path+"/bystander"] == changedEnv[module.Path+"/bystander"] {
		t.Fatalf("toolchain change must invalidate everything")
	}
	writeFile(t, module.Dir, "go.sum", "example.com/dep v1.0.0 h1:abc=\n")
	changedSum := fingerprintsForTest(t, module, packages, nil)
	if before[module.Path+"/bystander"] == changedSum[module.Path+"/bystander"] {
		t.Fatalf("go.sum change must invalidate everything")
	}
	writeFile(t, filepath.Join(module.Dir, "dependent", "testdata", "nested"), "fixture.txt", "before\n")
	withTestdata := fingerprintsForTest(t, module, packages, nil)
	writeFile(t, filepath.Join(module.Dir, "dependent", "testdata", "nested"), "fixture.txt", "after\n")
	changedTestdata := fingerprintsForTest(t, module, packages, nil)
	if withTestdata[module.Path+"/dependent"] == changedTestdata[module.Path+"/dependent"] {
		t.Fatalf("testdata edit must invalidate owning suite")
	}
	if withTestdata[module.Path+"/bystander"] != changedTestdata[module.Path+"/bystander"] {
		t.Fatalf("testdata edit must not invalidate bystander")
	}
}

// TestFingerprintExternalTestCycleTerminates protects the build-import-only topo graph.
func TestFingerprintExternalTestCycleTerminates(t *testing.T) {
	module := moduleFixtureRoot(t)
	left := packageFixture(module, "left")
	left.TestGoFiles = nil
	left.XTestGoFiles = []string{"left_x_test.go"}
	left.XTestImports = []string{module.Path + "/right"}
	right := packageFixture(module, "right")
	right.TestGoFiles = nil
	right.Imports = []string{module.Path + "/left"}
	writeFile(t, left.Dir, "left.go", "package left\n")
	writeFile(t, left.Dir, "left_x_test.go", "package left_test\n")
	writeFile(t, right.Dir, "right.go", "package right\n")
	result := fingerprintsForTest(t, module, []gotool.Package{right, left}, nil)
	if result[left.ImportPath] == "" {
		t.Fatalf("left suite did not fingerprint")
	}
}

// TestFingerprintLocalReplaceInvalidatesDependentOnly protects replaced-module hashing.
func TestFingerprintLocalReplaceInvalidatesDependentOnly(t *testing.T) {
	module, dep, packages := localDependencyFixture(t, "replace example.com/dep => ../dep\n")
	before := fingerprintsForTest(t, module, packages, nil)
	writeFile(t, filepath.Join(dep.Dir, "util"), "util.go", "package util\n// edit\n")
	afterSource := fingerprintsForTest(t, module, packages, nil)
	if before[module.Path+"/dependent"] == afterSource[module.Path+"/dependent"] {
		t.Fatalf("local dependency edit must invalidate dependent")
	}
	if before[module.Path+"/bystander"] != afterSource[module.Path+"/bystander"] {
		t.Fatalf("local dependency edit must not invalidate bystander")
	}
	if afterSource[dep.Path+"/util"] != "" {
		t.Fatalf("local dependency package must not become a selected suite")
	}
	writeFile(t, dep.Dir, "go.mod", "module example.com/dep\n\ngo 1.27\n")
	afterMod := fingerprintsForTest(t, module, packages, nil)
	if before[module.Path+"/bystander"] == afterMod[module.Path+"/bystander"] {
		t.Fatalf("local dependency go.mod edit must salt every suite")
	}
}

// TestFingerprintGoWorkHashesWorkspaceModulesAndFile protects workspace inputs.
func TestFingerprintGoWorkHashesWorkspaceModulesAndFile(t *testing.T) {
	module, dep, packages := localDependencyFixture(t, "")
	workPath := filepath.Join(filepath.Dir(module.Dir), "go.work")
	writeFile(t, filepath.Dir(workPath), "go.work", "go 1.26\n\nuse (\n\t./m\n\t./dep\n)\n")
	env := []byte("GOARCH='amd64'\nGOOS='linux'\nGOVERSION='go1.26'\nGOWORK='" + workPath + "'\n")
	tool := &fakeTool{environment: env, modules: []gotool.Module{module, dep}}
	before := fingerprintsWithToolForTest(t, tool, module, packages)
	writeFile(t, filepath.Join(dep.Dir, "util"), "util.go", "package util\n// work edit\n")
	afterSource := fingerprintsWithToolForTest(t, tool, module, packages)
	if before[module.Path+"/dependent"] == afterSource[module.Path+"/dependent"] {
		t.Fatalf("workspace dependency edit must invalidate dependent")
	}
	if before[module.Path+"/bystander"] != afterSource[module.Path+"/bystander"] {
		t.Fatalf("workspace dependency edit must not invalidate bystander")
	}
	writeFile(t, filepath.Dir(workPath), "go.work", "go 1.26\n\nuse ./m\nuse ./dep\n")
	afterWork := fingerprintsWithToolForTest(t, tool, module, packages)
	if before[module.Path+"/bystander"] == afterWork[module.Path+"/bystander"] {
		t.Fatalf("go.work edit must salt every suite")
	}
}

// TestFingerprintStableAcrossGOGCCFLAGSOnlyChanges protects same-tree cache hits.
func TestFingerprintStableAcrossGOGCCFLAGSOnlyChanges(t *testing.T) {
	module, packages := fingerprintFixture(t)
	firstEnv := []byte("GOARCH='arm64'\nGOFLAGS='-race'\n" +
		"GOGCCFLAGS='-ffile-prefix-map=/tmp/go-build111=/tmp/go-build'\n" +
		"GOOS='darwin'\nGOVERSION='go1.26'\nGOWORK=''\n")
	secondEnv := []byte("GOARCH='arm64'\nGOFLAGS='-race'\n" +
		"GOGCCFLAGS='-ffile-prefix-map=/tmp/go-build222=/tmp/go-build'\n" +
		"GOOS='darwin'\nGOVERSION='go1.26'\nGOWORK=''\n")
	before := fingerprintsForTest(t, module, packages, firstEnv)
	again := fingerprintsForTest(t, module, packages, secondEnv)
	if fmt.Sprint(before) != fmt.Sprint(again) {
		t.Fatalf("fingerprints changed when only GOGCCFLAGS changed")
	}
}

// TestFingerprintWithRandPermutesReadySuites protects inter-suite order draws.
func TestFingerprintWithRandPermutesReadySuites(t *testing.T) {
	module, packages := fingerprintFixture(t)
	first := fingerprintOrderForTest(t, module, packages, rand.New(rand.NewPCG(1, 101)))
	second := fingerprintOrderForTest(t, module, packages, rand.New(rand.NewPCG(2, 102)))
	if fmt.Sprint(first) == fmt.Sprint(second) {
		t.Fatalf("orders matched: %v", first)
	}
}

// TestFingerprintReadyQueueShufflePreservesDependencies protects topological emission.
func TestFingerprintReadyQueueShufflePreservesDependencies(t *testing.T) {
	module, packages := fingerprintFixture(t)
	for seed := uint64(1); seed <= 100; seed++ {
		order := fingerprintOrderForTest(t, module, packages, rand.New(rand.NewPCG(seed, seed+100)))
		if err := dependencyOrderError(packages, order); err != nil {
			t.Fatalf("seed %d order %v: %v", seed, order, err)
		}
	}
}

// TestSuitesRejectsUnresolvedBuildGraph protects stream-end graph validation.
func TestSuitesRejectsUnresolvedBuildGraph(t *testing.T) {
	module := moduleFixtureRoot(t)
	left := packageFixture(module, "left")
	right := packageFixture(module, "right")
	left.Imports = []string{right.ImportPath}
	right.Imports = []string{left.ImportPath}
	writeFile(t, left.Dir, "left.go", "package left\n")
	writeFile(t, left.Dir, "left_test.go", "package left\n")
	writeFile(t, right.Dir, "right.go", "package right\n")
	writeFile(t, right.Dir, "right_test.go", "package right\n")
	_, err := Suites(context.Background(), &fakeTool{}, module, []gotool.Package{left, right})
	if err == nil || err.Error() != "unresolved discovered imports prevented fingerprinting" {
		t.Fatalf("err = %v, want unresolved graph error", err)
	}
}

// TestSuitesReturnsHashingErrors protects direct propagation from file hashing.
func TestSuitesReturnsHashingErrors(t *testing.T) {
	module := moduleFixtureRoot(t)
	broken := packageFixture(module, "broken")
	_, err := Suites(context.Background(), &fakeTool{}, module, []gotool.Package{broken})
	if err == nil || !strings.Contains(err.Error(), "hashing") {
		t.Fatalf("err = %v, want hashing error", err)
	}
}

// moduleFixtureRoot creates a minimal module directory for fingerprint tests.
func moduleFixtureRoot(t *testing.T) gotool.Module {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/m\n\ngo 1.26\n")
	return gotool.Module{Path: "example.com/m", Dir: dir}
}

// fingerprintFixture creates packages that exercise build and test-only imports.
func fingerprintFixture(t *testing.T) (gotool.Module, []gotool.Package) {
	module := moduleFixtureRoot(t)
	dep := packageFixture(module, "dependency")
	helper := packageFixture(module, "helper")
	helper.TestGoFiles = nil
	dependent := packageFixture(module, "dependent")
	dependent.Imports = []string{dep.ImportPath}
	dependent.TestImports = []string{helper.ImportPath}
	bystander := packageFixture(module, "bystander")
	for _, p := range []gotool.Package{dep, helper, dependent, bystander} {
		writeFile(t, p.Dir, p.GoFiles[0], "package "+p.Name+"\n")
		if len(p.TestGoFiles) > 0 {
			writeFile(t, p.Dir, p.TestGoFiles[0], "package "+p.Name+"\n")
		}
	}
	return module, []gotool.Package{dep, helper, dependent, bystander}
}

// localDependencyFixture creates a main module with an unselected local dependency.
func localDependencyFixture(
	t *testing.T,
	extraGoMod string,
) (gotool.Module, gotool.Module, []gotool.Package) {
	t.Helper()
	parent := t.TempDir()
	module := moduleFixtureRootAt(t, filepath.Join(parent, "m"), "example.com/m",
		"module example.com/m\n\ngo 1.26\n\n"+extraGoMod)
	dep := moduleFixtureRootAt(t, filepath.Join(parent, "dep"), "example.com/dep",
		"module example.com/dep\n\ngo 1.26\n")
	util := packageFixture(dep, "util")
	util.Selected = false
	util.TestGoFiles = nil
	dependent := packageFixture(module, "dependent")
	dependent.Imports = []string{util.ImportPath}
	bystander := packageFixture(module, "bystander")
	for _, p := range []gotool.Package{util, dependent, bystander} {
		writeFile(t, p.Dir, p.GoFiles[0], "package "+p.Name+"\n")
		if len(p.TestGoFiles) > 0 {
			writeFile(t, p.Dir, p.TestGoFiles[0], "package "+p.Name+"\n")
		}
	}
	return module, dep, []gotool.Package{util, dependent, bystander}
}

// moduleFixtureRootAt creates a minimal module directory at dir.
func moduleFixtureRootAt(t *testing.T, dir string, modulePath string, goMod string) gotool.Module {
	t.Helper()
	writeFile(t, dir, "go.mod", goMod)
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

// fingerprintsForTest returns suite digests keyed by import path.
func fingerprintsForTest(
	t *testing.T,
	module gotool.Module,
	packages []gotool.Package,
	env []byte,
) map[string]string {
	t.Helper()
	return fingerprintsWithToolForTest(t, &fakeTool{environment: env}, module, packages)
}

// fingerprintsWithToolForTest returns suite digests using a configured fake tool.
func fingerprintsWithToolForTest(
	t *testing.T,
	tool *fakeTool,
	module gotool.Module,
	packages []gotool.Package,
) map[string]string {
	t.Helper()
	suites, err := fingerprintWithRand(
		context.Background(),
		tool,
		module,
		packages,
		rand.New(rand.NewPCG(1, 2)),
	)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	result := make(map[string]string, len(suites))
	for _, suite := range suites {
		result[suite.Package.ImportPath] = suite.Digest
	}
	return result
}

// fingerprintOrderForTest returns suite import paths for a seeded fingerprint run.
func fingerprintOrderForTest(
	t *testing.T,
	module gotool.Module,
	packages []gotool.Package,
	rand *rand.Rand,
) []string {
	t.Helper()
	suites, err := fingerprintWithRand(context.Background(), &fakeTool{}, module, packages, rand)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	order := make([]string, 0, len(suites))
	for _, suite := range suites {
		order = append(order, suite.Package.ImportPath)
	}
	return order
}

// dependencyOrderError reports the first dependency ordering violation.
func dependencyOrderError(packages []gotool.Package, order []string) error {
	position := make(map[string]int, len(order))
	for index, importPath := range order {
		position[importPath] = index
	}
	for _, p := range packages {
		for _, dep := range p.Imports {
			if position[p.ImportPath] <= position[dep] {
				return fmt.Errorf("%s preceded dependency %s", p.ImportPath, dep)
			}
		}
	}
	return nil
}

// fakeTool provides stable Go environment bytes for fingerprint tests.
type fakeTool struct {
	environment []byte
	modules     []gotool.Module
}

// CurrentModule fails because fingerprint tests pass modules directly.
func (f *fakeTool) CurrentModule(context.Context) (gotool.Module, error) {
	return gotool.Module{}, fmt.Errorf("unexpected CurrentModule call")
}

// ListPackages fails because fingerprint tests pass packages directly.
func (f *fakeTool) ListPackages(context.Context, string, []string) ([]gotool.Package, error) {
	return nil, fmt.Errorf("unexpected ListPackages call")
}

func (f *fakeTool) ListDependencies(context.Context, string, []string) ([]gotool.Package, error) {
	return nil, fmt.Errorf("unexpected ListDependencies call")
}

// ListModules returns configured workspace modules for fingerprint salt tests.
func (f *fakeTool) ListModules(context.Context, string) ([]gotool.Module, error) {
	return f.modules, nil
}

// Environment returns configured or default Go environment bytes.
func (f *fakeTool) Environment(context.Context, string) ([]byte, error) {
	if f.environment != nil {
		return f.environment, nil
	}
	return []byte("GOARCH='amd64'\nGOOS='linux'\nGOVERSION='go1.26'\nGOWORK=''\n"), nil
}

// ListTests fails because fingerprinting never enumerates tests.
func (f *fakeTool) ListTests(context.Context, string, string) ([]string, error) {
	return nil, fmt.Errorf("unexpected ListTests call")
}

// StartTests fails because fingerprinting never starts test processes.
func (f *fakeTool) StartTests(context.Context, string, string) (gotool.TestProcess, error) {
	return nil, fmt.Errorf("unexpected StartTests call")
}

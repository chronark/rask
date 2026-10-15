// Package gotool defines the boundary around the Go command.
//
// The package is the only code in rask that spawns the `go` binary. It owns the
// system implementation of [Tool], the streaming [TestProcess] handle returned
// by `go test -json`, and the data shapes emitted by `go list`.
//
// [SystemTool] starts one command per method call and does not retain shared
// mutable state. Returned [TestProcess] values are single-use handles; callers
// must read [TestProcess.Events] before [TestProcess.Wait] so the child process
// cannot block on a full stdout pipe.
package gotool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chronark/rask/internal/assert"
)

var listTestsRegexp = regexp.MustCompile(ListTestsPattern)

const (
	// listFields is the bounded `go list` vocabulary consumed by rask.
	listFields = "Dir,ImportPath,Name,Error,Incomplete,GoFiles,CgoFiles,SFiles,EmbedFiles," +
		"TestGoFiles,XTestGoFiles,TestEmbedFiles,XTestEmbedFiles,Imports,TestImports,XTestImports"

	// ListTestsPattern is shared by `go test -list` and live progress counting.
	// The runner separately rejects subtests so its x counter matches this
	// top-level denominator even when `go test -json` emits subtest terminals.
	ListTestsPattern = "^Test"

	// packageCountMax bounds malformed or unexpectedly large JSON streams from `go list`.
	packageCountMax = 16384

	// testProcessWaitDelay bounds Wait after cancellation because orphaned pipe
	// writers can otherwise keep the command wait path blocked indefinitely.
	testProcessWaitDelay = 5 * time.Second
)

// Module describes one module object returned by `go list -m -json`.
type Module struct {
	// Path is the module import path.
	Path string

	// Dir is the absolute module directory reported by the Go command.
	Dir string
}

// PackageError describes the Error field emitted by `go list -json`.
type PackageError struct {
	// Err is the human-readable load error reported by `go list`.
	Err string
}

// Package describes the subset of `go list -json` package metadata used by rask.
//
// Selected is not emitted by `go list`; discovery sets it to remember which
// packages matched the user's patterns after the discovered closure is loaded.
type Package struct {
	// Dir is the package directory.
	Dir string

	// ImportPath is the package import path.
	ImportPath string

	// Name is the declared package name.
	Name string

	// Error is the load error reported by `go list`, when one exists.
	Error *PackageError

	// Incomplete reports whether `go list` could not load the package fully.
	Incomplete bool

	// GoFiles are non-test Go files.
	GoFiles []string

	// CgoFiles are Cgo files that participate in the package build.
	CgoFiles []string

	// SFiles are assembly files that participate in the package build.
	SFiles []string

	// EmbedFiles are files embedded by non-test Go files.
	EmbedFiles []string

	// TestGoFiles are internal test files.
	TestGoFiles []string

	// XTestGoFiles are external test package files.
	XTestGoFiles []string

	// TestEmbedFiles are files embedded by internal test files.
	TestEmbedFiles []string

	// XTestEmbedFiles are files embedded by external test files.
	XTestEmbedFiles []string

	// Imports are build imports for the package.
	Imports []string

	// TestImports are imports used by internal test files.
	TestImports []string

	// XTestImports are imports used by external test package files.
	XTestImports []string

	// ModuleDir is not emitted by `go list`; discovery sets it to the local
	// module root that produced this package.
	ModuleDir string

	// Selected reports whether this package matched the user's requested patterns.
	Selected bool
}

// HasTests reports whether the package has internal or external test files.
func (p Package) HasTests() bool {
	return len(p.TestGoFiles) > 0 || len(p.XTestGoFiles) > 0
}

// MatchesListTestsPattern reports whether name matches the lister denominator.
func MatchesListTestsPattern(name string) bool {
	return listTestsRegexp.MatchString(name)
}

// Tool is the seam between rask and the Go command.
//
// Implementations must be safe to call from the runner's worker pool because
// stale suites execute concurrently. [StartTests] must return a process whose
// stdout can be drained through [TestProcess.Events].
type Tool interface {
	// CurrentModule accepts ctx so discovery startup can abort a blocked Go command.
	CurrentModule(ctx context.Context) (Module, error)

	// ListPackages accepts ctx because package discovery is often cancelled by Ctrl+C.
	ListPackages(ctx context.Context, dir string, patterns []string) ([]Package, error)

	// ListDependencies includes transitive build imports of the supplied packages.
	ListDependencies(ctx context.Context, dir string, importPaths []string) ([]Package, error)

	// ListModules accepts ctx so workspace discovery does not outlive the run.
	ListModules(ctx context.Context, dir string) ([]Module, error)

	// Environment accepts ctx so cache salting cannot keep rask alive after cancellation.
	Environment(ctx context.Context, moduleDir string) ([]byte, error)

	// ListTests returns test names from `go test -list '^Test'` for importPath.
	ListTests(ctx context.Context, moduleDir string, importPath string) ([]string, error)

	// StartTests starts one streaming `go test -json` process for importPath.
	StartTests(ctx context.Context, moduleDir string, importPath string) (TestProcess, error)
}

// TestProcess is a streaming handle for one `go test -json` process.
//
// Callers should drain [TestProcess.Events] before calling [TestProcess.Wait].
// [Stderr] returns buffered stderr after the process has exited.
type TestProcess interface {
	// Events returns stdout containing newline-delimited `go test -json` events.
	Events() io.Reader

	// Wait waits for the process after callers have drained [TestProcess.Events].
	Wait() error

	// Stderr returns buffered stderr output from the test process.
	Stderr() string
}

// SystemTool runs the real Go command in the local environment.
type SystemTool struct{}

// CurrentModule resolves the module containing the current working directory.
//
// Workspaces may cause `go list -m -json` to emit more than one module. The
// deepest module directory containing the current directory wins.
func (SystemTool) CurrentModule(ctx context.Context) (Module, error) {
	output, _, err := runGo(ctx, "", "list", "-m", "-json")
	if err != nil {
		if ctx.Err() != nil {
			return Module{}, ctx.Err()
		}
		return Module{}, errors.New("not inside a Go module (go list -m failed)")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return Module{}, fmt.Errorf("resolving working directory: %w", err)
	}
	return decodeCurrentModule(output, cwd)
}

// ListPackages returns package metadata for the provided patterns from dir.
//
// The method rejects non-zero `go list` exits and parses successful JSON
// streams. Syntax and load errors carried in the JSON Error field are handled
// by discovery because `go list` may still exit zero for those packages.
func (SystemTool) ListPackages(ctx context.Context, dir string, patterns []string) ([]Package, error) {
	return listPackages(ctx, dir, patterns, false)
}

// ListDependencies resolves build dependencies without loading their tests.
func (SystemTool) ListDependencies(ctx context.Context, dir string, importPaths []string) ([]Package, error) {
	return listPackages(ctx, dir, importPaths, true)
}

func listPackages(ctx context.Context, dir string, patterns []string, dependencies bool) ([]Package, error) {
	assert.NotZero(dir)
	assert.NotZero(len(patterns))
	arguments := []string{"list", "-json=" + listFields}
	if dependencies {
		arguments = append(arguments, "-deps")
	}
	arguments = append(arguments, patterns...)
	output, stderr, err := runGo(ctx, dir, arguments...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("go list failed: %s", stderr)
	}
	return decodePackages(output)
}

// ListModules returns module metadata visible from dir.
func (SystemTool) ListModules(ctx context.Context, dir string) ([]Module, error) {
	assert.NotZero(dir)
	output, stderr, err := runGo(ctx, dir, "list", "-m", "-json")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("go list -m failed: %s", stderr)
	}
	return decodeModules(output)
}

// Environment returns full Go environment output for discovery and cache salting.
func (SystemTool) Environment(ctx context.Context, moduleDir string) ([]byte, error) {
	output, _, err := runGo(ctx, moduleDir, "env")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("go env failed: %w", err)
	}
	if len(output) == 0 {
		return nil, errors.New("go env returned no output")
	}
	return output, nil
}

// ListTests enumerates top-level tests before the suite stream starts.
//
// The command may execute package TestMain, so it uses the same process-group
// cancellation and WaitDelay policy as streaming test runs. Callers own the
// context deadline because listing progress is cosmetic for rask.
func (SystemTool) ListTests(ctx context.Context, moduleDir string, importPath string) ([]string, error) {
	assert.NotNil(ctx)
	output, stderr, err := runGoListTests(ctx, moduleDir, importPath)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if stderr == "" {
			stderr = err.Error()
		}
		return nil, fmt.Errorf("go test -list failed: %s", stderr)
	}
	return parseListedTests(output), nil
}

// runGoListTests executes the test lister with test-process cancellation rules.
func runGoListTests(ctx context.Context, moduleDir string, importPath string) ([]byte, string, error) {
	command := exec.CommandContext(ctx, "go", goListTestArguments(importPath)...)
	command.Dir = moduleDir
	command.WaitDelay = testProcessWaitDelay
	configureTestCommandCancel(command)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), strings.TrimSpace(stderr.String()), err
}

// goListTestArguments returns the fixed test-list invocation for progress totals.
func goListTestArguments(importPath string) []string {
	return []string{"test", "-list", ListTestsPattern, importPath}
}

// parseListedTests filters the package summary printed after listed test names.
func parseListedTests(output []byte) []string {
	lines := strings.Split(string(output), "\n")
	tests := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || isGoTestSummaryLine(line) {
			continue
		}
		tests = append(tests, line)
	}
	return tests
}

// isGoTestSummaryLine reports package status lines appended by `go test`.
func isGoTestSummaryLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	return fields[0] == "ok" || fields[0] == "FAIL" || fields[0] == "?"
}

// runGo executes one non-streaming `go` subcommand and returns stdout plus
// trimmed stderr.
//
// Stderr comes back separately because each caller folds the Go command's
// diagnostic into its own domain-specific error message. An empty dir runs in
// the current working directory.
func runGo(ctx context.Context, dir string, arguments ...string) ([]byte, string, error) {
	assert.NotNil(ctx)
	assert.NotZero(len(arguments))
	command := exec.CommandContext(ctx, "go", arguments...)
	command.Dir = dir
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	return output, strings.TrimSpace(stderr.String()), err
}

// SaltEnvironment returns go-env bytes suitable for fingerprint salting.
//
// GOGCCFLAGS is excluded because the Go tool derives it from stable inputs
// already present in go env, and it embeds per-invocation temporary directories.
func SaltEnvironment(environment []byte) []byte {
	lines := bytes.SplitAfter(environment, []byte("\n"))
	filtered := make([]byte, 0, len(environment))
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("GOGCCFLAGS=")) {
			continue
		}
		filtered = append(filtered, line...)
	}
	return filtered
}

// StartTests starts `go test -json -count=1 -failfast -shuffle=on` for one package.
//
// `-count=1` disables the Go test cache because rask owns result caching. The
// `-shuffle=on` flag makes intra-suite order random on every stale execution. The
// process is bound to ctx through [exec.CommandContext], and gotool installs
// the cancellation policy so fail-fast and watchdog kills reap the whole
// process tree started by `go test`.
func (SystemTool) StartTests(ctx context.Context, moduleDir string, importPath string) (TestProcess, error) {
	return startTestProcess(ctx, moduleDir, "go", goTestArguments(importPath)...)
}

// goTestArguments returns the fixed test invocation rask uses for stale suites.
func goTestArguments(importPath string) []string {
	return []string{"test", "-json", "-count=1", "-failfast", "-shuffle=on", importPath}
}

// startTestProcess starts one streaming test command with rask cancellation.
//
// The helper exists so tests can exercise process-group cancellation with a
// tiny shell program instead of spawning the Go tool. All callers still use the
// same command setup, so every suite kill path keeps the same process cleanup
// behavior.
func startTestProcess(ctx context.Context, moduleDir string, name string, args ...string) (TestProcess, error) {
	assert.NotNil(ctx)
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = moduleDir
	command.WaitDelay = testProcessWaitDelay
	configureTestCommandCancel(command)
	process := &systemTestProcess{command: command}
	command.Stderr = &process.stderr
	events, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("piping test command: %w", err)
	}
	process.events = events
	if err := command.Start(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("starting test command: %w", err)
	}
	return process, nil
}

// systemTestProcess adapts [exec.Cmd] to [TestProcess].
type systemTestProcess struct {
	command *exec.Cmd
	events  io.Reader
	stderr  bytes.Buffer
}

// Events returns the stdout stream carrying `go test -json` events.
func (p *systemTestProcess) Events() io.Reader {
	assert.NotNil(p.events)
	return p.events
}

// Wait waits for the underlying Go command to exit.
func (p *systemTestProcess) Wait() error {
	return p.command.Wait()
}

// Stderr returns the stderr captured from the underlying Go command.
func (p *systemTestProcess) Stderr() string {
	return p.stderr.String()
}

// decodeCurrentModule selects the module containing cwd from a module JSON stream.
func decodeCurrentModule(output []byte, cwd string) (Module, error) {
	modules, err := decodeModules(output)
	if err != nil {
		return Module{}, err
	}
	module, found := selectModuleForDir(modules, cwd)
	if !found {
		return Module{}, fmt.Errorf("go list -m did not report a module containing %s", cwd)
	}
	if module.Path == "" || module.Dir == "" {
		return Module{}, errors.New("go list -m reported incomplete module info")
	}
	return module, nil
}

// decodeModules decodes the newline-delimited module objects emitted by `go list -m -json`.
func decodeModules(output []byte) ([]Module, error) {
	modules := make([]Module, 0, 1)
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoded := 0; ; decoded++ {
		if decoded >= packageCountMax {
			return nil, fmt.Errorf("workspace exceeds %d modules", packageCountMax)
		}
		var module Module
		err := decoder.Decode(&module)
		if err == io.EOF {
			return modules, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parsing module info: %w", err)
		}
		modules = append(modules, module)
	}
}

// decodePackages decodes the newline-delimited package objects emitted by `go list -json`.
func decodePackages(output []byte) ([]Package, error) {
	packages := make([]Package, 0, 256)
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoded := 0; ; decoded++ {
		if decoded >= packageCountMax {
			return nil, fmt.Errorf("module exceeds %d packages", packageCountMax)
		}
		var p Package
		err := decoder.Decode(&p)
		if err == io.EOF {
			return packages, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parsing go list output: %w", err)
		}
		if p.ImportPath == "" || p.Dir == "" {
			return nil, fmt.Errorf("go list emitted a package without identity: %+v", p)
		}
		packages = append(packages, p)
	}
}

// selectModuleForDir returns the deepest module directory containing cwd.
func selectModuleForDir(modules []Module, cwd string) (Module, bool) {
	workingDir, err := filepath.Abs(cwd)
	assert.NoError(err)
	var selected Module
	for _, module := range modules {
		dir, err := filepath.Abs(module.Dir)
		assert.NoError(err)
		if containsDir(dir, workingDir) && len(dir) > len(selected.Dir) {
			selected = module
		}
	}
	return selected, selected.Dir != ""
}

// containsDir reports whether child is equal to parent or is inside it.
func containsDir(parent string, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

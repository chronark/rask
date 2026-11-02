package e2e

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestJUnitReportsExecutedTestDurations(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "timed", test: testPass})
	writeFile(t, module.dir, "timed/timed_test.go", `package timed
import (
	"testing"
	"time"
)
func TestShort(t *testing.T) { time.Sleep(100 * time.Millisecond) }
func TestLong(t *testing.T) { time.Sleep(300 * time.Millisecond) }
`)
	path := filepath.Join(module.dir, "reports", "junit.xml")
	result := runRask(t, module.dir, cacheEnv(t.TempDir()), "--x-junit-file", path, "./...")
	assertCode(t, result, 0)
	assertSuites(t, result.stdout, "Passed", "timed")
	report := readJUnit(t, path)
	if report.Tests != 2 || len(report.Suites) != 1 {
		t.Fatalf("report=%+v, want one suite with two tests", report)
	}
	suite := report.Suites[0]
	if suite.Name != module.path+"/timed" || suite.Tests != 2 || len(suite.Cases) != 2 {
		t.Fatalf("suite=%+v, want timed package with two tests", suite)
	}
	durations := map[string]float64{}
	for _, test := range suite.Cases {
		if test.Classname != module.path+"/timed" || test.Time == nil {
			t.Fatalf("test=%+v, want package classname and duration", test)
		}
		durations[test.Name] = *test.Time
	}
	if durations["TestShort"] < 0.1 || durations["TestLong"] < 0.3 || durations["TestShort"] >= durations["TestLong"] {
		t.Fatalf("durations=%v, want separate short and long test durations", durations)
	}
}

func TestJUnitReportsFailuresAndSkips(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "outcomes", test: testPass})
	writeFile(t, module.dir, "outcomes/outcomes_test.go", `package outcomes
import "testing"
func TestOutcomes(t *testing.T) {
	t.Run("passed", func(t *testing.T) {})
	t.Run("skipped", func(t *testing.T) { t.Skip("not available") })
	t.Run("failed", func(t *testing.T) { t.Fatal("expected <value> & got another") })
}
`)
	path := "reports with spaces/junit.xml"
	result := runRask(t, module.dir, cacheEnv(t.TempDir()), "test", "--x-junit-file="+path, "./...")
	assertCode(t, result, 1)
	report := readJUnit(t, filepath.Join(module.dir, path))
	if report.Tests != 4 || report.Failures != 2 || report.Skipped != 1 || len(report.Suites) != 1 {
		t.Fatalf("report=%+v, want four results with two failures and one skip", report)
	}
	suite := report.Suites[0]
	if suite.Failures != 2 || suite.Skipped != 1 {
		t.Fatalf("suite=%+v, want two failures and one skip", suite)
	}
	for _, test := range suite.Cases {
		switch test.Name {
		case "TestOutcomes/failed":
			if test.Failure == nil {
				t.Fatal("missing failure details")
			}
			assertContains(t, test.Failure.Text, "expected <value> & got another")
		case "TestOutcomes/skipped":
			if test.Skipped == nil {
				t.Fatal("missing skip details")
			}
			assertContains(t, test.Skipped.Text, "not available")
		}
	}
}

func TestJUnitReportsInterruptedTests(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "interrupted", test: testPass})
	marker := filepath.Join(t.TempDir(), "started")
	writeFile(t, module.dir, "interrupted/interrupted_test.go", fmt.Sprintf(`package interrupted
import (
	"os"
	"testing"
	"time"
)
func TestInterruption(t *testing.T) {
	t.Run("completed", func(t *testing.T) {})
	t.Run("running", func(t *testing.T) {
		if err := os.WriteFile(%q, nil, 0600); err != nil { t.Fatal(err) }
		time.Sleep(time.Minute)
	})
}
`, marker))
	path := filepath.Join(module.dir, "junit.xml")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, raskBinary, "--x-junit-file", path, "./...")
	command.Dir = module.dir
	command.Env = baseEnv(cacheEnv(t.TempDir()))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			_ = command.Wait()
			t.Fatalf("test did not start: %s\n%s", stdout.String(), stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); exitCodeForError(err) != 130 {
		t.Fatalf("exit=%v, want 130\n%s\n%s", err, stdout.String(), stderr.String())
	}
	report := readJUnit(t, path)
	if report.Tests != 3 || report.Errors != 2 || len(report.Suites) != 1 {
		t.Fatalf("report=%+v, want completed test and two interrupted results", report)
	}
	for _, test := range report.Suites[0].Cases {
		if test.Name == "TestInterruption/completed" {
			if test.Error != nil || test.Failure != nil || test.Time == nil {
				t.Fatalf("completed test=%+v", test)
			}
		} else if test.Error == nil || test.Time != nil {
			t.Fatalf("interrupted test=%+v, want error without invented duration", test)
		}
	}
}

func TestJUnitReportsPackageFailures(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "broken", test: testPass})
	writeFile(t, module.dir, "broken/main_test.go", `package broken
import (
	"fmt"
	"os"
	"testing"
)
func TestMain(m *testing.M) {
	m.Run()
	fmt.Println("TestMain teardown failed")
	os.Exit(1)
}
`)
	path := filepath.Join(module.dir, "junit.xml")
	result := runRask(t, module.dir, cacheEnv(t.TempDir()), "--x-junit-file", path, "./...")
	assertCode(t, result, 1)
	report := readJUnit(t, path)
	if report.Tests != 2 || report.Errors != 1 || len(report.Suites) != 1 {
		t.Fatalf("report=%+v, want passing test and package error", report)
	}
	for _, test := range report.Suites[0].Cases {
		if test.Name == "[package]" {
			if test.Error == nil {
				t.Fatal("missing package failure details")
			}
			assertContains(t, test.Error.Text, "TestMain teardown failed")
		}
	}
}

func TestJUnitExcludesCachedSuites(t *testing.T) {
	module := scaffoldModule(t, demoPackages(false)...)
	cache := cacheEnv(t.TempDir())
	path := filepath.Join(module.dir, "junit.xml")
	assertCode(t, runRask(t, module.dir, cache, "--x-junit-file", path), 0)
	if report := readJUnit(t, path); report.Tests != 3 {
		t.Fatalf("cold report=%+v, want three tests", report)
	}
	warm := runRask(t, module.dir, cache, "--x-junit-file", path)
	assertCode(t, warm, 0)
	assertSuites(t, warm.stdout, "Cached", "bystander", "dep", "dependent")
	if report := readJUnit(t, path); report.Tests != 0 || len(report.Suites) != 0 {
		t.Fatalf("warm report=%+v, want empty report replacing previous results", report)
	}
	editDepSource(t, module)
	assertCode(t, runRask(t, module.dir, cache, "--x-junit-file", path), 0)
	report := readJUnit(t, path)
	if report.Tests != 2 || len(report.Suites) != 2 {
		t.Fatalf("mixed report=%+v, want two executed suites", report)
	}
	for _, suite := range report.Suites {
		if suite.Name == module.path+"/bystander" {
			t.Fatal("cached suite must not be reported as a fresh execution")
		}
	}
}

func TestJUnitRequiresOutputPath(t *testing.T) {
	module := scaffoldModule(t, packageSpec{name: "passing", test: testPass})
	for _, args := range [][]string{
		{"--x-junit-file"},
		{"--x-junit-file="},
		{"--x-junit-file", "--no-cache"},
	} {
		result := runRask(t, module.dir, cacheEnv(t.TempDir()), args...)
		assertCode(t, result, 1)
		assertContains(t, result.stderr, "missing value for flag: x-junit-file")
	}
}

func TestJUnitWriteErrorFailsCommand(t *testing.T) {
	for _, kind := range []testKind{testPass, testFail} {
		t.Run(string(kind), func(t *testing.T) {
			module := scaffoldModule(t, packageSpec{name: "example", test: kind})
			result := runRask(t, module.dir, cacheEnv(t.TempDir()), "--x-junit-file", module.dir)
			assertCode(t, result, 1)
			assertContains(t, result.stderr, "writing JUnit report")
			if kind == testFail {
				assertContains(t, result.stdout+result.stderr, "intentional failure in example")
			}
		})
	}
}

type junitReport struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      *float64      `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure"`
	Error     *junitMessage `xml:"error"`
	Skipped   *junitMessage `xml:"skipped"`
}

type junitMessage struct {
	Text string `xml:",chardata"`
}

func readJUnit(t *testing.T, path string) junitReport {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read JUnit report: %v", err)
	}
	var report junitReport
	if err := xml.Unmarshal(body, &report); err != nil {
		t.Fatalf("decode JUnit report: %v\n%s", err, body)
	}
	return report
}

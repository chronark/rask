package testcmd

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"

	"github.com/chronark/rask/internal/run"
)

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
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     *float64    `xml:"time,attr,omitempty"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      *float64      `xml:"time,attr,omitempty"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Error     *junitMessage `xml:"error,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
}

type junitMessage struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

func writeJUnit(path string, outcomes []suiteOutcome) error {
	report := junitReport{}
	for _, outcome := range outcomes {
		if outcome.kind != outcomeRan && outcome.kind != outcomeKilled {
			continue
		}
		result := outcome.result
		suite := junitSuite{Name: result.Package.ImportPath}
		if result.Completed {
			suite.Time = &result.ElapsedSeconds
		}
		for _, test := range result.Tests {
			entry := junitCase{
				Name: test.Name, Classname: suite.Name, Time: test.ElapsedSeconds,
			}
			switch test.Outcome {
			case run.TestFailed:
				entry.Failure = &junitMessage{Message: "test failed", Text: strings.Join(test.Output, "")}
				suite.Failures++
			case run.TestIgnored:
				entry.Skipped = &junitMessage{Message: "test skipped", Text: strings.Join(test.Output, "")}
				suite.Skipped++
			case run.TestPassed:
			default:
				entry.Error = &junitMessage{Message: "test interrupted", Text: strings.Join(test.Output, "")}
				suite.Errors++
			}
			suite.Cases = append(suite.Cases, entry)
		}
		if result.Failed && suite.Failures == 0 {
			suite.Cases = append(suite.Cases, junitCase{
				Name: "[package]", Classname: suite.Name,
				Error: &junitMessage{Message: "package failed", Text: strings.Join(result.PackageOutput, "")},
			})
			suite.Errors++
		}
		suite.Tests = len(suite.Cases)
		report.Tests += suite.Tests
		report.Failures += suite.Failures
		report.Errors += suite.Errors
		report.Skipped += suite.Skipped
		report.Suites = append(report.Suites, suite)
	}
	body, err := xml.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(xml.Header), body...), 0o644)
}

package cli

import (
	"context"
	"strings"
	"testing"
)

func TestParserAcceptsArgsFlagsAroundPositionals(t *testing.T) {
	var captured []string
	var verbose bool
	cmd := &Command{
		Name:        "run",
		AcceptsArgs: true,
		Flags:       []Flag{Bool("verbose", "verbose output")},
		Action: func(_ context.Context, c *Command) error {
			captured = c.Args()
			verbose = c.Bool("verbose")
			return nil
		},
	}
	err := cmd.parse(context.Background(), []string{"file1", "--verbose", "file2"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(captured, ",") != "file1,file2" || !verbose {
		t.Fatalf("args=%v verbose=%v", captured, verbose)
	}
}

func TestParserDoubleDashStopsFlagParsing(t *testing.T) {
	var captured []string
	cmd := &Command{
		Name:        "run",
		AcceptsArgs: true,
		Flags:       []Flag{Bool("verbose", "verbose output")},
		Action: func(_ context.Context, c *Command) error {
			captured = c.Args()
			return nil
		},
	}
	err := cmd.parse(context.Background(), []string{"--", "--verbose", "-x", "test"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(captured, ",") != "--verbose,-x,test" {
		t.Fatalf("args=%v", captured)
	}
}

func TestParserAcceptsIntFlagEqualsValue(t *testing.T) {
	var workers int
	cmd := &Command{
		Name:        "run",
		AcceptsArgs: true,
		Flags:       []Flag{Int("concurrency", "suite workers")},
		Action: func(_ context.Context, c *Command) error {
			workers = c.Int("concurrency")
			return nil
		},
	}
	err := cmd.parse(context.Background(), []string{"--concurrency=8", "./..."})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if workers != 8 || strings.Join(cmd.Args(), ",") != "./..." {
		t.Fatalf("workers=%d args=%v, want workers=8 and pattern", workers, cmd.Args())
	}
}

func TestParserAcceptsIntFlagSpaceValue(t *testing.T) {
	var workers int
	cmd := &Command{
		Name:        "run",
		AcceptsArgs: true,
		Flags:       []Flag{Int("concurrency", "suite workers")},
		Action: func(_ context.Context, c *Command) error {
			workers = c.Int("concurrency")
			return nil
		},
	}
	err := cmd.parse(context.Background(), []string{"--concurrency", "-1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if workers != -1 || len(cmd.Args()) != 0 {
		t.Fatalf("workers=%d args=%v, want negative value and no args", workers, cmd.Args())
	}
}

func TestParserRejectsInvalidIntFlagValue(t *testing.T) {
	cmd := &Command{
		Name:  "run",
		Flags: []Flag{Int("concurrency", "suite workers")},
	}
	err := cmd.parse(context.Background(), []string{"--concurrency=abc"})
	if err == nil || !strings.Contains(err.Error(), "invalid integer value") {
		t.Fatalf("err=%v, want invalid integer value", err)
	}
}

func TestParserRejectsMissingIntFlagValue(t *testing.T) {
	cmd := &Command{
		Name:  "run",
		Flags: []Flag{Int("concurrency", "suite workers")},
	}
	err := cmd.parse(context.Background(), []string{"--concurrency"})
	if err == nil || !strings.Contains(err.Error(), "missing value for flag: concurrency") {
		t.Fatalf("err=%v, want missing value", err)
	}
}

func TestParserStringFlags(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		value string
		set   bool
	}{
		{name: "absent"},
		{name: "equals", args: []string{"--shard=2/5"}, value: "2/5", set: true},
		{name: "space", args: []string{"--shard", "3/7"}, value: "3/7", set: true},
		{name: "empty", args: []string{"--shard="}, set: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &Command{Name: "rask", Flags: []Flag{String("shard", "package shard")}}
			if err := cmd.Run(context.Background(), append([]string{"rask"}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			if cmd.String("shard") != tc.value || cmd.IsSet("shard") != tc.set {
				t.Fatalf("value=%q set=%t, want %q %t", cmd.String("shard"), cmd.IsSet("shard"), tc.value, tc.set)
			}
		})
	}
	cmd := &Command{Name: "rask", Flags: []Flag{String("shard", "package shard")}}
	if err := cmd.Run(context.Background(), []string{"rask", "--shard"}); err == nil {
		t.Fatal("missing string value accepted")
	}
}

func TestParserRoutesSubcommandBeforeCollectingArgs(t *testing.T) {
	ran := ""
	root := &Command{
		Name:        "rask",
		AcceptsArgs: true,
		Action:      func(context.Context, *Command) error { ran = "root"; return nil },
		Commands: []*Command{{
			Name:   "clean",
			Action: func(context.Context, *Command) error { ran = "clean"; return nil },
		}},
	}
	err := root.parse(context.Background(), []string{"clean"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ran != "clean" {
		t.Fatalf("ran=%q, want clean", ran)
	}
}

func TestParserUsesDefaultActionNames(t *testing.T) {
	var captured []string
	cmd := &Command{
		Name:               "rask",
		AcceptsArgs:        true,
		DefaultActionNames: []string{"test", "t"},
		Action: func(_ context.Context, c *Command) error {
			captured = c.Args()
			return nil
		},
	}
	err := cmd.parse(context.Background(), []string{"test", "./..."})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(captured, ",") != "./..." {
		t.Fatalf("args=%v", captured)
	}
}

func TestParserUnknownFlagUsesHint(t *testing.T) {
	cmd := &Command{
		Name:            "rask",
		Flags:           []Flag{Bool("v", "verbose output")},
		UnknownFlagHint: "rask supports only -v",
	}
	err := cmd.parse(context.Background(), []string{"-run"})
	if err == nil {
		t.Fatalf("parse must fail")
	}
	if !strings.Contains(err.Error(), "unknown flag: run") ||
		!strings.Contains(err.Error(), "rask supports only -v") {
		t.Fatalf("err=%q", err.Error())
	}
}

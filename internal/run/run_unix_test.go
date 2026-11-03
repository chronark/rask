//go:build unix

package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chronark/rask/internal/gotool"
)

// TestExecuteCancellationKillsProcessGroup protects grandchild cleanup.
func TestExecuteCancellationKillsProcessGroup(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("missing /bin/sh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool := newShellTool()
	resultCh := make(chan SuiteResult, 1)
	go func() {
		executor := newExecutor(tool, t.TempDir(), time.Second)
		resultCh <- executor.Execute(ctx, suiteForTest("example.com/m/shell"), Hooks{})
	}()
	pids := tool.waitPIDs(t)
	cancel()
	wantCanceledSuite(t, resultCh)
	waitForPIDsGone(t, pids[0], pids[1])
}

// shellTool starts a tiny process tree so Execute cancellation can be observed.
type shellTool struct {
	fakeTool
	reports chan pidReport
}

// newShellTool buffers one pid report so Execute can keep draining stdout.
func newShellTool() *shellTool {
	return &shellTool{reports: make(chan pidReport, 1)}
}

// StartTests returns a shell process that would leak a grandchild without group kill.
func (s *shellTool) StartTests(ctx context.Context, moduleDir string, _ string, _ ...string) (gotool.TestProcess, error) {
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `sleep 60 & echo "$$ $!"; wait`)
	command.Dir = moduleDir
	command.WaitDelay = time.Second
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return killProcessGroup(command)
	}
	process := &shellProcess{command: command}
	command.Stderr = &process.stderr
	events, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("piping shell command: %w", err)
	}
	process.events = &pidReader{reader: events, reports: s.reports}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("starting shell command: %w", err)
	}
	return process, nil
}

// waitPIDs waits until the shell proves both parent and grandchild exist.
func (s *shellTool) waitPIDs(t *testing.T) [2]int {
	t.Helper()
	select {
	case report := <-s.reports:
		if report.err != nil {
			t.Fatalf("read shell pids: %v", report.err)
		}
		return report.pids
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for shell pids")
	}
	return [2]int{}
}

// shellProcess adapts exec.Cmd to gotool.TestProcess for executor tests.
type shellProcess struct {
	command *exec.Cmd
	events  io.Reader
	stderr  bytes.Buffer
}

// Events returns stdout so Execute owns draining before Wait.
func (p *shellProcess) Events() io.Reader {
	return p.events
}

// Wait reaps the shell after Execute cancels its context.
func (p *shellProcess) Wait() error {
	return p.command.Wait()
}

// Stderr returns shell diagnostics after the process exits.
func (p *shellProcess) Stderr() string {
	return p.stderr.String()
}

// pidReport carries either parsed shell pids or the parse failure.
type pidReport struct {
	pids [2]int
	err  error
}

// pidReader forwards stdout while extracting the first pid announcement.
type pidReader struct {
	reader   io.Reader
	reports  chan<- pidReport
	line     strings.Builder
	reported bool
}

// Read forwards process output and reports pids once the first line is complete.
func (r *pidReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && !r.reported {
		r.record(p[:n])
	}
	return n, err
}

// record buffers output until the shell pid line is parseable.
func (r *pidReader) record(chunk []byte) {
	r.line.Write(chunk)
	text := r.line.String()
	index := strings.IndexByte(text, '\n')
	if index < 0 {
		return
	}
	r.reported = true
	pids, err := parsePIDLine(text[:index])
	r.reports <- pidReport{pids: pids, err: err}
}

// parsePIDLine converts the shell announcement into numeric process ids.
func parsePIDLine(line string) ([2]int, error) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return [2]int{}, fmt.Errorf("pid line = %q, want two fields", line)
	}
	parent, err := strconv.Atoi(fields[0])
	if err != nil {
		return [2]int{}, fmt.Errorf("parse parent pid %q: %w", fields[0], err)
	}
	child, err := strconv.Atoi(fields[1])
	if err != nil {
		return [2]int{}, fmt.Errorf("parse child pid %q: %w", fields[1], err)
	}
	return [2]int{parent, child}, nil
}

// killProcessGroup mirrors gotool cancellation so the executor path proves it.
func killProcessGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
		return command.Process.Kill()
	}
	return nil
}

// waitForPIDsGone polls because process reaping is scheduler-dependent.
func waitForPIDsGone(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if allPIDsGone(pids) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("processes still exist after group kill: %v", pids)
}

// allPIDsGone keeps the retry loop focused on the terminal condition.
func allPIDsGone(pids []int) bool {
	for _, pid := range pids {
		if processExists(pid) {
			return false
		}
	}
	return true
}

// processExists treats EPERM as alive because only ESRCH proves cleanup.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

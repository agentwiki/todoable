package local

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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

// Runner executes one committed step using its submission snapshot.
type Runner struct {
	Dir          string
	Started      func(domain.ProcessIdentity) error
	LogBytes     int64
	Cancelled    func(string) (bool, error)
	RunRemaining func(string) (int64, error)
}

func (r Runner) Execute(ctx context.Context, step domain.Execution) (out domain.Outcome, err error) {
	out.Kind = "start_failed"
	out.ExitCode = -1
	runDir, err := filepath.Abs(filepath.Join(r.Dir, "runs", step.RunID))
	if err != nil {
		return out, err
	}
	stepDir := filepath.Join(runDir, "steps", step.StepID)
	if err = os.MkdirAll(stepDir, 0700); err != nil {
		return out, err
	}
	contextPath := filepath.Join(stepDir, "context.json")
	data, err := json.Marshal(map[string]any{
		"protocol_version": 1, "task_id": step.TaskID, "task_version": step.TaskVersion,
		"submission_id": step.SubmissionID, "run_id": step.RunID, "run_seq": step.RunSeq,
		"step_id": step.StepID, "stage": step.Stage, "call_index": step.CallIndex,
		"input_key": step.InputKey, "input": step.Input, "prompt": step.Task.Prompt,
		"workdir": step.Task.Workdir, "run_dir": runDir, "last_check": step.LastCheck,
	})
	if err != nil {
		return out, err
	}
	f, err := os.OpenFile(contextPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return out, err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return out, err
	}
	out.StdoutPath = filepath.Join(stepDir, "stdout")
	out.StderrPath = filepath.Join(stepDir, "stderr")
	stdout, err := os.OpenFile(out.StdoutPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, stdout.Close()) }()
	stderr, err := os.OpenFile(out.StderrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, stderr.Close()) }()
	argv := stageCommand(step)
	if len(argv) == 0 {
		return out, fmt.Errorf("missing command for %s", step.Stage)
	}
	executable, resolveErr := snapshotExecutable(argv[0], step.Task)
	if resolveErr != nil {
		out.Stderr = resolveErr.Error()
		return out, nil
	}
	command := &exec.Cmd{Path: executable, Args: append([]string(nil), argv...), Dir: step.Task.Workdir, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	environment := make(map[string]string, len(step.Task.Env)+8)
	for key, value := range step.Task.Env {
		if !strings.HasPrefix(key, "TODOABLE_") {
			environment[key] = value
		}
	}
	environment["TODOABLE_CONTEXT_PATH"] = contextPath
	environment["TODOABLE_RUN_DIR"] = runDir
	environment["TODOABLE_TASK_ID"] = step.TaskID
	environment["TODOABLE_TASK_VERSION"] = strconv.Itoa(step.TaskVersion)
	environment["TODOABLE_SUBMISSION_ID"] = step.SubmissionID
	environment["TODOABLE_RUN_ID"] = step.RunID
	environment["TODOABLE_STEP_ID"] = step.StepID
	environment["TODOABLE_CALL_INDEX"] = strconv.Itoa(step.CallIndex)
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	sort.Strings(command.Env)
	if step.Stage == "agent" {
		command.Stdin = bytes.NewReader(data)
	}
	capBytes := r.LogBytes
	if capBytes <= 0 {
		capBytes = 10485760
	}
	capture := &logCapture{remaining: capBytes}
	stdoutStream := &logStream{capture: capture, file: stdout}
	stderrStream := &logStream{capture: capture, file: stderr}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return out, err
	}
	defer stdoutRead.Close()
	defer stdoutWrite.Close()
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		return out, err
	}
	defer stderrRead.Close()
	defer stderrWrite.Close()
	command.Stdout = stdoutWrite
	command.Stderr = stderrWrite
	if r.Cancelled != nil {
		cancelled, e := r.Cancelled(step.StepID)
		if e != nil {
			return out, e
		}
		if cancelled {
			out.Kind = "not_started"
			return out, nil
		}
	}
	if step.Stage != "start_check" && r.RunRemaining != nil {
		remaining, e := r.RunRemaining(step.StepID)
		if e != nil {
			return out, e
		}
		if remaining <= 0 {
			out.Kind = "budget_exhausted"
			return out, nil
		}
		if remaining < step.TimeoutNS {
			step.TimeoutNS = remaining
		}
	}
	if ctx.Err() != nil {
		out.Kind = "unknown"
		return out, nil
	}
	started := time.Now()
	if startErr := command.Start(); startErr != nil {
		out.Stderr = startErr.Error()
		return out, nil
	}
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	var drains sync.WaitGroup
	drains.Add(2)
	go func() { defer drains.Done(); _, _ = io.Copy(stdoutStream, stdoutRead) }()
	go func() { defer drains.Done(); _, _ = io.Copy(stderrStream, stderrRead) }()
	out.PID = command.Process.Pid
	out.PGID = out.PID
	out.BootID, out.ProcessStart, err = processIdentity(out.PID)
	out.Kind = "unknown"
	if err == nil && r.Started != nil {
		err = r.Started(domain.ProcessIdentity{StepID: step.StepID, PID: out.PID, PGID: out.PGID, BootID: out.BootID, ProcessStart: out.ProcessStart})
	}
	// The leader is reaped separately from pipe draining: descendants may
	// continue producing output after their leader exits.
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	timeout := time.Duration(step.TimeoutNS)
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	timer := time.NewTimer(max(time.Nanosecond, timeout-time.Since(started)))
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var waitErr error
	leaderDone := false
	interrupted := err != nil
	stopAt := time.Time{}
	killed := false
	if interrupted {
		_ = syscall.Kill(-out.PGID, syscall.SIGTERM)
		stopAt = time.Now().Add(10 * time.Second)
	}
	for {
		alive, scanErr := groupAlive(out.PGID)
		if scanErr != nil {
			out.Kind = "process_unknown"
			err = errors.Join(err, scanErr)
			break
		}
		if !alive && leaderDone {
			break
		}
		select {
		case waitErr = <-waited:
			leaderDone = true
			waited = nil
		case <-ctx.Done():
			if !interrupted {
				interrupted = true
				_ = syscall.Kill(-out.PGID, syscall.SIGTERM)
				stopAt = time.Now().Add(10 * time.Second)
			}
			// Avoid continuously selecting an already cancelled context.
			ctx = context.Background()
		case <-timer.C:
			if !interrupted {
				interrupted = true
				_ = syscall.Kill(-out.PGID, syscall.SIGTERM)
				stopAt = time.Now().Add(10 * time.Second)
			}
		case <-ticker.C:
			if r.Cancelled != nil && !interrupted {
				cancelled, e := r.Cancelled(step.StepID)
				if e != nil {
					err = errors.Join(err, e)
				}
				if cancelled || e != nil {
					interrupted = true
					_ = syscall.Kill(-out.PGID, syscall.SIGTERM)
					stopAt = time.Now().Add(10 * time.Second)
				}
			}
		}
		if interrupted && !stopAt.IsZero() && !time.Now().Before(stopAt) {
			if !killed {
				_ = syscall.Kill(-out.PGID, syscall.SIGKILL)
				killed = true
				stopAt = time.Now().Add(time.Second)
			} else {
				out.Kind = "process_unknown"
				break
			}
		}
	}
	if out.Kind == "process_unknown" {
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
	}
	drained := make(chan struct{})
	go func() { drains.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(time.Second):
		// A command that escaped its group can retain inherited pipe handles.
		out.Untracked = true
		out.Kind = "process_unknown"
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
		<-drained
	}
	out.ElapsedNS = time.Since(started).Nanoseconds()
	if out.Kind != "process_unknown" && !interrupted && leaderDone {
		var exitErr *exec.ExitError
		if waitErr == nil || errors.As(waitErr, &exitErr) {
			status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
			if ok && status.Exited() {
				out.Kind = "exited"
				out.ExitCode = status.ExitStatus()
			}
		}
	}
	capture.mu.Lock()
	out.Stdout = strings.ToValidUTF8(string(stdoutStream.feedback), "\uFFFD")
	out.Stderr = strings.ToValidUTF8(string(stderrStream.feedback), "\uFFFD")
	out.Truncated = capture.truncated
	err = errors.Join(err, capture.err)
	capture.mu.Unlock()
	err = errors.Join(err, stdout.Sync(), stderr.Sync())
	if err != nil && out.Kind == "exited" {
		out.Kind = "unknown"
	}
	return out, err
}

func stageCommand(step domain.Execution) []string {
	switch step.Stage {
	case "start_check":
		if step.Task.Start != nil {
			return step.Task.Start.Check
		}
	case "finish_check":
		return step.Task.Finish.Check
	case "before":
		return step.Task.Before
	case "after":
		return step.Task.After
	case "agent":
		return step.Task.Agent
	}
	return nil
}

func snapshotExecutable(name string, task domain.Task) (string, error) {
	if strings.ContainsRune(name, '/') {
		return name, nil
	}
	for _, dir := range filepath.SplitList(task.Env["PATH"]) {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(task.Workdir, dir)
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in snapshot PATH", name)
}

type logCapture struct {
	mu        sync.Mutex
	remaining int64
	truncated bool
	err       error
}
type logStream struct {
	capture  *logCapture
	file     *os.File
	feedback []byte
}

func (s *logStream) Write(p []byte) (int, error) {
	s.capture.mu.Lock()
	defer s.capture.mu.Unlock()
	n := len(p)
	feedbackN := min(n, 8192-len(s.feedback))
	s.feedback = append(s.feedback, p[:feedbackN]...)
	if feedbackN < n {
		s.capture.truncated = true
	}
	keep := min(int64(n), s.capture.remaining)
	if keep < int64(n) {
		s.capture.truncated = true
	}
	if keep > 0 {
		written, err := s.file.Write(p[:keep])
		s.capture.remaining -= int64(written)
		if err == nil && int64(written) != keep {
			err = io.ErrShortWrite
		}
		s.capture.err = errors.Join(s.capture.err, err)
	}
	// Continue draining after quota exhaustion or disk failures.
	return n, nil
}

func processIdentity(pid int) (string, string, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", err
	}
	fields, err := processFields(pid)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(boot)), fields[19], nil
}
func processFields(pid int) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil, err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return nil, fmt.Errorf("invalid proc stat for %d", pid)
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return nil, fmt.Errorf("short proc stat for %d", pid)
	}
	return fields, nil
}
func groupAlive(pgid int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true, err
	}
	return scanProcessGroup(entries, pgid, processFields)
}

func scanProcessGroup(entries []os.DirEntry, pgid int, read func(int) ([]string, error)) (bool, error) {
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}
		fields, readErr := read(pid)
		// A process may disappear after ReadDir and before its proc stat read.
		if os.IsNotExist(readErr) || errors.Is(readErr, syscall.ESRCH) {
			continue
		}
		if readErr != nil {
			return true, readErr
		}
		group, parseErr := strconv.Atoi(fields[2])
		if parseErr != nil {
			return true, parseErr
		}
		if group == pgid && fields[0] != "Z" && fields[0] != "X" {
			return true, nil
		}
	}
	return false, nil
}

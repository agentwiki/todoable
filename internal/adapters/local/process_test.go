package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

func executionFixture(t *testing.T) domain.Execution {
	t.Helper()
	return domain.Execution{StepID: "step", RunID: "run", SubmissionID: "submission", TaskID: "task", TaskVersion: 3, RunSeq: 2, CallIndex: 1, InputKey: "item", Input: json.RawMessage(`{"n":7}`), Stage: "agent", TimeoutNS: int64(5 * time.Second), Task: domain.Task{Workdir: t.TempDir(), Prompt: "process this input", Env: map[string]string{"PATH": "/usr/bin:/bin", "FIXTURE": "snapshot"}}}
}

func TestRunnerContextAndSnapshot(t *testing.T) {
	step := executionFixture(t)
	step.Task.Agent = []string{"sh", "-c", `cat; printf '\n%s' "$FIXTURE:${TODOABLE_CALL_INDEX}:$UNSELECTED" >&2`}
	t.Setenv("FIXTURE", "ambient")
	t.Setenv("UNSELECTED", "must-not-leak")
	var identity domain.ProcessIdentity
	runner := Runner{Dir: t.TempDir(), Started: func(p domain.ProcessIdentity) error { identity = p; return nil }}
	out, err := runner.Execute(context.Background(), step)
	if err != nil || out.Kind != "exited" || out.ExitCode != 0 {
		t.Fatalf("execute: %+v %v", out, err)
	}
	if out.Stderr != "\nsnapshot:1:" {
		t.Fatalf("environment: %q", out.Stderr)
	}
	var payload map[string]any
	if err = json.Unmarshal([]byte(out.Stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["task_version"] != float64(3) || payload["run_seq"] != float64(2) || payload["last_check"] != nil || payload["prompt"] != "process this input" {
		t.Fatalf("context: %v", payload)
	}
	contextPath := filepath.Join(runner.Dir, "runs", "run", "steps", "step", "context.json")
	data, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != out.Stdout {
		t.Fatal("stdin and immutable context differ")
	}
	info, err := os.Stat(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0222 != 0 {
		t.Fatal("context is writable")
	}
	if identity.PID <= 0 || identity.PGID != identity.PID || identity.BootID == "" || identity.ProcessStart == "" || identity.StepID != "step" {
		t.Fatalf("identity: %+v", identity)
	}
	if _, err = runner.Execute(context.Background(), step); err == nil {
		t.Fatal("existing step context overwritten")
	}
}

func TestRunnerCheckEOFAndNonzero(t *testing.T) {
	step := executionFixture(t)
	step.Stage = "finish_check"
	step.Task.Finish.Check = []string{"sh", "-c", `read value && exit 9; printf eof; exit 7`}
	out, err := (Runner{Dir: t.TempDir()}).Execute(context.Background(), step)
	if err != nil || out.Kind != "exited" || out.ExitCode != 7 || out.Stdout != "eof" {
		t.Fatalf("check: %+v %v", out, err)
	}
}

func TestRunnerDescendantAndTimeout(t *testing.T) {
	t.Run("descendant", func(t *testing.T) {
		step := executionFixture(t)
		step.Task.Agent = []string{"sh", "-c", `(sleep 1.1; printf descendant) & exit 0`}
		out, err := (Runner{Dir: t.TempDir()}).Execute(context.Background(), step)
		if err != nil || out.Kind != "exited" || out.Stdout != "descendant" || out.ElapsedNS < int64(time.Second) {
			t.Fatalf("descendant lost: %+v %v", out, err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		step := executionFixture(t)
		step.TimeoutNS = int64(30 * time.Millisecond)
		step.Task.Agent = []string{"sleep", "5"}
		out, err := (Runner{Dir: t.TempDir()}).Execute(context.Background(), step)
		if err != nil || out.Kind != "unknown" {
			t.Fatalf("timeout: %+v %v", out, err)
		}
		alive, err := groupAlive(out.PGID)
		if err != nil || alive {
			t.Fatalf("group survived timeout: %v %v", alive, err)
		}
	})
}

func TestRunnerLogsCapAndRawBytes(t *testing.T) {
	step := executionFixture(t)
	step.Task.Agent = []string{"sh", "-c", `printf '\377abc'; printf '123456789' >&2`}
	out, err := (Runner{Dir: t.TempDir(), LogBytes: 6}).Execute(context.Background(), step)
	if err != nil || out.Kind != "exited" || !out.Truncated {
		t.Fatalf("logs: %+v %v", out, err)
	}
	if out.Stdout != "�abc" || out.Stderr != "123456789" {
		t.Fatalf("feedback: %+v", out)
	}
	stdout, err := os.ReadFile(out.StdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(out.StderrPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout)+len(stderr) != 6 {
		t.Fatalf("combined log length %d", len(stdout)+len(stderr))
	}
	if len(stdout) > 0 && stdout[0] != 255 {
		t.Fatalf("raw bytes transformed: %v", stdout)
	}
	step.StepID = "large"
	step.Task.Agent = []string{"sh", "-c", `head -c 12000000 /dev/zero | tr '\000' x`}
	out, err = (Runner{Dir: t.TempDir()}).Execute(context.Background(), step)
	if err != nil || out.Kind != "exited" || len(out.Stdout) != 8192 || !out.Truncated || strings.Trim(out.Stdout, "x") != "" {
		t.Fatalf("large output: kind=%s len=%d error=%v", out.Kind, len(out.Stdout), err)
	}
	info, err := os.Stat(out.StdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 10485760 {
		t.Fatalf("raw cap: %d", info.Size())
	}
}

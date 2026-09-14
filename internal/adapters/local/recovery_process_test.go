package local

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

func recoveryFixture(t *testing.T, script string) (*exec.Cmd, domain.ProcessIdentity, string) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "TODOABLE_STEP_ID=recovery-fixture", "FIXTURE_DIR="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	boot, start, err := processIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	owner := domain.ProcessIdentity{StepID: "recovery-fixture", PID: cmd.Process.Pid, PGID: cmd.Process.Pid, BootID: boot, ProcessStart: start}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	return cmd, owner, dir
}

func assertRecoveryPIDAlive(t *testing.T, pid int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		t.Fatalf("foreign process disappeared: %v", err)
	}
	tail := string(raw[strings.LastIndexByte(string(raw), ')')+1:])
	state := strings.Fields(tail)[0]
	if state == "Z" || state == "X" {
		t.Fatalf("foreign process died: %s", state)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("process not alive: %v", err)
	}
}

func TestRecordedProcessOwnershipRefusesForeignProcess(t *testing.T) {
	for _, kind := range []string{"start", "step", "group", "missing"} {
		t.Run(kind, func(t *testing.T) {
			cmd, owner, _ := recoveryFixture(t, `touch "$FIXTURE_DIR/ready"; exec sleep 60`)
			switch kind {
			case "start":
				owner.ProcessStart = "0"
			case "step":
				owner.StepID = "different-step"
			case "group":
				owner.PGID++
			case "missing":
				owner.BootID = ""
			}
			stopped, err := StopRecordedProcess(context.Background(), owner)
			if err != nil || stopped {
				t.Fatalf("foreign process accepted: stopped=%v err=%v", stopped, err)
			}
			assertRecoveryPIDAlive(t, cmd.Process.Pid)
			stopped, err = RecordedProcessStopped(owner)
			if err != nil || stopped {
				t.Fatalf("foreign process called stopped: %v %v", stopped, err)
			}
		})
	}
}

func TestRecordedProcessPreviousBootDoesNotSignal(t *testing.T) {
	cmd, owner, _ := recoveryFixture(t, `touch "$FIXTURE_DIR/ready"; exec sleep 60`)
	owner.BootID = "previous-boot"
	stopped, err := StopRecordedProcess(context.Background(), owner)
	if err != nil || !stopped {
		t.Fatalf("previous boot: %v %v", stopped, err)
	}
	assertRecoveryPIDAlive(t, cmd.Process.Pid)
}

func TestRecordedProcessStopsOrphanGroup(t *testing.T) {
	cmd, owner, dir := recoveryFixture(t, `sleep 60 & echo $! > "$FIXTURE_DIR/child"; touch "$FIXTURE_DIR/ready"; while [ ! -e "$FIXTURE_DIR/exit" ]; do sleep 0.01; done`)
	raw, err := os.ReadFile(filepath.Join(dir, "child"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	assertRecoveryPIDAlive(t, child)
	futureOwner := owner
	start, err := strconv.ParseUint(owner.ProcessStart, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	futureOwner.ProcessStart = strconv.FormatUint(start+1000000, 10)
	stopped, err := StopRecordedProcess(context.Background(), futureOwner)
	if err != nil || stopped {
		t.Fatalf("child predating recorded leader accepted: %v %v", stopped, err)
	}
	assertRecoveryPIDAlive(t, child)
	stopped, err = RecordedProcessStopped(owner)
	if err != nil || stopped {
		t.Fatalf("live orphan reported stopped: %v %v", stopped, err)
	}
	stopped, err = StopRecordedProcess(context.Background(), owner)
	if err != nil || !stopped {
		t.Fatalf("orphan stop: %v %v", stopped, err)
	}
	raw, err = os.ReadFile(filepath.Join("/proc", strconv.Itoa(child), "stat"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err == nil {
		state := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))[0]
		if state != "Z" && state != "X" {
			t.Fatalf("orphan still live: %s", string(raw))
		}
	}
}

func TestRecordedProcessEscalatesAfterTERMGrace(t *testing.T) {
	cmd, owner, _ := recoveryFixture(t, `trap '' TERM; touch "$FIXTURE_DIR/ready"; exec sleep 60`)
	started := time.Now()
	stopped, err := StopRecordedProcess(context.Background(), owner)
	elapsed := time.Since(started)
	if err != nil || !stopped {
		t.Fatalf("stop: %v %v", stopped, err)
	}
	if elapsed < 10*time.Second || elapsed > 13*time.Second {
		t.Fatalf("TERM grace not respected: %v", elapsed)
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected signal death: %v", err)
	}
	status := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("not killed after ignored TERM: %v", status)
	}
}

func TestRecordedProcessCancellationDoesNotClaimStopped(t *testing.T) {
	cmd, owner, _ := recoveryFixture(t, `touch "$FIXTURE_DIR/ready"; exec sleep 60`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped, err := StopRecordedProcess(ctx, owner)
	if stopped || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stop: %v %v", stopped, err)
	}
	assertRecoveryPIDAlive(t, cmd.Process.Pid)
}

func TestRecordedProcessRefusesForeignGroupMember(t *testing.T) {
	cmd, owner, dir := recoveryFixture(t, `env TODOABLE_STEP_ID=foreign sleep 60 & echo $! > "$FIXTURE_DIR/child"; touch "$FIXTURE_DIR/ready"; wait`)
	raw, err := os.ReadFile(filepath.Join(dir, "child"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// Wait for env to exec sleep so its initial environment is observable.
	deadline := time.Now().Add(time.Second)
	for {
		environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(child), "environ"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(environ), "TODOABLE_STEP_ID=foreign\x00") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("foreign child failed to exec")
		}
		time.Sleep(time.Millisecond)
	}
	stopped, err := StopRecordedProcess(context.Background(), owner)
	if stopped || err != nil {
		t.Fatalf("foreign member not rejected: %v %v", stopped, err)
	}
	assertRecoveryPIDAlive(t, cmd.Process.Pid)
	assertRecoveryPIDAlive(t, child)
}

func TestRecordedProcessTERMAndCancellationDuringGrace(t *testing.T) {
	t.Run("TERM", func(t *testing.T) {
		cmd, owner, _ := recoveryFixture(t, `touch "$FIXTURE_DIR/ready"; exec sleep 60`)
		stopped, err := StopRecordedProcess(context.Background(), owner)
		if err != nil || !stopped {
			t.Fatalf("TERM stop: %v %v", stopped, err)
		}
		_ = cmd.Wait()
		status := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Fatalf("expected TERM: %v", status)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		cmd, owner, _ := recoveryFixture(t, `trap '' TERM; touch "$FIXTURE_DIR/ready"; exec sleep 60`)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		stopped, err := StopRecordedProcess(ctx, owner)
		if stopped || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancelled grace: %v %v", stopped, err)
		}
		assertRecoveryPIDAlive(t, cmd.Process.Pid)
	})
}

func TestRecordedProcessPinnedDescriptorNeverSignalsReplacementPID(t *testing.T) {
	original, _, _ := recoveryFixture(t, `touch "$FIXTURE_DIR/ready"; exec sleep 60`)
	fd, err := openRecordedPID(original.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := original.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = original.Wait()
	foreign, _, _ := recoveryFixture(t, `touch "$FIXTURE_DIR/ready"; exec sleep 60`)
	// A stale numeric lookup now names a different live process. The descriptor
	// still identifies the dead original; raw kill(member.pid) would kill foreign.
	member := recordedMember{fd: fd, pid: foreign.Process.Pid, start: "stale"}
	err = signalRecordedMembers(context.Background(), []recordedMember{member}, syscall.SIGKILL, map[string]syscall.Signal{})
	if err != nil {
		t.Fatal(err)
	}
	assertRecoveryPIDAlive(t, foreign.Process.Pid)
}

func TestRecordedProcessRejectsChangedIdentityAfterPinning(t *testing.T) {
	cmd, _, _ := recoveryFixture(t, `touch "$FIXTURE_DIR/ready"; exec sleep 60`)
	before, err := processFields(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	after := append([]string(nil), before...)
	if !sameRecordedProcess(before, after) {
		t.Fatal("unchanged process rejected")
	}
	after[19] = "replacement-start"
	if sameRecordedProcess(before, after) {
		t.Fatal("replacement start accepted after pinning")
	}
	after = append([]string(nil), before...)
	after[2] = "replacement-group"
	if sameRecordedProcess(before, after) {
		t.Fatal("replacement group accepted after pinning")
	}
	assertRecoveryPIDAlive(t, cmd.Process.Pid)
}

func TestRecordedProcessRescansChildrenForkedOnTERM(t *testing.T) {
	cmd, owner, dir := recoveryFixture(t, `trap 'sleep 60 & echo $! > "$FIXTURE_DIR/new-child"; exit 0' TERM; touch "$FIXTURE_DIR/ready"; while :; do sleep 1; done`)
	stopped, err := StopRecordedProcess(context.Background(), owner)
	if err != nil || !stopped {
		t.Fatalf("stop newly forked child: %v %v", stopped, err)
	}
	_ = cmd.Wait()
	raw, err := os.ReadFile(filepath.Join(dir, "new-child"))
	if err != nil {
		t.Fatalf("TERM handler did not spawn child: %v", err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join("/proc", strconv.Itoa(child), "stat"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	state := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))[0]
	if state != "Z" && state != "X" {
		t.Fatalf("new child survived: %s", raw)
	}
}

package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

// RecordedProcessStopped confirms disappearance of the recorded group without
// sending a signal. Unknown ownership is deliberately not proof of termination.
func RecordedProcessStopped(owner domain.ProcessIdentity) (bool, error) {
	stopped, _, err := recordedGroup(owner)
	return stopped, err
}

// StopRecordedProcess pins verified group members with pidfds before signaling.
// New descendants discovered during the grace period receive TERM as well.
func StopRecordedProcess(ctx context.Context, owner domain.ProcessIdentity) (bool, error) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	started := time.Time{}
	sent := make(map[string]syscall.Signal)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		stopped, owned, members, err := inspectRecordedGroup(owner, true)
		if cancelErr := ctx.Err(); cancelErr != nil {
			closeRecordedMembers(members)
			return false, cancelErr
		}
		if err != nil || stopped || !owned {
			return stopped, err
		}
		if started.IsZero() {
			started = time.Now()
		}
		signal := syscall.SIGTERM
		if time.Since(started) >= 10*time.Second {
			signal = syscall.SIGKILL
		}
		if time.Since(started) >= 11*time.Second {
			closeRecordedMembers(members)
			return false, nil
		}
		err = signalRecordedMembers(ctx, members, signal, sent)
		closeRecordedMembers(members)
		if err != nil {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-tick.C:
		}
	}
}

type recordedMember struct {
	fd    int
	pid   int
	start string
}

// Linux amd64 asm/unistd_64.h and arm64's asm-generic/unistd.h both
// define __NR_pidfd_open=434 and __NR_pidfd_send_signal=424.
const recoveryPIDFDOpen = 434
const recoveryPIDFDSendSignal = 424

func openRecordedPID(pid int) (int, error) {
	fd, _, errno := syscall.Syscall(recoveryPIDFDOpen, uintptr(pid), 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

func closeRecordedMembers(members []recordedMember) {
	for _, member := range members {
		_ = syscall.Close(member.fd)
	}
}

func signalRecordedMembers(ctx context.Context, members []recordedMember, signal syscall.Signal, sent map[string]syscall.Signal) error {
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := strconv.Itoa(member.pid) + ":" + member.start
		if sent[key] == signal {
			continue
		}
		// This syscall targets the pinned process even if its numeric PID has since
		// been reused. Unsupported pidfds fail closed; there is no kill(PID) fallback.
		_, _, errno := syscall.Syscall6(recoveryPIDFDSendSignal, uintptr(member.fd), uintptr(signal), 0, 0, 0, 0)
		if errno != 0 && errno != syscall.ESRCH {
			return errno
		}
		sent[key] = signal
	}
	return nil
}

// recordedGroup returns (stopped, safe to signal, error). A missing leader does
// not prove the group is gone: children can outlive it and retain its PGID.
func recordedGroup(owner domain.ProcessIdentity) (bool, bool, error) {
	stopped, owned, _, err := inspectRecordedGroup(owner, false)
	return stopped, owned, err
}

func inspectRecordedGroup(owner domain.ProcessIdentity, pin bool) (stopped, owned bool, members []recordedMember, err error) {
	// No descriptors escape unless the entire group has been verified.
	defer func() {
		if !owned {
			closeRecordedMembers(members)
			members = nil
		}
	}()
	if owner.PID <= 1 || owner.PGID <= 1 || owner.StepID == "" || owner.BootID == "" || owner.ProcessStart == "" {
		return false, false, members, nil
	}
	start, err := strconv.ParseUint(owner.ProcessStart, 10, 64)
	if err != nil {
		return false, false, members, nil
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return false, false, members, err
	}
	if strings.TrimSpace(string(boot)) != owner.BootID {
		return true, false, members, nil
	}
	leader, err := processFields(owner.PID)
	if err == nil {
		if leader[19] != owner.ProcessStart || leader[2] != strconv.Itoa(owner.PGID) {
			return false, false, members, nil
		}
	} else if !procDisappeared(err) {
		return false, false, members, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false, members, err
	}
	alive := false
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}
		fields, readErr := processFields(pid)
		if procDisappeared(readErr) {
			continue
		}
		if readErr != nil {
			return false, false, members, readErr
		}
		if pid == owner.PID && (fields[19] != owner.ProcessStart || fields[2] != strconv.Itoa(owner.PGID)) {
			return false, false, members, nil
		}
		group, parseErr := strconv.Atoi(fields[2])
		if parseErr != nil {
			return false, false, members, parseErr
		}
		if group != owner.PGID || fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		// Even if this member exits while being inspected, require another
		// scan: it may have forked a child after /proc's directory snapshot.
		alive = true
		memberStart, parseErr := strconv.ParseUint(fields[19], 10, 64)
		if parseErr != nil {
			return false, false, members, parseErr
		}
		if memberStart < start {
			return false, false, members, nil
		}
		if pin {
			fd, openErr := openRecordedPID(pid)
			if procDisappeared(openErr) {
				continue
			}
			if openErr != nil {
				return false, false, members, openErr
			}
			members = append(members, recordedMember{fd: fd, pid: pid, start: fields[19]})
			pinned, readErr := processFields(pid)
			if procDisappeared(readErr) {
				continue
			}
			if readErr != nil {
				return false, false, members, readErr
			}
			if !sameRecordedProcess(fields, pinned) {
				return false, false, members, nil
			}
		}
		environ, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if procDisappeared(readErr) {
			continue
		}
		if readErr != nil {
			return false, false, members, readErr
		}
		// Re-read after environ so an exiting process or PID reuse cannot lend its
		// environment to a different process's stat record.
		current, readErr := processFields(pid)
		if procDisappeared(readErr) {
			continue
		}
		if readErr != nil {
			return false, false, members, readErr
		}
		if current[0] == "Z" || current[0] == "X" {
			continue
		}
		if !sameRecordedProcess(fields, current) {
			return false, false, members, nil
		}
		matches := 0
		for _, value := range strings.Split(string(environ), "\x00") {
			if strings.HasPrefix(value, "TODOABLE_STEP_ID=") {
				if value != "TODOABLE_STEP_ID="+owner.StepID {
					return false, false, members, nil
				}
				matches++
			}
		}
		if matches != 1 {
			return false, false, members, nil
		}
	}
	return !alive, alive, members, nil
}

func procDisappeared(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ESRCH)
}

func sameRecordedProcess(before, after []string) bool {
	return before[19] == after[19] && before[2] == after[2]
}

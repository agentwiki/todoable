package local

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

type Daemon struct {
	lock        *os.File
	context     context.Context
	stop        context.CancelFunc
	nextCleanup time.Time
}

func (s *Store) Daemon() (*Daemon, error) {
	publication, e := s.configPublication(syscall.LOCK_EX)
	if e != nil {
		return nil, e
	}
	defer func() { _ = publication.Close() }()
	lock, e := os.OpenFile(filepath.Join(s.dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	deadline := time.Now().Add(time.Second)
	for {
		e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			break
		}
		if !errors.Is(e, syscall.EWOULDBLOCK) {
			_ = lock.Close()
			return nil, e
		}
		if !time.Now().Before(deadline) {
			_ = lock.Close()
			return nil, &domain.Fault{Code: 5, Kind: "daemon_running", Message: "a daemon owns this data directory"}
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Overwrite the fixed-width hash before trimming stale bytes, so an
	// existing complete record is never replaced by an empty publication.
	_, e = lock.WriteAt([]byte(s.ConfigHash()), 0)
	if e == nil {
		e = lock.Truncate(64)
	}
	if e == nil {
		e = lock.Sync()
	}
	if e != nil {
		_ = lock.Close()
		return nil, e
	}
	_ = publication.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	e = s.recoverIntents(ctx)

	if e != nil {
		s.health.recovery = true
		s.PauseStorage(e)
	} else {
		_ = os.Remove(filepath.Join(s.dir, "storage_paused"))
	}
	return &Daemon{lock: lock, context: ctx, stop: stop}, nil
}
func (d *Daemon) Context() context.Context { return d.context }
func (d *Daemon) Pause() bool {
	select {
	case <-d.context.Done():
		return false
	case <-time.After(20 * time.Millisecond):
		return true
	}
}
func (d *Daemon) Close() error { d.stop(); return d.lock.Close() }
func (d *Daemon) Stopped(err error) bool {
	return errors.Is(err, context.Canceled) || d.context.Err() != nil
}
func (s *Store) Executor() *Runner {
	return &Runner{Dir: s.dir, Started: s.Started, LogBytes: s.config.StepLogBytes, Cancelled: s.CancellationRequested, RunRemaining: s.RemainingRunTime, Failed: s.failedExecution, SyncFile: s.syncLogFile, StorageFailure: s.PauseStorage, AdmissionOpen: func() bool { return !s.storagePaused() }}
}

func (d *Daemon) Stop() { d.stop() }

// CleanupDue is called only by the daemon's single maintenance loop.
func (d *Daemon) CleanupDue() bool {
	now := time.Now()
	if now.Before(d.nextCleanup) {
		return false
	}
	d.nextCleanup = now.Add(time.Second)
	return true
}

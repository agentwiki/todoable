package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

type storageHealth struct {
	mu       sync.Mutex
	probe    sync.Mutex
	paused   bool
	epoch    uint64
	recovery bool
	next     time.Time
	pending  map[string]domain.Execution
}

// PauseStorage stops admission while already running commands may still save results.
func (s *Store) PauseStorage(err error) {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	s.pauseStorageLocked(err)
}
func (s *Store) pauseStorageLocked(err error) {
	if !s.health.paused {
		s.health.next = time.Now().Add(time.Second)
	}
	s.health.paused = true
	s.health.epoch++
	_, _ = fmt.Fprintf(os.Stderr, "storage_paused: %v\n", err)
	_ = os.WriteFile(filepath.Join(s.dir, "storage_paused"), []byte(err.Error()), 0600)
}
func (s *Store) failedExecution(x domain.Execution, err error) {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	s.pauseStorageLocked(err)
	if s.health.pending == nil {
		s.health.pending = make(map[string]domain.Execution)
	}
	s.health.pending[x.StepID] = x
}
func (s *Store) storagePaused() bool {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	return s.health.paused
}

// StorageReady probes at most once per second and resolves only failed workers,
// never intents that an active worker still owns.
func (s *Store) StorageReady(ctx context.Context) bool {
	s.health.probe.Lock()
	defer s.health.probe.Unlock()
	s.health.mu.Lock()
	if !s.health.paused {
		s.health.mu.Unlock()
		return true
	}
	if time.Now().Before(s.health.next) {
		s.health.mu.Unlock()
		return false
	}
	s.health.next = time.Now().Add(time.Second)
	recovering := s.health.recovery
	epoch := s.health.epoch
	pending := make([]domain.Execution, 0, len(s.health.pending))
	for _, x := range s.health.pending {
		pending = append(pending, x)
	}
	s.health.mu.Unlock()
	if err := s.probeStorage(); err != nil {
		s.PauseStorage(err)
		return false
	}
	if recovering {
		if err := s.recoverIntents(ctx); err != nil {
			s.PauseStorage(err)
			return false
		}
		s.health.mu.Lock()
		s.health.recovery = false
		s.health.pending = nil
		s.health.mu.Unlock()
		pending = nil
	}
	for _, x := range pending {
		if ctx.Err() != nil {
			return false
		}
		var owner domain.ProcessIdentity
		owner.StepID = x.StepID
		if err := s.db.QueryRow("SELECT coalesce(pid,0),coalesce(pgid,0),coalesce(boot_id,''),coalesce(process_start,'') FROM steps WHERE id=?", x.StepID).Scan(&owner.PID, &owner.PGID, &owner.BootID, &owner.ProcessStart); err != nil {
			s.PauseStorage(err)
			return false
		}
		stopped, err := StopRecordedProcess(ctx, owner)
		out := domain.Outcome{Kind: "interrupted", ExitCode: -1}
		if !stopped && owner.PID != 0 {
			out.Kind = "process_unknown"
		}
		if err != nil {
			out.Stderr = err.Error()
		}
		next := domain.NextStage(x, out)
		if err := s.Complete(x, out, next); err != nil {
			return false
		}
		s.health.mu.Lock()
		delete(s.health.pending, x.StepID)
		s.health.mu.Unlock()
	}
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	if len(s.health.pending) != 0 || s.health.epoch != epoch {
		return false
	}
	s.health.paused = false
	_ = os.Remove(filepath.Join(s.dir, "storage_paused"))
	return true
}
func (s *Store) probeStorage() error {
	if _, err := s.db.Exec("CREATE TABLE IF NOT EXISTS storage_probe(id INTEGER PRIMARY KEY, checked_at INTEGER NOT NULL)"); err != nil {
		return err
	}
	if _, err := s.db.Exec("INSERT INTO storage_probe VALUES(1,?) ON CONFLICT(id) DO UPDATE SET checked_at=excluded.checked_at", time.Now().UnixNano()); err != nil {
		return err
	}
	dir := filepath.Join(s.dir, "runs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".storage-probe-")
	if err != nil {
		return err
	}
	name := f.Name()
	_, writeErr := f.Write([]byte("storage probe\n"))
	err = errors.Join(writeErr, s.syncLogFile(f), f.Close())
	return errors.Join(err, os.Remove(name))
}

func (s *Store) syncLogFile(f *os.File) error {
	if s.SyncFile != nil {
		return s.SyncFile(f)
	}
	return f.Sync()
}

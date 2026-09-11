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
	lock    *os.File
	context context.Context
	stop    context.CancelFunc
}

func (s *Store) Daemon() (*Daemon, error) {
	lock, e := os.OpenFile(filepath.Join(s.dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = lock.Close()
		return nil, &domain.Fault{Code: 6, Kind: "daemon_running", Message: "a daemon owns this data directory"}
	}
	// An unfinished intent cannot be treated as an unissued command on restart.
	_, e = s.db.Exec(`UPDATE runs SET state='blocked' WHERE id IN(SELECT run_id FROM steps WHERE result IS NULL);
 UPDATE runtime SET stage='blocked:process_unknown' WHERE run_id IN(SELECT run_id FROM steps WHERE result IS NULL);`)
	if e != nil {
		_ = lock.Close()
		return nil, e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
func (s *Store) Executor() *Runner { return &Runner{Dir: s.dir, Started: s.Started} }

func (d *Daemon) Stop() { d.stop() }

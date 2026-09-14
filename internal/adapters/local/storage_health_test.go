package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSQLiteConnectionDurability(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.SetMaxOpenConns(2)
	// Hold both simultaneously so the second check covers a new connection.
	for range 2 {
		conn, e := s.db.Conn(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		defer conn.Close()
		for pragma, want := range map[string]string{"journal_mode": "wal", "synchronous": "2", "foreign_keys": "1", "busy_timeout": "5000"} {
			var got string
			if e = conn.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&got); e != nil || got != want {
				t.Fatalf("%s=%q want %q: %v", pragma, got, want, e)
			}
		}
	}
}

func TestExecutorUsesRealFileSync(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	// Linux cannot fsync a pipe. A no-op sync wiring would incorrectly succeed.
	if err = s.Executor().syncFile(write); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("executor did not call real fsync: %v", err)
	}
}

func TestDaemonObservationUsesInMemoryPause(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.PauseStorage(errors.New("storage unavailable"))
	if err = os.Remove(filepath.Join(s.dir, "storage_paused")); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordDaemonState("running"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = s.db.QueryRow("SELECT state FROM daemon_observation WHERE singleton=1").Scan(&state); err != nil || state != "storage_paused" {
		t.Fatalf("observation used missing marker instead of daemon state: %s %v", state, err)
	}
}

func TestStorageProbeRequiresHeartbeatWrite(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.Exec("CREATE TRIGGER reject_heartbeat BEFORE INSERT ON daemon_observation BEGIN SELECT RAISE(ABORT,'heartbeat disk failure'); END"); err != nil {
		t.Fatal(err)
	}
	s.PauseStorage(errors.New("heartbeat disk failure"))
	s.health.mu.Lock()
	s.health.next = time.Time{}
	s.health.mu.Unlock()
	if s.StorageReady(context.Background()) {
		t.Fatal("generic storage probe reopened admission despite failed heartbeat")
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_heartbeat"); err != nil {
		t.Fatal(err)
	}
	s.health.mu.Lock()
	s.health.next = time.Time{}
	s.health.mu.Unlock()
	if !s.StorageReady(context.Background()) {
		t.Fatal("healthy heartbeat failed to reopen admission")
	}
}

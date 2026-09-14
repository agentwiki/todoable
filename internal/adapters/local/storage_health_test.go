package local

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
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

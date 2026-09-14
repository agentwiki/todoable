package local

import (
	"errors"
	"github.com/agentwiki/todoable/internal/domain"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestDaemonWaitsForClientConfigProbe(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	lock, e := os.OpenFile(filepath.Join(s.dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = lock.Close() }()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); e != nil {
		t.Fatal(e)
	}
	type result struct {
		d *Daemon
		e error
	}
	started := make(chan struct{})
	done := make(chan result, 1)
	go func() { close(started); d, e := s.Daemon(); done <- result{d, e} }()
	<-started
	select {
	case got := <-done:
		if got.d != nil {
			_ = got.d.Close()
		}
		t.Fatalf("client configuration probe must not be treated as a daemon: %v", got.e)
	case <-time.After(30 * time.Millisecond):
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); e != nil {
		t.Fatal(e)
	}
	select {
	case got := <-done:
		if got.e != nil {
			t.Fatal(got.e)
		}
		_ = got.d.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not acquire released client probe")
	}
}

func TestCLIWaitsForDaemonConfigPublication(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	d, e := s.Daemon()
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(s.dir, "daemon.lock")
	published, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	_ = d.Close()
	lock, e := os.OpenFile(path, os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = lock.Close() }()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		t.Fatal(e)
	}
	if e = lock.Truncate(0); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- s.CheckCLIConfig() }()
	select {
	case e := <-done:
		t.Fatalf("incomplete publication must be retried: %v", e)
	case <-time.After(30 * time.Millisecond):
	}
	if _, e = lock.WriteAt(published, 0); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("published daemon configuration was not accepted")
	}
}

func TestLiveDaemonGuardsAreBounded(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	d, e := s.Daemon()
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = d.Close() }()
	t.Run("duplicate-daemon", func(t *testing.T) {
		done := make(chan error, 1)
		go func() {
			other, e := s.Daemon()
			if other != nil {
				_ = other.Close()
			}
			done <- e
		}()
		select {
		case e := <-done:
			var fault *domain.Fault
			if !errors.As(e, &fault) || fault.Code != 6 || fault.Kind != "daemon_running" {
				t.Fatalf("duplicate daemon accepted: %v", e)
			}
		case <-time.After(2 * time.Second):
			_ = d.Close()
			t.Fatal("duplicate daemon guard did not return")
		}
	})
	t.Run("different-config", func(t *testing.T) {
		s.config.MaxRepeat = 0
		done := make(chan error, 1)
		go func() { done <- s.CheckCLIConfig() }()
		select {
		case e := <-done:
			var fault *domain.Fault
			if !errors.As(e, &fault) || fault.Code != 6 || fault.Kind != "config_mismatch" {
				t.Fatalf("mismatched configuration accepted: %v", e)
			}
		case <-time.After(2 * time.Second):
			_ = d.Close()
			t.Fatal("mismatched configuration guard did not return")
		}
	})
}

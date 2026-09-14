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
			if !errors.As(e, &fault) || fault.Code != 5 || fault.Kind != "daemon_running" {
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

func TestCLIRejectsPreviousOwnersHashDuringPublication(t *testing.T) {
	old, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = old.Close() }()
	path := filepath.Join(old.dir, "daemon.lock")
	if e = os.WriteFile(path, []byte(old.ConfigHash()), 0600); e != nil {
		t.Fatal(e)
	}
	fresh, e := Open(old.dir)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = fresh.Close() }()
	fresh.config.MaxRepeat = 0
	gate, e := os.OpenFile(filepath.Join(old.dir, "daemon-publication.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = gate.Close() }()
	if e = syscall.Flock(int(gate.Fd()), syscall.LOCK_EX); e != nil {
		t.Fatal(e)
	}
	lock, e := os.OpenFile(path, os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = lock.Close() }()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- old.CheckCLIConfig() }()
	select {
	case e := <-done:
		t.Fatalf("previous owner's hash admitted during new ownership publication: %v", e)
	case <-time.After(30 * time.Millisecond):
	}
	if _, e = lock.WriteAt([]byte(fresh.ConfigHash()), 0); e != nil {
		t.Fatal(e)
	}
	if e = syscall.Flock(int(gate.Fd()), syscall.LOCK_UN); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		var fault *domain.Fault
		if !errors.As(e, &fault) || fault.Code != 6 || fault.Kind != "config_mismatch" {
			t.Fatalf("previous configuration admitted: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configuration check did not finish after publication")
	}
	if e = fresh.CheckCLIConfig(); e != nil {
		t.Fatalf("new configuration rejected: %v", e)
	}
}

func TestDaemonAcquiresOwnershipOnlyUnderPublicationGate(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	gate, e := os.OpenFile(filepath.Join(s.dir, "daemon-publication.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = gate.Close() }()
	if e = syscall.Flock(int(gate.Fd()), syscall.LOCK_SH); e != nil {
		t.Fatal(e)
	}
	type result struct {
		d *Daemon
		e error
	}
	done := make(chan result, 1)
	go func() { d, e := s.Daemon(); done <- result{d, e} }()
	select {
	case got := <-done:
		if got.d != nil {
			_ = got.d.Close()
		}
		t.Fatalf("daemon changed ownership during CLI publication observation: %v", got.e)
	case <-time.After(30 * time.Millisecond):
	}
	lock, e := os.OpenFile(filepath.Join(s.dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = lock.Close()
		t.Fatalf("ownership acquired before publication gate: %v", e)
	}
	_ = lock.Close()
	if e = syscall.Flock(int(gate.Fd()), syscall.LOCK_UN); e != nil {
		t.Fatal(e)
	}
	select {
	case got := <-done:
		if got.e != nil {
			t.Fatal(got.e)
		}
		defer func() { _ = got.d.Close() }()
		if e = s.CheckCLIConfig(); e != nil {
			t.Fatalf("published configuration rejected: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not start after observation ended")
	}
}

func TestPublicationContentionIsBounded(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	gate, e := os.OpenFile(filepath.Join(s.dir, "daemon-publication.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = gate.Close() }()
	if e = syscall.Flock(int(gate.Fd()), syscall.LOCK_EX); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"cli", "daemon"} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				if name == "cli" {
					done <- s.CheckCLIConfig()
					return
				}
				d, e := s.Daemon()
				if d != nil {
					_ = d.Close()
				}
				done <- e
			}()
			select {
			case e := <-done:
				var fault *domain.Fault
				if !errors.As(e, &fault) || fault.Code != 5 || fault.Kind != "config_busy" {
					t.Fatalf("publication contention must return retryable error: %v", e)
				}
			case <-time.After(2 * time.Second):
				_ = gate.Close()
				t.Fatal("publication contention did not return")
			}
		})
	}
}

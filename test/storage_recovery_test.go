package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/agentwiki/todoable/internal/adapters/local"
	"github.com/agentwiki/todoable/internal/usecases"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func storageDaemon(t *testing.T, f runtimeFixture, env ...string) func(syscall.Signal) {
	t.Helper()
	cmd := exec.Command(f.bin, "--data-dir", f.dir, "daemon")
	cmd.Env = append(append(os.Environ(), "GORACE=atexit_sleep_ms=0"), env...)
	log, err := os.Create(filepath.Join(f.root, "daemon.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func(sig syscall.Signal) {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(sig)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("daemon stop timed out")
		}
		_ = log.Close()
	}
	t.Cleanup(func() { stop(syscall.SIGTERM) })
	return stop
}
func storageSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}
func awaitPaused(t *testing.T, f runtimeFixture, id string) {
	t.Helper()
	waitUntil(t, func() bool { return f.call(t, "run", "show", id, "--json")["storage_paused"] == true })
	raw, err := os.ReadFile(filepath.Join(f.root, "daemon.stderr"))

	if err != nil || !strings.Contains(string(raw), "storage_paused") {
		t.Fatalf("missing stderr diagnostic %s %v", raw, err)
	}
}
func storageSyncDaemon(t *testing.T, f runtimeFixture, flag string) {
	t.Helper()
	log, e := os.Create(filepath.Join(f.root, "daemon.stderr"))
	if e != nil {
		t.Fatal(e)
	}
	originalStderr := os.Stderr
	os.Stderr = log
	t.Cleanup(func() { os.Stderr = originalStderr; _ = log.Close() })
	s, err := local.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := s.Executor()
	s.SyncFile = func(file *os.File) error {
		if _, e := os.Stat(flag); e == nil {
			return syscall.EIO
		}
		return file.Sync()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if s.StorageReady(ctx) {
				_, _ = usecases.Advance(ctx, s, runner)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); <-done; _ = s.Close() })
}
func storageAdmissionFailures(t *testing.T) {
	for _, mode := range []string{"db", "log-create", "log-sync"} {
		t.Run(mode, func(t *testing.T) {
			f := orderFixture(t, 0, "0s", "0s")
			delete(f.task, "start")
			updateOrderTask(t, f)
			input := map[string]any{"label": "first"}
			a := submitOrder(t, f, input, "first")
			db := f.database(t)
			var restore func()
			syncFlag := ""
			switch mode {
			case "db":
				storageSQL(t, db, "CREATE TRIGGER fail_intent BEFORE INSERT ON steps BEGIN SELECT RAISE(ABORT,'disk write failure'); END")
				restore = func() { storageSQL(t, db, "DROP TRIGGER fail_intent") }
			case "log-create":
				writeTest(t, filepath.Join(f.dir, "runs"), nil, 0600)
				restore = func() {
					if err := os.Remove(filepath.Join(f.dir, "runs")); err != nil {
						t.Fatal(err)
					}
				}
			case "log-sync":
				flag := filepath.Join(f.root, "fail-sync")
				syncFlag = flag
				writeTest(t, flag, nil, 0600)

				restore = func() {
					if err := os.Remove(flag); err != nil {
						t.Fatal(err)
					}
				}
			}
			if syncFlag != "" {
				storageSyncDaemon(t, f, syncFlag)
			} else {
				storageDaemon(t, f)
			}
			awaitPaused(t, f, a["run_id"].(string))
			b := submitCore(t, f, "runtime", "second", "other-resource", map[string]any{"label": "second"})
			time.Sleep(1150 * time.Millisecond)
			if events := orderEvents(t, f); len(events) != 0 {
				t.Fatalf("commands during storage failure: %v", events)
			}
			duplicate := submitOrder(t, f, input, "first")
			if duplicate["deduplicated"] != true || duplicate["run_id"] != a["run_id"] {
				t.Fatalf("lost duplicate %v", duplicate)
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM submissions").Scan(&count); err != nil || count != 2 {
				t.Fatalf("lost input %d %v", count, err)
			}
			restore()
			got := f.await(t, b["run_id"].(string))
			if got["state"] != "succeeded" {
				t.Fatalf("storage did not recover %v", got)
			}
			if mode != "db" {
				got = f.await(t, a["run_id"].(string))
				if got["stage"] != "blocked:outcome_unknown" {
					t.Fatalf("unrecorded changing intent inferred success %v", got)
				}
			}
		})
	}
}
func storageLostResults(t *testing.T) {
	for _, mode := range []string{"db-result", "log-sync-result"} {
		t.Run(mode, func(t *testing.T) {
			f := orderFixture(t, 0, "0s", "0s")
			gate := filepath.Join(f.root, "release")
			a := submitOrder(t, f, map[string]any{"label": "first", "before_gate": gate}, "first")
			var restore func()
			syncFlag := ""
			db := f.database(t)
			if mode == "log-sync-result" {
				flag := filepath.Join(f.root, "fail-sync")
				syncFlag = flag

				restore = func() {
					if err := os.Remove(flag); err != nil {
						t.Fatal(err)
					}
				}
			}
			if syncFlag != "" {
				storageSyncDaemon(t, f, syncFlag)
			} else {
				storageDaemon(t, f)
			}
			waitExternal(t, f, "first", "before", 1)
			if mode == "db-result" {
				storageSQL(t, db, "CREATE TRIGGER fail_result BEFORE UPDATE OF result ON steps WHEN NEW.stage='before' BEGIN SELECT RAISE(ABORT,'result disk failure'); END")
				restore = func() { storageSQL(t, db, "DROP TRIGGER fail_result") }
			} else {
				writeTest(t, filepath.Join(f.root, "fail-sync"), nil, 0600)
			}
			writeTest(t, gate, nil, 0600)
			awaitPaused(t, f, a["run_id"].(string))
			before := len(orderEvents(t, f))
			var firstProbe int64
			waitUntil(t, func() bool {
				return db.QueryRow("SELECT checked_at FROM storage_probe WHERE id=1").Scan(&firstProbe) == nil
			})
			var secondProbe int64
			waitUntil(t, func() bool {
				return db.QueryRow("SELECT checked_at FROM storage_probe WHERE id=1").Scan(&secondProbe) == nil && secondProbe != firstProbe
			})
			if delta := time.Duration(secondProbe - firstProbe); delta < 900*time.Millisecond || delta > 3*time.Second {
				t.Fatalf("storage probe interval %v", delta)
			}
			if after := len(orderEvents(t, f)); after != before {
				t.Fatalf("external execution while paused %d -> %d", before, after)
			}
			var missing int
			if err := db.QueryRow("SELECT count(*) FROM steps WHERE stage='before' AND result IS NULL").Scan(&missing); err != nil || missing != 1 {
				t.Fatalf("unsaved result falsely saved %d %v", missing, err)
			}
			restore()
			got := f.await(t, a["run_id"].(string))
			if got["stage"] != "blocked:outcome_unknown" || got["cancel_requested"] != false {
				t.Fatalf("lost result not blocked %v", got)
			}
			waitUntil(t, func() bool {
				return f.call(t, "run", "show", a["run_id"].(string), "--json")["storage_paused"] == false
			})
		})
	}
}

func signalAndBackup(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			f := orderFixture(t, 0, "0s", "0s")
			a := submitOrder(t, f, map[string]any{"label": "done"}, "done")
			stop := storageDaemon(t, f)
			if got := f.await(t, a["run_id"].(string)); got["state"] != "succeeded" {
				t.Fatal(got)
			}
			audit := submitOrder(t, f, map[string]any{"label": "audit", "block_before_run": 1}, "audit")
			av := f.await(t, audit["run_id"].(string))
			steps := av["steps"].([]any)
			step := steps[len(steps)-1].(map[string]any)["step_id"].(string)
			f.call(t, resumeArgs(audit["run_id"].(string), step, "confirm-success")...)
			if got := f.await(t, audit["run_id"].(string)); got["state"] != "succeeded" {
				t.Fatalf("audit recovery %v", got)
			}
			gate := filepath.Join(f.root, "release")
			b := submitOrder(t, f, map[string]any{"label": "interrupted", "before_gate": gate}, "interrupted")
			waitExternal(t, f, "interrupted", "before", 1)
			c := submitOrder(t, f, map[string]any{"label": "queued"}, "queued")
			stop(sig)
			observed := f.call(t, "run", "show", b["run_id"].(string), "--json")
			if observed["stage"] != "blocked:outcome_unknown" || observed["cancel_requested"] != false {
				t.Fatalf("signal outcome %v", observed)
			}
			events := orderEvents(t, f)
			for _, e := range events {
				if e.Label == "queued" && e.Stage != "start_check" {
					t.Fatalf("queued execution after shutdown %v", e)
				}
			}
			// VACUUM INTO is SQLite's consistent online backup operation. It includes
			// every table, independent of WAL checkpoint state or our table expectations.
			backupDir := t.TempDir()
			db := f.database(t)
			target := filepath.Join(backupDir, "todoable.db")
			if _, err := db.Exec("VACUUM INTO ?", target); err != nil {
				t.Fatal(err)
			}
			restored := f
			restored.dir = backupDir
			for _, submission := range []map[string]any{a, audit, b, c} {
				id := submission["run_id"].(string)
				original := f.call(t, "run", "show", id, "--json")
				copy := restored.call(t, "run", "show", id, "--json")
				delete(original, "time_remaining")
				delete(copy, "time_remaining")
				if !reflect.DeepEqual(original, copy) {
					t.Fatalf("backup run mismatch %v %v", original, copy)
				}
			}
			assertDatabaseBackup(t, db, restored.database(t))
			storageDaemon(t, f)
			time.Sleep(1200 * time.Millisecond)
			if got := f.call(t, "run", "show", b["run_id"].(string), "--json"); got["stage"] != "blocked:outcome_unknown" {
				t.Fatalf("restart cleared block %v", got)
			}
			for _, event := range orderEvents(t, f)[len(events):] {
				if event.Stage != "start_check" {
					t.Fatalf("restart replayed blocked resource %v", event)
				}
			}
		})
	}
}
func assertDatabaseBackup(t *testing.T, a, b *sql.DB) {
	t.Helper()
	rows, err := a.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	dump := func(db *sql.DB, table string) []string {
		r, e := db.Query(`SELECT * FROM "` + table + `" ORDER BY rowid`)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Close()
		cols, _ := r.Columns()
		var out []string
		for r.Next() {
			vals := make([]any, len(cols))
			ptr := make([]any, len(cols))
			for i := range vals {
				ptr[i] = &vals[i]
			}
			if e = r.Scan(ptr...); e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(vals)
			out = append(out, string(raw))
		}
		if e = r.Err(); e != nil {
			t.Fatal(e)
		}
		return out
	}
	for _, table := range tables {
		if !reflect.DeepEqual(dump(a, table), dump(b, table)) {
			t.Fatalf("backup table differs: %s", table)
		}
	}
}

func storageTransactionBoundaries(t *testing.T) {
	f := orderFixture(t, 0, "0s", "0s")
	gate := filepath.Join(f.root, "release")
	a := submitOrder(t, f, map[string]any{"label": "waiting", "before_gate": gate}, "waiting")
	stop := storageDaemon(t, f)
	waitExternal(t, f, "waiting", "before", 1)
	db := f.database(t)
	start := time.Now()
	storageSQL(t, db, "CREATE TABLE external_writer(value TEXT)")
	storageSQL(t, db, "INSERT INTO external_writer VALUES('command still running')")
	if time.Since(start) > time.Second {
		t.Fatal("external command held SQLite write transaction")
	}
	// A refusal in the middle of admission must roll back the submission and
	// its repeat budget rather than leave partial deduplication state.
	storageSQL(t, db, "CREATE TRIGGER reject_run BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT,'atomic refusal'); END")
	raw := []byte(`{"task_id":"runtime","input_key":"rejected","input":{"label":"rejected"},"concurrency_key":"rejected"}`)
	path := filepath.Join(f.root, "rejected.json")
	writeTest(t, path, raw, 0600)
	cmd := exec.Command(f.bin, "--data-dir", f.dir, "run", "submit", path)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "atomic refusal") {
		t.Fatalf("refusal not surfaced %s %v", out, err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM submissions WHERE input_key='rejected'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial submission %d %v", n, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM repeat_budgets").Scan(&n); err != nil || n != 1 {
		t.Fatalf("partial repeat budget %d %v", n, err)
	}
	storageSQL(t, db, "DROP TRIGGER reject_run")
	stop(syscall.SIGTERM)
	// A plausible completion log has no authority over a missing/unknown result.
	writeTest(t, filepath.Join(f.dir, "runs", a["run_id"].(string), "success.log"), []byte("success exit 0\n"), 0600)
	storageDaemon(t, f)
	got := f.call(t, "run", "show", a["run_id"].(string), "--json")
	if got["stage"] != "blocked:outcome_unknown" {
		t.Fatalf("log used as completion: %v", got)
	}
}

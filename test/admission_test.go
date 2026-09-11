package test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func (f intakeFixture) database(t *testing.T) *sql.DB {
	t.Helper()
	db, e := sql.Open("sqlite3", "file:"+f.dir+"/todoable.db?_busy_timeout=5000")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func (f intakeFixture) submitRaw(t *testing.T, raw string) map[string]any {
	t.Helper()
	writeTest(t, f.input, []byte(raw), 0600)
	return f.call(t, "run", "submit", f.input)
}
func inputJSON(input string) string {
	return fmt.Sprintf(`{"task_id":"concurrent","input_key":"item","input":%s,"concurrency_key":"resource"}`, input)
}
func (f intakeFixture) rejected(t *testing.T, code int, kind string, args ...string) {
	t.Helper()
	cmd := exec.Command(f.bin, append([]string{"--data-dir", f.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	e := cmd.Run()
	exit, ok := e.(*exec.ExitError)
	if !ok || exit.ExitCode() != code || out.Len() != 0 {
		t.Fatalf("rejection code %d: %v out=%s err=%s", code, e, &out, &stderr)
	}
	var v map[string]any
	if e = json.Unmarshal(stderr.Bytes(), &v); e != nil || v["protocol_version"] != float64(1) || (kind != "" && v["error"] != kind) {
		t.Fatalf("error response %s %v", &stderr, e)
	}
}
func sameSubmission(t *testing.T, a, b map[string]any) {
	t.Helper()
	if a["submission_id"] != b["submission_id"] || a["run_id"] != b["run_id"] || b["deduplicated"] != true || a["task_version"] != b["task_version"] {
		t.Fatalf("duplicate mismatch %v %v", a, b)
	}
}
func (f intakeFixture) noExecution(t *testing.T) {
	t.Helper()
	for _, p := range []string{f.log, f.artifact} {
		if _, e := os.Stat(p); !os.IsNotExist(e) {
			t.Fatalf("external execution %s: %v", p, e)
		}
	}
}
func (f intakeFixture) counts(t *testing.T, n int) {
	t.Helper()
	db := f.database(t)
	for _, table := range []string{"submissions", "runs", "repeat_budgets"} {
		var got int
		if e := db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); e != nil || got != n {
			t.Fatalf("%s count %d want %d: %v", table, got, n, e)
		}
	}
	f.noExecution(t)
}
func crashAfterCommit(t *testing.T, f intakeFixture) map[string]any {
	t.Helper()
	r, w, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	// Fill the response pipe before passing it to the CLI. Its JSON write blocks,
	// while the independently observed DB commit identifies the exact kill boundary.
	fd := int(w.Fd())
	if e = syscall.SetNonblock(fd, true); e != nil {
		t.Fatal(e)
	}
	fill := make([]byte, 4096)
	for {
		_, e = syscall.Write(fd, fill)
		if e == syscall.EAGAIN {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	if e = syscall.SetNonblock(fd, false); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(f.bin, "--data-dir", f.dir, "run", "submit", f.input)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill() }()
	db := f.database(t)
	var sid, rid string
	deadline := time.Now().Add(10 * time.Second)
	for {
		e = db.QueryRow("SELECT s.id,r.id FROM submissions s JOIN runs r ON r.submission_id=s.id").Scan(&sid, &rid)
		if e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("commit never observed: %v", e)
		}
		time.Sleep(time.Millisecond)
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e = cmd.Wait(); e == nil {
		t.Fatal("CLI was not killed")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr %s", &stderr)
	}
	return map[string]any{"submission_id": sid, "run_id": rid, "task_version": float64(1)}
}
func updateTask(t *testing.T, f intakeFixture) {
	t.Helper()
	p := filepath.Join(filepath.Dir(f.input), "task.json")
	raw, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	var task map[string]any
	if e = json.Unmarshal(raw, &task); e != nil {
		t.Fatal(e)
	}
	task["prompt"] = "version two"
	task["repeat"] = 7
	raw, e = json.Marshal(task)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, p, raw, 0600)
	got := f.call(t, "task", "update", p, "--if-version", "1")
	if got["task_version"] != float64(2) {
		t.Fatalf("update %v", got)
	}
	f.rejected(t, 3, "version_conflict", "task", "update", p, "--if-version", "1")
	got = f.call(t, "task", "update", p, "--if-version", "2")
	if got["task_version"] != float64(2) {
		t.Fatalf("identical update %v", got)
	}
}
func hashForCollision() string {
	sum := sha256.Sum256([]byte(`["concurrent","item",{"collision":true}]`))
	return hex.EncodeToString(sum[:])
}

// durableAdmission records the immutable input and budget state independently
// of the CLI response so rejected requests cannot mutate an existing row.
func (f intakeFixture) durableAdmission(t *testing.T) string {
	t.Helper()
	rows, e := f.database(t).Query("SELECT s.id,s.input,s.snapshot,s.hash,b.total,b.remaining,r.id,r.calls_used FROM submissions s JOIN repeat_budgets b ON b.submission_id=s.id JOIN runs r ON r.submission_id=s.id ORDER BY s.seq,r.run_seq")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = rows.Close() }()
	var out bytes.Buffer
	for rows.Next() {
		var sid, input, snap, hash, rid string
		var total, remaining, calls int
		if e = rows.Scan(&sid, &input, &snap, &hash, &total, &remaining, &rid, &calls); e != nil {
			t.Fatal(e)
		}
		fmt.Fprintf(&out, "%q %q %q %q %d %d %q %d\n", sid, input, snap, hash, total, remaining, rid, calls)
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	return out.String()
}

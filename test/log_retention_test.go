package test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScenario_SC_27(t *testing.T) {
	verify(t, "V-01", func(t *testing.T) { logRetentionFlow(t, false) })
	verify(t, "V-02", func(t *testing.T) { logRetentionFlow(t, true) })
}
func logRetentionFlow(t *testing.T, age bool) {
	f := orderFixture(t, 0, "0s", "0s")
	delete(f.task, "start")
	updateOrderTask(t, f)
	script := strings.Replace(orderScript, "gates=inp.get(s+'_gates',{})", "sys.stdout.write('O'*64);sys.stdout.flush();sys.stderr.write('E'*64);sys.stderr.flush()\ngates=inp.get(s+'_gates',{})", 1)
	writeTest(t, filepath.Join(f.root, "runner.py"), []byte(script), 0700)
	config := "completed_log_bytes: 640\n"
	if age {
		config = "completed_log_bytes: 1000000\ncompleted_log_retention: 4s\n"
	}
	writeTest(t, filepath.Join(f.dir, "config.yaml"), []byte(config), 0600)
	old := submitCore(t, f, "runtime", "old", "old", map[string]any{"label": "old", "block_before_run": 1})
	stop := configDaemon(t, f)
	oldID := old["run_id"].(string)
	blockedOld := f.await(t, oldID)
	if blockedOld["stage"] != "blocked:outcome_unknown" {
		t.Fatalf("audit fixture did not block: %v", blockedOld)
	}
	oldStep := currentStep(t, blockedOld)
	f.call(t, resumeArgs(oldID, oldStep, "confirm-success")...)
	oldView := completedRuns(t, f, old, 1)[0]
	audits := oldView["resolutions"].([]any)
	if len(audits) != 1 {
		t.Fatalf("missing actual manual audit: %v", oldView)
	}
	audit := audits[0].(map[string]any)
	if audit["step_id"] != oldStep || audit["action"] != "confirm-success" || audit["reason"] != "verified destination and stopped worker" || audit["manual"] != true {
		t.Fatalf("unexpected actual manual audit: %v", audit)
	}
	oldLogs := retentionLogs(t, f, oldView)
	assertLogBytes(t, oldLogs, 64)
	if len(oldLogs) != 10 {
		t.Fatalf("completed log count %d", len(oldLogs))
	}
	oldState, _ := json.Marshal(oldView)
	gate := filepath.Join(f.root, "progress-release")
	progress := submitCore(t, f, "runtime", "progress", "progress", map[string]any{"label": "progress", "before_gate": gate})
	waitExternal(t, f, "progress", "before", 1)
	blocked := submitCore(t, f, "runtime", "blocked", "blocked", map[string]any{"label": "blocked", "block_before_run": 1})
	blockedView := f.await(t, blocked["run_id"].(string))
	if blockedView["stage"] != "blocked:outcome_unknown" {
		t.Fatalf("blocked fixture: %v", blockedView)
	}
	progressView := f.call(t, "run", "show", progress["run_id"].(string), "--json")
	if progressView["state"] != "running" {
		t.Fatal(progressView)
	}
	protected := retentionLogs(t, f, progressView)
	for path, b := range retentionLogs(t, f, blockedView) {
		protected[path] = b
	}
	assertLogBytes(t, protected, 64)
	if len(protected) != 4 {
		t.Fatalf("protected log count %d", len(protected))
	}
	// At least one maintenance interval must preserve the exact byte boundary;
	// the separate age case must also retain a still-young completed receipt.
	time.Sleep(1200 * time.Millisecond)
	assertLogsEqual(t, oldLogs)
	assertLogsEqual(t, protected)
	if age {
		time.Sleep(1300 * time.Millisecond)
	}
	newest := submitCore(t, f, "runtime", "new", "new", map[string]any{"label": "new"})
	newestView := completedRuns(t, f, newest, 1)[0]
	newestLogs := retentionLogs(t, f, newestView)
	assertLogBytes(t, newestLogs, 64)
	before := retentionRecords(t, f)
	definitions := parsingDefinitions(t, f.intakeFixture)
	events, e := os.ReadFile(filepath.Join(f.records, "events"))
	if e != nil {
		t.Fatal(e)
	}
	waitUntil(t, func() bool {
		for path := range oldLogs {
			if _, e := os.Stat(path); !os.IsNotExist(e) {
				return false
			}
		}
		return true
	})
	assertLogsEqual(t, newestLogs)
	assertLogsEqual(t, protected)
	if after := retentionRecords(t, f); after != before {
		t.Fatalf("cleanup changed durable records:\nbefore %s\nafter %s", before, after)
	}
	if parsingDefinitions(t, f.intakeFixture) != definitions {
		t.Fatal("cleanup changed Task versions")
	}
	afterState, _ := json.Marshal(f.call(t, "run", "show", old["run_id"].(string), "--json"))
	if string(afterState) != string(oldState) {
		t.Fatal("cleanup changed completed Run results or summary")
	}
	for _, item := range []map[string]any{old, newest} {
		label := "old"
		if item["run_id"] == newest["run_id"] {
			label = "new"
		}
		input := map[string]any{"label": label}
		if label == "old" {
			input["block_before_run"] = 1
		}
		replay := submitCore(t, f, "runtime", label, label, input)
		sameSubmission(t, item, replay)
		for name, want := range map[string]string{"artifact": label + "-1", "count": "1", "published": label + "-1\n"} {
			b, e := os.ReadFile(filepath.Join(f.dir, "runs", item["run_id"].(string), name))
			if e != nil || string(b) != want {
				t.Fatalf("preserved external artifact %s: %q %v", name, b, e)
			}
		}
	}
	if after := retentionRecords(t, f); after != before {
		t.Fatal("retransmission after cleanup mutated history")
	}
	afterEvents, e := os.ReadFile(filepath.Join(f.records, "events"))
	if e != nil || string(afterEvents) != string(events) {
		t.Fatalf("retransmission re-executed external commands: %v", e)
	}
	for _, item := range []map[string]any{progress, blocked} {
		for _, name := range []string{"artifact", "published"} {
			if _, e := os.Stat(filepath.Join(f.dir, "runs", item["run_id"].(string), name)); !os.IsNotExist(e) {
				t.Fatalf("unexpected protected Run effect: %s %v", name, e)
			}
		}
	}
	assertLogsEqual(t, protected)
	// Let the real progress process finish before daemon cleanup; the blocked
	// receipt stays blocked and its result evidence remains available.
	writeTest(t, gate, nil, 0600)
	completedRuns(t, f, progress, 1)
	stop()
}
func retentionLogs(t *testing.T, f runtimeFixture, view map[string]any) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for _, raw := range view["steps"].([]any) {
		step := raw.(map[string]any)
		for _, name := range []string{"stdout", "stderr"} {
			path := filepath.Join(f.dir, "runs", view["run_id"].(string), "steps", step["step_id"].(string), name)
			waitUntil(t, func() bool { b, e := os.ReadFile(path); return e == nil && len(b) == 64 })
			b, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			result[path] = b
		}
	}
	return result
}
func assertLogBytes(t *testing.T, logs map[string][]byte, want int) {
	t.Helper()
	for path, b := range logs {
		expected := strings.Repeat("O", want)
		if filepath.Base(path) == "stderr" {
			expected = strings.Repeat("E", want)
		}
		if string(b) != expected {
			t.Fatalf("log bytes %s: %q", path, b)
		}
	}
}
func assertLogsEqual(t *testing.T, logs map[string][]byte) {
	t.Helper()
	for path, want := range logs {
		b, e := os.ReadFile(path)
		if e != nil || !reflect.DeepEqual(b, want) {
			t.Fatalf("preserved log %s: %q %v", path, b, e)
		}
	}
}
func retentionRecords(t *testing.T, f runtimeFixture) string {
	t.Helper()
	var result strings.Builder
	result.WriteString(f.durableAdmission(t))
	rows, e := f.database(t).Query("SELECT id,run_id,stage,call_index,owner_version,result,pid,pgid,boot_id,process_start FROM steps ORDER BY seq")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, run, stage string
		var call, owner int
		var outcome, boot, start sql.NullString
		var pid, pgid sql.NullInt64
		if e = rows.Scan(&id, &run, &stage, &call, &owner, &outcome, &pid, &pgid, &boot, &start); e != nil {
			t.Fatal(e)
		}
		fmt.Fprintf(&result, "%q %q %q %d %d %+v %+v %+v %+v %+v\n", id, run, stage, call, owner, outcome, pid, pgid, boot, start)
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	auditRows, e := f.database(t).Query("SELECT * FROM step_resolutions ORDER BY rowid")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = auditRows.Close() }()
	columns, e := auditRows.Columns()
	if e != nil {
		t.Fatal(e)
	}
	for auditRows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if e = auditRows.Scan(pointers...); e != nil {
			t.Fatal(e)
		}
		raw, e := json.Marshal(values)
		if e != nil {
			t.Fatal(e)
		}
		result.Write(raw)
	}
	if e = auditRows.Err(); e != nil {
		t.Fatal(e)
	}
	return result.String()
}

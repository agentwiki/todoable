package test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScenario_SC_33(t *testing.T) {
	f := newIntake(t)
	config := filepath.Join(f.dir, "config.yaml")
	path := filepath.Join(f.dir, "parse.yaml")
	base := fmt.Sprintf("version: 1\nid: parsed\nworkdir: %s\nagent: &cmd [/bin/true]\nprompt: work\nfinish:\n  check: *cmd\nstart:\n  check: *cmd\n", filepath.Dir(f.dir))
	f.call(t, "run", "submit", f.input)
	before := f.durableAdmission(t)
	verify(t, "V-01", func(t *testing.T) {
		writeTest(t, path, []byte(base), 0600)
		f.call(t, "task", "register", path)
		db, e := sql.Open("sqlite3", "file:"+f.dir+"/todoable.db?mode=ro")
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = db.Close() }()
		var raw string
		if e = db.QueryRow("SELECT definition FROM tasks WHERE id='parsed'").Scan(&raw); e != nil {
			t.Fatal(e)
		}
		var task map[string]any
		if e = json.Unmarshal([]byte(raw), &task); e != nil {
			t.Fatal(e)
		}
		if task["repeat"] != float64(0) || task["repeat_delay"] != "0s" || task["finish"].(map[string]any)["max_calls"] != float64(3) || task["start"].(map[string]any)["poll_every"] != "60s" || task["start"].(map[string]any)["wait_timeout"] != "0s" {
			t.Fatalf("defaults: %s", raw)
		}
		for k, v := range map[string]string{"start_check_timeout": "30s", "finish_check_timeout": "30s", "before_timeout": "5m", "agent_timeout": "30m", "after_timeout": "5m", "run_timeout": "2h"} {
			if task["limits"].(map[string]any)[k] != v {
				t.Fatalf("default %s: %s", k, raw)
			}
		}
		for _, extra := range []string{"unknown: true\n", "repeat: forever\n", "repeat: -1\n", "repeat: 1001\n", "repeat_delay: 24h1ns\n", "repeat_delay: -1s\n", "repeat_delay: 1d\n", "repeat_delay: +1s\n", "prompt: duplicate\n", "env: {x: !user hi}\n", "env: {<<: {x: hi}}\n", "before: &cycle [*cycle]\n", "limits: {agent_timeout: 4h1ns}\n", "limits: {unknown: 1s}\n"} {
			writeTest(t, path, []byte(base+extra), 0600)
			parsingRejected(t, f, "task", "register", path)
		}
		aliasKey := strings.Replace(base, "id: parsed", "id: alias-key", 1) + "env: {NAME: &key SECOND, *key: value}\n"
		writeTest(t, path, []byte(aliasKey), 0600)
		f.call(t, "task", "register", path)
		// Alias expansion is bounded even when its source is small.
		expanded := strings.Replace(base, "prompt: work", "prompt: &text "+strings.Repeat("x", 400), 1) + "env:\n  one: *text\n  two: *text\n"
		writeTest(t, config, []byte("max_manifest_bytes: 1024\n"), 0600)
		writeTest(t, path, []byte(expanded), 0600)
		parsingRejected(t, f, "task", "register", path)

		// The manifest JSON representation has an independent exact byte boundary.
		manifest := fmt.Sprintf(`{"version":1,"id":"byte-boundary","workdir":%q,"agent":["/bin/true"],"prompt":"ok","finish":{"check":["/bin/true"]}}`, filepath.Dir(f.dir))
		writeTest(t, config, []byte(fmt.Sprintf("max_manifest_bytes: %d", len(manifest))), 0600)
		writeTest(t, path, []byte(manifest), 0600)
		f.call(t, "task", "register", path)
		writeTest(t, path, []byte(manifest+" "), 0600)
		parsingRejected(t, f, "task", "register", path)
		writeTest(t, config, []byte("{}"), 0600)
		for _, field := range []string{"max_running_runs", "max_check_processes", "max_pending_submissions", "max_pending_per_input_key", "max_input_bytes", "max_manifest_bytes", "max_calls_per_run", "step_log_bytes", "completed_log_bytes"} {
			writeTest(t, config, []byte(field+": 0\n"), 0600)
			parsingRejected(t, f, "task", "register", path)
		}
		for _, bad := range []string{"version: 2", "unknown: 1", "max_repeat: -1", "max_pending_submissions: 99", "completed_log_retention: 0s", "caps: {unknown: 1s}", "caps: {agent_timeout: 0s}", "max_running_runs: 2\nmax_running_runs: 3", strings.Repeat(" ", 1048577)} {
			writeTest(t, config, []byte(bad), 0600)
			parsingRejected(t, f, "task", "register", path)
		}
		for _, cap := range []string{"repeat_delay", "start_poll_every", "start_wait_timeout", "start_check_timeout", "finish_check_timeout", "before_timeout", "agent_timeout", "after_timeout", "run_timeout"} {
			writeTest(t, config, []byte("caps: {"+cap+": -1s}"), 0600)
			parsingRejected(t, f, "task", "register", path)
		}

		// Every Task cap accepts its boundary and rejects one nanosecond more.
		for field, cap := range map[string]string{"start_check_timeout": "5m", "finish_check_timeout": "5m", "before_timeout": "1h", "agent_timeout": "4h", "after_timeout": "1h", "run_timeout": "24h"} {
			writeTest(t, config, []byte("{}"), 0600)
			valid := strings.Replace(base, "id: parsed", "id: boundary-"+field, 1) + "limits: {" + field + ": " + cap + "}\n"
			writeTest(t, path, []byte(valid), 0600)
			f.call(t, "task", "register", path)
			writeTest(t, path, []byte(strings.Replace(valid, cap+"}", cap+"1ns}", 1)), 0600)
			parsingRejected(t, f, "task", "register", path)
			writeTest(t, config, []byte("caps: {"+field+": 1ns}"), 0600)
			writeTest(t, path, []byte(base), 0600)
			parsingRejected(t, f, "task", "register", path)
		}
		for _, cfg := range []string{"max_repeat: 0", "max_calls_per_run: 2"} {
			writeTest(t, config, []byte(cfg), 0600)
			writeTest(t, path, []byte(base+"repeat: 1\n"), 0600)
			parsingRejected(t, f, "task", "register", path)
		}
		writeTest(t, config, []byte("max_input_bytes: 10"), 0600)
		// JCS expands this legal source from ten bytes to fourteen bytes.
		writeTest(t, f.input, []byte(inputJSON(`{"x":1e-6}`)), 0600)
		parsingRejected(t, f, "run", "submit", f.input)
		writeTest(t, config, []byte("max_input_bytes: 64\n"), 0600)
		for _, input := range []string{`{"x":"` + strings.Repeat("a", 57) + `"}`, `{"x":` + strings.Repeat(" ", 60) + `1}`, `{"x":` + strings.Repeat("[", 64) + `0` + strings.Repeat("]", 64) + `}`} {
			writeTest(t, f.input, []byte(inputJSON(input)), 0600)
			parsingRejected(t, f, "run", "submit", f.input)
		}
		// Exact original and canonical input boundary is accepted.
		writeTest(t, f.input, []byte(inputJSON(`{"x":"`+strings.Repeat("a", 56)+`"}`)), 0600)
		f.call(t, "run", "submit", f.input)
		writeTest(t, config, []byte("{}"), 0600)
		input := `{"x":` + strings.Repeat("[", 63) + `0` + strings.Repeat("]", 63) + `}`
		writeTest(t, f.input, []byte(inputJSON(input)), 0600)
		f.call(t, "run", "submit", f.input)
		writeTest(t, f.input, []byte(inputJSON(`{"x":`+strings.Repeat("[", 64)+`0`+strings.Repeat("]", 64)+`}`)), 0600)
		parsingRejected(t, f, "run", "submit", f.input)
	})
	verify(t, "V-02", func(t *testing.T) {
		// Compare the original submission and its budgets after every rejection and successful unrelated intake.
		if after := f.durableAdmission(t); !strings.Contains(after, before) {
			t.Fatalf("durable snapshot before=%s after=%s", before, after)
		}
		writeTest(t, f.input, []byte(`{"task_id":"concurrent","input_key":"item","input":{"b":[true,"1"],"a":1},"concurrency_key":"resource"}`), 0600)
		result := f.call(t, "run", "submit", f.input)
		if result["deduplicated"] != true {
			t.Fatal(result)
		}
		f.noExecution(t)
		// User keys are stored verbatim, never used as filesystem paths.
		writeTest(t, f.input, []byte(`{"task_id":"concurrent","input_key":"../../outside","input":{},"concurrency_key":"../../lock"}`), 0600)
		f.call(t, "run", "submit", f.input)
		for _, name := range []string{"outside", "lock"} {
			if _, e := os.Stat(filepath.Join(filepath.Dir(f.dir), name)); !os.IsNotExist(e) {
				t.Fatalf("key became path: %s %v", name, e)
			}
		}
	})
}

func parsingRejected(t *testing.T, f intakeFixture, args ...string) {
	t.Helper()
	before := f.durableAdmission(t)
	definitions := parsingDefinitions(t, f)
	f.rejected(t, 2, "validation_error", args...)
	if after := f.durableAdmission(t); after != before {
		t.Fatalf("rejection mutated admission: %s -> %s", before, after)
	}
	if after := parsingDefinitions(t, f); after != definitions {
		t.Fatalf("rejection mutated definitions: %s -> %s", definitions, after)
	}
}
func parsingDefinitions(t *testing.T, f intakeFixture) string {
	t.Helper()
	rows, e := f.database(t).Query("SELECT task_id,version,definition FROM task_versions ORDER BY task_id,version")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = rows.Close() }()
	var out strings.Builder
	for rows.Next() {
		var id, definition string
		var version int
		if e = rows.Scan(&id, &version, &definition); e != nil {
			t.Fatal(e)
		}
		fmt.Fprintf(&out, "%q %d %q\n", id, version, definition)
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	return out.String()
}

package test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func scenarioPastInputV1(t *testing.T) { scenarioPastInput(t, false) }
func scenarioPastInputV2(t *testing.T) { scenarioPastInput(t, true) }

func scenarioPastInput(t *testing.T, conflict bool) {
	t.Helper()
	f := newRuntime(t)
	a := f.submit(t, map[string]any{"value": "A", "success_at": 1})
	f.daemon(t)
	av := f.await(t, a["run_id"].(string))
	assertRuntime(t, f, a, av, "succeeded", 1, append(runtimeStages(1), "after"))
	b := f.submit(t, map[string]any{"value": "B", "success_at": 2})
	bv := f.await(t, b["run_id"].(string))
	assertRuntime(t, f, b, bv, "succeeded", 2, append(runtimeStages(2), "after"))
	if a["submission_id"] == b["submission_id"] {
		t.Fatal("different inputs coalesced")
	}
	before := f.durableAdmission(t)
	recordsA, recordsB := f.recordsFor(t, a["run_id"].(string)), f.recordsFor(t, b["run_id"].(string))
	if conflict {
		writeTest(t, f.input, []byte(`{"task_id":"runtime","input_key":"item","input":{"value":"A","success_at":1},"concurrency_key":"changed"}`), 0600)
		f.rejected(t, 3, "metadata_conflict", "run", "submit", f.input)
		if f.durableAdmission(t) != before {
			t.Fatal("metadata conflict changed history or budgets")
		}
	}
	duplicate := f.submit(t, map[string]any{"value": "A", "success_at": 1})
	sameSubmission(t, a, duplicate)
	if duplicate["state"] != "succeeded" {
		t.Fatalf("past duplicate state: %v", duplicate)
	}
	// Leave the daemon running so an incorrectly appended admission has an opportunity to execute.
	time.Sleep(150 * time.Millisecond)
	if f.durableAdmission(t) != before {
		t.Fatal("past A recreated or replenished its budgets")
	}
	if !reflect.DeepEqual(f.recordsFor(t, a["run_id"].(string)), recordsA) || !reflect.DeepEqual(f.recordsFor(t, b["run_id"].(string)), recordsB) {
		t.Fatal("duplicate executed original runs again")
	}
	paths, e := filepath.Glob(filepath.Join(f.records, "*.json"))
	if e != nil || len(paths) != len(recordsA)+len(recordsB) {
		t.Fatalf("unexpected external execution: %d %v", len(paths), e)
	}
	var submissions, runs, budgets, calls int
	e = f.database(t).QueryRow(`SELECT (SELECT count(*) FROM submissions),(SELECT count(*) FROM runs),(SELECT count(*) FROM repeat_budgets),(SELECT sum(calls_used) FROM runs)`).Scan(&submissions, &runs, &budgets, &calls)
	if e != nil || submissions != 2 || runs != 2 || budgets != 2 || calls != 3 {
		t.Fatalf("history counts %d %d %d calls=%d: %v", submissions, runs, budgets, calls, e)
	}
	if got := f.call(t, "run", "show", a["run_id"].(string), "--json"); !reflect.DeepEqual(got, av) {
		t.Fatalf("past A result mutated: %v", got)
	}
}

func scenarioVersionRaceV1(t *testing.T) { scenarioVersionRace(t) }
func scenarioVersionRaceV2(t *testing.T) { scenarioVersionRace(t) }

func scenarioVersionRace(t *testing.T) {
	t.Helper()
	f := newRuntime(t)
	// The external command observes the held resource and live run budget, independently of the CLI view.
	script := strings.Replace(runtimeScript, "db.close()", "held=db.execute('SELECT key FROM resources WHERE run_id=?',(run,)).fetchone()\nbudget=db.execute('SELECT remaining_ns FROM runtime WHERE run_id=?',(run,)).fetchone()[0]\ndb.close()", 1)
	script = strings.Replace(script, "record={'intent':intent,", "record={'held_resource':held,'remaining_ns':budget,'intent':intent,", 1)
	writeTest(t, filepath.Join(f.root, "runner.py"), []byte(script), 0700)
	definitions := map[int]map[string]any{}
	for version := 2; version <= 3; version++ {
		command := []string{"/usr/bin/python3", filepath.Join(f.root, "runner.py"), fmt.Sprintf("command-v%d", version), fmt.Sprintf("argument-v%d", version), f.records}
		limits := map[string]string{}
		for _, key := range []string{"start_check_timeout", "finish_check_timeout", "before_timeout", "agent_timeout", "after_timeout", "run_timeout"} {
			limits[key] = fmt.Sprintf("%ds", version*10)
		}
		work := filepath.Join(f.root, fmt.Sprintf("work-v%d", version))
		if e := os.Mkdir(work, 0700); e != nil {
			t.Fatal(e)
		}
		definitions[version] = map[string]any{"version": 1, "id": "runtime", "workdir": work, "agent": command, "before": command, "after": command, "prompt": fmt.Sprintf("prompt-v%d", version), "env": map[string]string{"OVERRIDE": fmt.Sprintf("env-v%d", version)}, "inherit_env": []string{"CAPTURE"}, "start": map[string]any{"check": command, "poll_every": "1s", "wait_timeout": "0s"}, "finish": map[string]any{"check": command, "max_calls": version - 1}, "repeat": version - 1, "repeat_delay": "0s", "limits": limits}
	}
	raw, e := json.Marshal(definitions[2])
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, f.manifest, raw, 0600)
	f.call(t, "task", "update", f.manifest, "--if-version", "1")
	raw, e = json.Marshal(definitions[3])
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, f.manifest, raw, 0600)
	writeTest(t, f.input, []byte(`{"task_id":"runtime","input_key":"raced-input","input":{"value":"race-original","success_at":99},"concurrency_key":"race-resource"}`), 0600)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var submitted, updated map[string]any
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		updated = f.call(t, "task", "update", f.manifest, "--if-version", "2")
	}()
	go func() { defer wg.Done(); <-start; submitted = f.call(t, "run", "submit", f.input) }()
	close(start)
	wg.Wait()
	if updated["task_version"] != float64(3) {
		t.Fatalf("update: %v", updated)
	}
	selected := int(submitted["task_version"].(float64))
	definition, ok := definitions[selected]
	if !ok {
		t.Fatalf("unexpected selected version: %v", submitted)
	}
	db := f.database(t)
	var storedVersion int
	var snapshot, input, key, inputKey string
	e = db.QueryRow(`SELECT task_version,snapshot,input,concurrency_key,input_key FROM submissions WHERE id=?`, submitted["submission_id"]).Scan(&storedVersion, &snapshot, &input, &key, &inputKey)
	if e != nil || storedVersion != selected || input != `{"success_at":99,"value":"race-original"}` || key != "race-resource" || inputKey != "raced-input" {
		t.Fatalf("admission snapshot: version=%d input=%s key=%s inputKey=%s: %v", storedVersion, input, key, inputKey, e)
	}
	var snap map[string]any
	if e = json.Unmarshal([]byte(snapshot), &snap); e != nil {
		t.Fatal(e)
	}
	expectedRaw, e := json.Marshal(definition)
	if e != nil {
		t.Fatal(e)
	}
	var expected map[string]any
	if e = json.Unmarshal(expectedRaw, &expected); e != nil {
		t.Fatal(e)
	}
	for field, want := range expected {
		if field == "env" {
			continue
		}
		if !reflect.DeepEqual(snap[field], want) {
			t.Fatalf("mixed snapshot field %s: got %v want %v", field, snap[field], want)
		}
	}
	env := snap["env"].(map[string]any)
	if env["OVERRIDE"] != fmt.Sprintf("env-v%d", selected) || env["CAPTURE"] != "submission-secret" {
		t.Fatalf("mixed environment: %v", env)
	}
	// A stale CAS proposes a third, distinguishable definition; it must not change either the current task or the admitted snapshot.
	f.task["prompt"] = "stale-CAS-must-not-run"
	raw, e = json.Marshal(f.task)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, f.manifest, raw, 0600)
	f.rejected(t, 3, "version_conflict", "task", "update", f.manifest, "--if-version", "2")
	var currentVersion int
	if e = db.QueryRow(`SELECT version FROM tasks WHERE id='runtime'`).Scan(&currentVersion); e != nil || currentVersion != 3 {
		t.Fatalf("CAS mutated task version: %d %v", currentVersion, e)
	}
	// Ensure the selected admission differs from the current definition in either race ordering.
	f.task["prompt"] = "future-definition-must-not-change-admitted-rounds"
	raw, e = json.Marshal(f.task)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, f.manifest, raw, 0600)
	f.call(t, "task", "update", f.manifest, "--if-version", "3")
	t.Setenv("CAPTURE", "changed-after-race")
	f.daemon(t)
	deadline := time.Now().Add(15 * time.Second)
	for {
		var state string
		if e = db.QueryRow(`SELECT state FROM submissions WHERE id=?`, submitted["submission_id"]).Scan(&state); e != nil {
			t.Fatal(e)
		}
		if state == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("all rounds did not finish: %s", state)
		}
		time.Sleep(15 * time.Millisecond)
	}
	rows, e := db.Query(`SELECT id,run_seq,state,calls_used FROM runs WHERE submission_id=? ORDER BY run_seq`, submitted["submission_id"])
	if e != nil {
		t.Fatal(e)
	}
	type round struct {
		id, state  string
		seq, calls int
	}
	var rounds []round
	for rows.Next() {
		var r round
		if e = rows.Scan(&r.id, &r.seq, &r.state, &r.calls); e != nil {
			t.Fatal(e)
		}
		rounds = append(rounds, r)
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	if e = rows.Close(); e != nil {
		t.Fatal(e)
	}
	if len(rounds) != selected {
		t.Fatalf("repeat snapshot: %d rounds want %d", len(rounds), selected)
	}
	expectedCalls := selected - 1
	for index, r := range rounds {
		if r.seq != index+1 || r.state != "failed" || r.calls != expectedCalls {
			t.Fatalf("round budget/result: %+v", r)
		}
		view := f.call(t, "run", "show", r.id, "--json")
		if view["stage"] != "failed:max_calls" || view["calls_used"] != float64(expectedCalls) || view["task_version"] != float64(selected) {
			t.Fatalf("run response: %v", view)
		}
		records := f.recordsFor(t, r.id)
		if len(records) != len(runtimeStages(expectedCalls)) {
			t.Fatalf("external steps: %d want %d", len(records), len(runtimeStages(expectedCalls)))
		}
		stages := map[string]int{}
		for _, record := range records {
			context := record["context"].(map[string]any)
			env := record["env"].(map[string]any)
			stages[context["stage"].(string)]++
			if context["stage"] != "start_check" && !reflect.DeepEqual(record["held_resource"], []any{"race-resource"}) {
				t.Fatalf("wrong execution resource: %v", record["held_resource"])
			}
			// Each run starts with the selected 20s/30s budget; all fixture commands complete far below their ten-second separation.
			remainingNS, ok := record["remaining_ns"].(float64)
			maximum := float64(selected*10) * float64(time.Second)
			if !ok || remainingNS > maximum || remainingNS < maximum-float64(8*time.Second) {
				t.Fatalf("mixed live run limit: %v want (%v,%v]", record["remaining_ns"], maximum-float64(8*time.Second), maximum)
			}
			if context["task_version"] != float64(selected) || context["run_seq"] != float64(index+1) || context["submission_id"] != submitted["submission_id"] || context["input_key"] != "raced-input" || context["prompt"] != definition["prompt"] || !reflect.DeepEqual(context["input"], map[string]any{"success_at": float64(99), "value": "race-original"}) {
				t.Fatalf("mixed external context: %v", context)
			}
			if !reflect.DeepEqual(record["argv"], []any{fmt.Sprintf("command-v%d", selected), fmt.Sprintf("argument-v%d", selected)}) || record["cwd"] != definition["workdir"] || env["OVERRIDE"] != fmt.Sprintf("env-v%d", selected) || env["CAPTURE"] != "submission-secret" {
				t.Fatalf("mixed external command/environment: %v", record)
			}
		}
		if !reflect.DeepEqual(stages, map[string]int{"start_check": 2, "before": 1, "finish_check": expectedCalls + 1, "agent": expectedCalls}) {
			t.Fatalf("external stage distribution: %v", stages)
		}
	}
	var total, remaining int
	if e = db.QueryRow(`SELECT total,remaining FROM repeat_budgets WHERE submission_id=?`, submitted["submission_id"]).Scan(&total, &remaining); e != nil || total != selected-1 || remaining != 0 {
		t.Fatalf("repeat budget total=%d remaining=%d: %v", total, remaining, e)
	}
	var finalSnapshot string
	if e = db.QueryRow(`SELECT snapshot FROM submissions WHERE id=?`, submitted["submission_id"]).Scan(&finalSnapshot); e != nil || finalSnapshot != snapshot {
		t.Fatalf("snapshot changed during execution: %v", e)
	}
}

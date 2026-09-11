package test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func orderFixture(t *testing.T, repeat int, delay, wait string) runtimeFixture {
	t.Helper()
	f := newRuntime(t)
	writeTest(t, filepath.Join(f.root, "runner.py"), []byte(orderScript), 0700)
	f.task["repeat"] = repeat
	f.task["repeat_delay"] = delay
	f.task["start"] = map[string]any{"check": f.task["agent"], "poll_every": "1s", "wait_timeout": wait}
	updateOrderTask(t, f)
	return f
}
func updateOrderTask(t *testing.T, f runtimeFixture) {
	t.Helper()
	db := f.database(t)
	var version int
	if e := db.QueryRow("SELECT version FROM tasks WHERE id='runtime'").Scan(&version); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(f.task)
	writeTest(t, f.manifest, b, 0600)
	f.call(t, "task", "update", f.manifest, "--if-version", fmt.Sprint(version))
}
func submitOrder(t *testing.T, f runtimeFixture, input map[string]any, key string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"task_id": "runtime", "input_key": key, "input": input, "concurrency_key": "resource"})
	writeTest(t, f.input, b, 0600)
	return f.call(t, "run", "submit", f.input)
}
func waitUntil(t *testing.T, check func() bool) {
	t.Helper()
	until := time.Now().Add(12 * time.Second)
	for time.Now().Before(until) {
		if check() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func waitExternal(t *testing.T, f runtimeFixture, label, stage string, seq int) {
	t.Helper()
	waitUntil(t, func() bool {
		for _, r := range orderEvents(t, f) {
			if r.Label == label && r.Stage == stage && r.Seq == seq {
				return true
			}
		}
		return false
	})
}

type orderEvent struct {
	Label, Stage, RunID string
	Seq                 int
	At                  int64
}

func orderEvents(t *testing.T, f runtimeFixture) []orderEvent {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(f.records, "events"))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		t.Fatal(e)
	}
	events := []orderEvent{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var event orderEvent
		if e = json.Unmarshal([]byte(line), &event); e != nil {
			t.Fatalf("external event %s: %v", line, e)
		}
		events = append(events, event)
	}
	return events
}
func completedRuns(t *testing.T, f runtimeFixture, submission map[string]any, total int) []map[string]any {
	t.Helper()
	db := f.database(t)
	waitUntil(t, func() bool {
		var state string
		if e := db.QueryRow("SELECT state FROM submissions WHERE id=?", submission["submission_id"]).Scan(&state); e != nil {
			t.Fatal(e)
		}
		return state == "completed"
	})
	rows, e := db.Query("SELECT id FROM runs WHERE submission_id=? ORDER BY run_seq", submission["submission_id"])
	if e != nil {
		t.Fatal(e)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	if e = rows.Close(); e != nil {
		t.Fatal(e)
	}
	if len(ids) != total {
		t.Fatalf("round count %d want %d", len(ids), total)
	}
	out := []map[string]any{}
	for _, id := range ids {
		out = append(out, f.call(t, "run", "show", id, "--json"))
	}
	return out
}
func assertOrder(t *testing.T, f runtimeFixture, want []string) {
	t.Helper()
	got := []string{}
	for _, e := range orderEvents(t, f) {
		if e.Stage == "before" {
			got = append(got, fmt.Sprintf("%s%d", e.Label, e.Seq))
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("external round order %v want %v", got, want)
	}
}
func inputOrder(t *testing.T, fail bool) {
	t.Helper()
	f := orderFixture(t, 2, "0s", "0s")
	gate := filepath.Join(f.root, "release-A")
	aInput := map[string]any{"label": "A", "before_gate": gate}
	if fail {
		aInput["fail_after_run"] = 1
	}
	a := submitOrder(t, f, aInput, "same")
	f.daemon(t)
	waitExternal(t, f, "A", "before", 1)
	b := submitOrder(t, f, map[string]any{"label": "B"}, "same")
	c := submitOrder(t, f, map[string]any{"label": "C"}, "same")
	writeTest(t, gate, nil, 0600)
	aruns := completedRuns(t, f, a, 3)
	bruns := completedRuns(t, f, b, 3)
	cruns := completedRuns(t, f, c, 3)
	assertOrder(t, f, []string{"A1", "A2", "A3", "B1", "B2", "B3", "C1", "C2", "C3"})
	expectedRuns := []expectedOrderRun{}
	for i, list := range [][]map[string]any{aruns, bruns, cruns} {
		for j, run := range list {
			want := "succeeded"
			if fail && i == 0 && j == 0 {
				want = "failed:after"
			}
			if run["stage"] != want || run["calls_used"] != float64(1) || run["run_seq"] != float64(j+1) {
				t.Fatalf("run %d/%d %v", i, j, run)
			}
			expectedRuns = append(expectedRuns, expectedOrderRun{run["run_id"].(string), []string{"A", "B", "C"}[i], j + 1, append(runtimeStages(1), "after")})
			input := run["input"].(map[string]any)
			if input["label"] != []string{"A", "B", "C"}[i] {
				t.Fatalf("input lost %v", input)
			}
		}
	}
	assertOrderExecutions(t, f, expectedRuns)
	db := f.database(t)
	var submissions, remaining int
	if e := db.QueryRow("SELECT count(*) FROM submissions").Scan(&submissions); e != nil || submissions != 3 {
		t.Fatalf("inputs lost %d %v", submissions, e)
	}
	if e := db.QueryRow("SELECT sum(remaining) FROM repeat_budgets").Scan(&remaining); e != nil || remaining != 0 {
		t.Fatalf("repeat budget %d %v", remaining, e)
	}
}
func blockedPredecessor(t *testing.T) {
	t.Helper()
	f := orderFixture(t, 2, "0s", "0s")
	a := submitOrder(t, f, map[string]any{"label": "A", "block_before_run": 1}, "same")
	b := submitOrder(t, f, map[string]any{"label": "B"}, "same")
	c := submitOrder(t, f, map[string]any{"label": "C"}, "same")
	f.daemon(t)
	view := f.await(t, a["run_id"].(string))
	if view["stage"] != "blocked:outcome_unknown" {
		t.Fatalf("A not blocked %v", view)
	}
	// An unrelated false start check proves the daemon continues polling while A blocks B/C.
	d := submitOrder(t, f, map[string]any{"label": "D", "start_false": true}, "other")
	waitUntil(t, func() bool {
		n := 0
		for _, event := range orderEvents(t, f) {
			if event.Label == "D" && event.Stage == "start_check" {
				n++
			}
		}
		return n >= 2
	})
	for _, event := range orderEvents(t, f) {
		if event.Label == "B" || event.Label == "C" {
			t.Fatalf("blocked predecessor allowed successor command %v", event)
		}
	}
	db := f.database(t)
	for index, sub := range []map[string]any{b, c} {
		var n int
		if e := db.QueryRow("SELECT count(*) FROM steps WHERE run_id=?", sub["run_id"]).Scan(&n); e != nil || n != 0 {
			t.Fatalf("successor Step %d %v", n, e)
		}
		v := f.call(t, "run", "show", sub["run_id"].(string), "--json")
		if v["input"].(map[string]any)["label"] != []string{"B", "C"}[index] {
			t.Fatalf("blocked successor input changed %v", v["input"])
		}
		if v["state"] != "waiting" || v["calls_used"] != float64(0) || v["repeat_remaining"] != float64(2) {
			t.Fatalf("successor input/budget changed %v", v)
		}
	}
	var count int
	if e := db.QueryRow("SELECT count(*) FROM submissions WHERE state='active'").Scan(&count); e != nil || count != 4 {
		t.Fatalf("inputs dropped %d %v", count, e)
	}
	if d["deduplicated"] != false {
		t.Fatal("unrelated observer not admitted")
	}
	assertRunFiles(t, f, map[string]string{})
}
func afterRepeat(t *testing.T, checkSummary bool) {
	t.Helper()
	f := orderFixture(t, 1, "0s", "0s")
	sub := submitOrder(t, f, map[string]any{"label": "A", "fail_after_run": 1}, "same")
	f.daemon(t)
	runs := completedRuns(t, f, sub, 2)
	assertOrder(t, f, []string{"A1", "A2"})
	if runs[0]["stage"] != "failed:after" || runs[1]["stage"] != "succeeded" || runs[0]["run_id"] == runs[1]["run_id"] {
		t.Fatalf("repeat outcomes %v", runs)
	}
	for index, r := range runs {
		if r["run_seq"] != float64(index+1) || r["calls_used"] != float64(1) {
			t.Fatalf("fresh budget %v", r)
		}
		stages := []string{}
		for _, value := range r["steps"].([]any) {
			s := value.(map[string]any)
			stages = append(stages, s["stage"].(string))
			if s["stage"] == "after" {
				want := float64(0)
				if index == 0 {
					want = 7
				}
				if s["result"].(map[string]any)["exit_code"] != want {
					t.Fatalf("after result overwritten %v", s)
				}
			}
		}
		if !reflect.DeepEqual(stages, append(runtimeStages(1), "after")) {
			t.Fatalf("after failure retried same run %v", stages)
		}
	}
	assertOrderExecutions(t, f, []expectedOrderRun{{runs[0]["run_id"].(string), "A", 1, append(runtimeStages(1), "after")}, {runs[1]["run_id"].(string), "A", 2, append(runtimeStages(1), "after")}})
	if checkSummary {
		for _, r := range runs {
			want := map[string]any{"succeeded": float64(1), "failed": float64(1), "skipped": float64(0), "last_result": "succeeded"}
			if !reflect.DeepEqual(r["summary"], want) {
				t.Fatalf("historical summary %v want %v", r["summary"], want)
			}
			if r["repeat_remaining"] != float64(0) {
				t.Fatalf("repeat remaining %v", r)
			}
		}
	}
}

const orderScript = `import os,sys,json,time,signal
c=json.load(open(os.environ['TODOABLE_CONTEXT_PATH']))
inp=c['input'];s=c['stage'];seq=c['run_seq'];label=inp.get('label','?');rd=c['run_dir']
sys.stdin.buffer.read()
record={'Label':label,'Stage':s,'RunID':c['run_id'],'Seq':seq,'At':time.monotonic_ns()}
fd=os.open(os.path.join(sys.argv[3],'events'),os.O_WRONLY|os.O_CREAT|os.O_APPEND,0o600)
os.write(fd,(json.dumps(record)+'\n').encode());os.close(fd)
gates=inp.get(s+'_gates',{})
gate=gates.get(str(seq),inp.get(s+'_gate'))
if gate and (str(seq) in gates or seq==inp.get(s+'_gate_run',1)):
 while not os.path.exists(gate):time.sleep(.01)
if s=='start_check':sys.exit(1 if inp.get('start_false',False) else 0)
if s=='before':
 if seq==inp.get('block_before_run'):os.kill(os.getpid(),signal.SIGTERM)
 sys.exit(0)
countpath=os.path.join(rd,'count')
count=int(open(countpath).read()) if os.path.exists(countpath) else 0
if s=='finish_check':sys.exit(0 if count>=1 else 1)
if s=='agent':
 open(countpath,'w').write(str(count+1));open(os.path.join(rd,'artifact'),'w').write(label+'-'+str(seq));sys.exit(0)
if s=='after':
 open(os.path.join(rd,'published'),'a').write(label+'-'+str(seq)+'\n')
 sys.exit(7 if seq==inp.get('fail_after_run') else 0)
`

type fairFixture struct {
	f                    runtimeFixture
	a, b                 map[string]any
	gateA, gateB, gateA2 string
	a2                   string
	bReady               int64
}

func newFair(t *testing.T) *fairFixture {
	t.Helper()
	f := orderFixture(t, 1, "0s", "0s")
	// No start subprocess: A's next round and the already-ready B compete in the same reservation transaction.
	delete(f.task, "start")
	updateOrderTask(t, f)
	fair := &fairFixture{f: f, gateA: filepath.Join(f.root, "A1-release"), gateB: filepath.Join(f.root, "B1-release"), gateA2: filepath.Join(f.root, "A2-release")}
	fair.a = submitOrder(t, f, map[string]any{"label": "A", "before_gates": map[string]string{"1": fair.gateA, "2": fair.gateA2}}, "A")
	f.daemon(t)
	waitExternal(t, f, "A", "before", 1)
	f.task["repeat"] = 0
	updateOrderTask(t, f)
	fair.b = submitOrder(t, f, map[string]any{"label": "B", "before_gate": fair.gateB}, "B")
	fair.bReady = waitReady(t, f, fair.b["run_id"].(string))
	return fair
}
func waitReady(t *testing.T, f runtimeFixture, id string) int64 {
	t.Helper()
	db := f.database(t)
	var ready int64
	waitUntil(t, func() bool {
		e := db.QueryRow("SELECT ready_seq FROM run_schedule WHERE run_id=?", id).Scan(&ready)
		return e == nil && ready > 0
	})
	return ready
}
func assertResource(t *testing.T, f runtimeFixture, want string) {
	t.Helper()
	var got string
	if e := f.database(t).QueryRow("SELECT run_id FROM resources WHERE key='resource'").Scan(&got); e != nil || got != want {
		t.Fatalf("resource owner %s want %s: %v", got, want, e)
	}
}
func fairFirst(t *testing.T, p *fairFixture) {
	t.Helper()
	assertResource(t, p.f, p.a["run_id"].(string))
	writeTest(t, p.gateA, nil, 0600)
	waitUntil(t, func() bool {
		count := 0
		for _, event := range orderEvents(t, p.f) {
			if event.Stage == "before" {
				count++
			}
		}
		return count >= 2
	})
	assertOrder(t, p.f, []string{"A1", "B1"})
	assertResource(t, p.f, p.b["run_id"].(string))
	waitUntil(t, func() bool {
		return p.f.database(t).QueryRow("SELECT id FROM runs WHERE submission_id=? AND run_seq=2", p.a["submission_id"]).Scan(&p.a2) == nil
	})
	aReady := waitReady(t, p.f, p.a2)
	if aReady <= p.bReady {
		t.Fatalf("new repeat got old priority: A2=%d B=%d", aReady, p.bReady)
	}
	assertOrder(t, p.f, []string{"A1", "B1"})
	var held int
	if e := p.f.database(t).QueryRow("SELECT slot_held FROM run_schedule WHERE run_id=?", p.a["run_id"]).Scan(&held); e != nil || held != 0 {
		t.Fatalf("finished A retained slot %d %v", held, e)
	}
	assertOrderExecutions(t, p.f, []expectedOrderRun{{p.a["run_id"].(string), "A", 1, []string{"before", "finish_check", "agent", "finish_check", "after"}}})
}
func fairSecond(t *testing.T, p *fairFixture) {
	t.Helper()
	if p.a2 == "" {
		t.Fatal("first fairness phase failed")
	}
	c := submitOrder(t, p.f, map[string]any{"label": "C"}, "C")
	cReady := waitReady(t, p.f, c["run_id"].(string))
	aReady := waitReady(t, p.f, p.a2)
	if cReady <= aReady {
		t.Fatalf("ready sequence C=%d A2=%d", cReady, aReady)
	}
	writeTest(t, p.gateB, nil, 0600)
	waitExternal(t, p.f, "A", "before", 2)
	assertResource(t, p.f, p.a2)
	assertOrder(t, p.f, []string{"A1", "B1", "A2"})
	writeTest(t, p.gateA2, nil, 0600)
	completedRuns(t, p.f, p.a, 2)
	completedRuns(t, p.f, p.b, 1)
	completedRuns(t, p.f, c, 1)
	assertOrder(t, p.f, []string{"A1", "B1", "A2", "C1"})
	expectedStages := []string{"before", "finish_check", "agent", "finish_check", "after"}
	assertOrderExecutions(t, p.f, []expectedOrderRun{{p.a["run_id"].(string), "A", 1, expectedStages}, {p.a2, "A", 2, expectedStages}, {p.b["run_id"].(string), "B", 1, expectedStages}, {c["run_id"].(string), "C", 1, expectedStages}})

	var n int
	if e := p.f.database(t).QueryRow("SELECT count(*) FROM resources").Scan(&n); e != nil || n != 0 {
		t.Fatalf("completed runs retain resource %d %v", n, e)
	}
}
func startWaiting(t *testing.T, delayed bool) {
	t.Helper()
	delay := "0s"
	wait := "2400ms"
	if delayed {
		delay = "1500ms"
		wait = "1200ms"
	}
	f := orderFixture(t, 1, delay, wait)
	var predecessor map[string]any
	expectedRuns := []expectedOrderRun{}
	gate := filepath.Join(f.root, "predecessor-release")
	if delayed {
		predecessor = submitOrder(t, f, map[string]any{"label": "P", "before_gate": gate}, "finite")
	}
	finite := submitOrder(t, f, map[string]any{"label": "F", "start_false": true}, "finite")
	f.task["start"] = map[string]any{"check": f.task["agent"], "poll_every": "1s", "wait_timeout": "0s"}
	updateOrderTask(t, f)
	infinite := submitOrder(t, f, map[string]any{"label": "I", "start_false": true}, "infinite")
	f.daemon(t)
	if delayed {
		waitExternal(t, f, "P", "before", 1)
		time.Sleep(1500 * time.Millisecond)
		for _, event := range orderEvents(t, f) {
			if event.Label == "F" {
				t.Fatalf("predecessor wait consumed start phase %v", event)
			}
		}
		var first int64
		if e := f.database(t).QueryRow("SELECT start_started_at FROM run_schedule WHERE run_id=?", finite["run_id"]).Scan(&first); e != nil || first != 0 {
			t.Fatalf("start deadline began behind predecessor %d %v", first, e)
		}
		writeTest(t, gate, nil, 0600)
		previous := completedRuns(t, f, predecessor, 2)
		for index, run := range previous {
			expectedRuns = append(expectedRuns, expectedOrderRun{run["run_id"].(string), "P", index + 1, append(runtimeStages(1), "after")})
		}

	}
	waitExternal(t, f, "F", "start_check", 1)
	db := f.database(t)
	var first, deadline int64
	if e := db.QueryRow("SELECT start_started_at,wait_deadline FROM run_schedule WHERE run_id=?", finite["run_id"]).Scan(&first, &deadline); e != nil {
		t.Fatal(e)
	}
	expected, _ := time.ParseDuration(wait)
	if deadline-first != int64(expected) {
		t.Fatalf("start deadline %d want %d", deadline-first, expected)
	}
	// Observe a second false check before the finite deadline; neither false result spends a call or repeat.
	waitUntil(t, func() bool {
		n := 0
		for _, event := range orderEvents(t, f) {
			if event.Label == "F" && event.Seq == 1 && event.Stage == "start_check" {
				n++
			}
		}
		return n >= 2
	})
	var calls, remaining int
	var state string
	if e := db.QueryRow("SELECT r.calls_used,r.state,b.remaining FROM runs r JOIN repeat_budgets b ON b.submission_id=r.submission_id WHERE r.id=?", finite["run_id"]).Scan(&calls, &state, &remaining); e != nil || calls != 0 || state != "waiting" || remaining != 1 {
		t.Fatalf("false condition spent budget: %d %s %d %v", calls, state, remaining, e)
	}
	runs := completedRuns(t, f, finite, 2)
	for i, run := range runs {
		if run["stage"] != "skipped:start_timeout" || run["calls_used"] != float64(0) || run["run_seq"] != float64(i+1) {
			t.Fatalf("finite start outcome %v", run)
		}
		for _, value := range run["steps"].([]any) {
			step := value.(map[string]any)
			result := step["result"].(map[string]any)
			if step["stage"] != "start_check" || result["kind"] != "exited" || result["exit_code"] != float64(1) {
				t.Fatalf("non-false start Step %v", step)
			}
		}
		if !reflect.DeepEqual(run["summary"], map[string]any{"succeeded": float64(0), "failed": float64(0), "skipped": float64(2), "last_result": "skipped:start_timeout"}) {
			t.Fatalf("skips not summarized %v", run["summary"])
		}
	}
	iv := f.call(t, "run", "show", infinite["run_id"].(string), "--json")
	if iv["state"] != "waiting" || iv["calls_used"] != float64(0) || iv["repeat_remaining"] != float64(1) || len(iv["runs"].([]any)) != 1 || iv["start_deadline"] != nil {
		t.Fatalf("infinite wait consumed budget %v", iv)
	}
	if delayed {
		var eligible, started int64
		if e := db.QueryRow("SELECT eligible_at,start_started_at FROM run_schedule WHERE run_id=?", runs[1]["run_id"]).Scan(&eligible, &started); e != nil || started < eligible {
			t.Fatalf("repeat delay charged to start deadline: %d %d %v", eligible, started, e)
		}
		var secondDeadline int64
		if e := db.QueryRow("SELECT wait_deadline FROM run_schedule WHERE run_id=?", runs[1]["run_id"]).Scan(&secondDeadline); e != nil || secondDeadline-started != int64(expected) {
			t.Fatalf("second start did not receive full waiting window: %d %d %v", secondDeadline, started, e)
		}
	}
	assertOrderExecutions(t, f, expectedRuns)
	for _, event := range orderEvents(t, f) {
		if (event.Label == "F" || event.Label == "I") && event.Stage != "start_check" {
			t.Fatalf("false condition ran changing stage %v", event)
		}
	}
}

// assertRunFiles observes every fixture output, including outputs under unexpected Run directories.
func assertRunFiles(t *testing.T, f runtimeFixture, want map[string]string) {
	t.Helper()
	actual := map[string]string{}
	for _, name := range []string{"artifact", "count", "published"} {
		paths, e := filepath.Glob(filepath.Join(f.dir, "runs", "*", name))
		if e != nil {
			t.Fatal(e)
		}
		for _, path := range paths {
			b, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			actual[path] = string(b)
		}
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("run output files differ: got %v want %v", actual, want)
	}
}

type expectedOrderRun struct {
	id, label string
	seq       int
	stages    []string
}

func assertOrderExecutions(t *testing.T, f runtimeFixture, runs []expectedOrderRun) {
	t.Helper()
	events := orderEvents(t, f)
	wantFiles := map[string]string{}
	for _, run := range runs {
		stages := []string{}
		for _, event := range events {
			if event.RunID == run.id {
				if event.Label != run.label || event.Seq != run.seq {
					t.Fatalf("external Run identity changed %v", event)
				}
				stages = append(stages, event.Stage)
			}
		}
		if !reflect.DeepEqual(stages, run.stages) {
			t.Fatalf("external stages for %s%d: %v want %v", run.label, run.seq, stages, run.stages)
		}
		dir := filepath.Join(f.dir, "runs", run.id)
		wantFiles[filepath.Join(dir, "artifact")] = fmt.Sprintf("%s-%d", run.label, run.seq)
		wantFiles[filepath.Join(dir, "count")] = "1"
		wantFiles[filepath.Join(dir, "published")] = fmt.Sprintf("%s-%d\n", run.label, run.seq)
	}
	assertRunFiles(t, f, wantFiles)
}

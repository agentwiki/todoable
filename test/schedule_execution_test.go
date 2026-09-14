package test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

type scheduleFixture struct {
	intakeFixture
	root, clock, records string
}

func newSchedule(t *testing.T, now string) scheduleFixture {
	t.Helper()
	root := t.TempDir()
	f := scheduleFixture{intakeFixture: intakeFixture{bin: filepath.Join(root, "todoable"), dir: filepath.Join(root, "data"), input: filepath.Join(root, "input.json")}, root: root, clock: filepath.Join(root, "clock"), records: filepath.Join(root, "records")}
	build := exec.Command("go", "build", "-race", "-ldflags", "-X github.com/agentwiki/todoable/internal/adapters/local.testControls=enabled", "-o", f.bin, "./cmd/todoable")
	build.Dir = ".."
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	if e := os.Mkdir(f.records, 0700); e != nil {
		t.Fatal(e)
	}
	writeTest(t, filepath.Join(root, "report.py"), []byte(scheduleReport), 0700)
	f.at(t, now)
	t.Setenv("TODOABLE_TEST_CLOCK", f.clock)
	return f
}

const scheduleReport = `import datetime,json,os,pathlib,sys,time
root=pathlib.Path(sys.argv[1]);c=json.load(open(os.environ['TODOABLE_CONTEXT_PATH']))
record=root/'records'/(c['step_id']+'.json');record.write_text(json.dumps(c))
out=root/'records'/(c['run_id']+'.report')
w=c['input']['occurrence'];data=c['input']['data']
def stamp(s):return datetime.datetime.fromisoformat(s.replace('Z','+00:00'))
rows=json.load(open(root/'events.json'))
selected=[r['id'] for r in rows if stamp(w['window_start'])<=stamp(r['at'])<stamp(w['window_end'])]
expected={'window_start':w['window_start'],'window_end':w['window_end'],'rows':selected,'report':data['report']}
if c['stage']=='agent':
 while (root/'hold').exists() and not (root/('release-'+c['run_id'])).exists():time.sleep(.01)
 out.write_text(json.dumps(expected));sys.exit(0)
if c['stage']=='finish_check':
 sys.exit(0 if out.exists() and json.loads(out.read_text())==expected else 1)
sys.exit(7)
`

func (f scheduleFixture) at(t *testing.T, now string) {
	t.Helper()
	writeTest(t, f.clock+".tmp", []byte(now), 0600)
	if e := os.Rename(f.clock+".tmp", f.clock); e != nil {
		t.Fatal(e)
	}
}
func (f scheduleFixture) events(t *testing.T, rows ...string) {
	t.Helper()
	values := []map[string]string{}
	for i, at := range rows {
		values = append(values, map[string]string{"id": fmt.Sprint(i + 1), "at": at})
	}
	b, e := json.Marshal(values)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, filepath.Join(f.root, "events.json"), b, 0600)
}
func (f scheduleFixture) task(id string, schedule map[string]any) map[string]any {
	return map[string]any{"version": 1, "id": id, "workdir": f.root, "agent": []string{"/usr/bin/python3", filepath.Join(f.root, "report.py"), f.root}, "prompt": "Generate a report for the exact supplied half-open window", "finish": map[string]any{"check": []string{"/usr/bin/python3", filepath.Join(f.root, "report.py"), f.root}}, "schedule": schedule}
}
func everySchedule(every string) map[string]any {
	return map[string]any{"every": every, "input_key": "period", "concurrency_key": "report", "input": map[string]any{"report": "daily"}}
}
func cronSchedule(cron, zone string) map[string]any {
	return map[string]any{"cron": cron, "timezone": zone, "input_key": "period", "concurrency_key": "report", "input": map[string]any{"report": "daily"}}
}
func (f scheduleFixture) manifest(t *testing.T, task map[string]any) string {
	t.Helper()
	p := filepath.Join(f.root, task["id"].(string)+".json")
	b, e := json.Marshal(task)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, p, b, 0600)
	return p
}
func (f scheduleFixture) register(t *testing.T, id string, schedule map[string]any) {
	t.Helper()
	f.call(t, "task", "register", f.manifest(t, f.task(id, schedule)))
}
func (f scheduleFixture) db(t *testing.T) *sql.DB {
	t.Helper()
	db, e := sql.Open("sqlite3", "file:"+f.dir+"/todoable.db?mode=ro&_busy_timeout=5000")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func (f scheduleFixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if e := f.db(t).QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func waitSchedule(t *testing.T, check func() bool) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("schedule observation timed out")
}
func (f scheduleFixture) show(t *testing.T, id string) map[string]any {
	t.Helper()
	return f.call(t, "schedule", "show", id, "--json")
}
func (f scheduleFixture) observed(t *testing.T, id, at string) map[string]any {
	t.Helper()
	var v map[string]any
	waitSchedule(t, func() bool { v = f.show(t, id); return v["observed_at"] == at })
	return v
}
func (f scheduleFixture) runAt(t *testing.T, id, at string) string {
	t.Helper()
	var run string
	waitSchedule(t, func() bool {
		return f.db(t).QueryRow("SELECT r.id FROM runs r JOIN submissions s ON s.id=r.submission_id JOIN scheduled_submissions ss ON ss.submission_id=s.id WHERE s.task_id=? AND ss.scheduled_at=?", id, at).Scan(&run) == nil
	})
	return run
}
func (f scheduleFixture) daemon(t *testing.T) func(bool) {
	t.Helper()
	cmd := exec.Command(f.bin, "--data-dir", f.dir, "daemon", "run")
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	stopped := false
	stop := func(kill bool) {
		if stopped {
			return
		}
		stopped = true
		if kill {
			_ = cmd.Process.Kill()
		} else {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case e := <-done:
			if !kill && e != nil {
				t.Errorf("daemon: %v %s", e, stderr.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("daemon failed to stop")
		}
	}
	t.Cleanup(func() { stop(false) })
	return stop
}
func (f scheduleFixture) assertReport(t *testing.T, run, start, end string, rows []string) {
	t.Helper()
	waitSchedule(t, func() bool { return f.call(t, "run", "show", run, "--json")["state"] == "succeeded" })
	view := f.call(t, "run", "show", run, "--json")
	if view["scheduled_at"] != end {
		t.Fatalf("scheduled_at: %v", view)
	}
	want := map[string]any{"data": map[string]any{"report": "daily"}, "occurrence": map[string]any{"scheduled_at": end, "window_start": start, "window_end": end}}
	if !reflect.DeepEqual(view["input"], want) {
		t.Fatalf("input: %v want %v", view["input"], want)
	}
	paths, e := filepath.Glob(filepath.Join(f.records, "*.json"))
	if e != nil {
		t.Fatal(e)
	}
	stages := map[string]int{}
	for _, p := range paths {
		var c map[string]any
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(b, &c); e != nil {
			t.Fatal(e)
		}
		if c["run_id"] == run {
			if !reflect.DeepEqual(c["input"], want) || c["submission_id"] != view["submission_id"] {
				t.Fatalf("external context mismatch: %v", c)
			}
			stages[c["stage"].(string)]++
		}
	}
	if !reflect.DeepEqual(stages, map[string]int{"agent": 1, "finish_check": 2}) {
		t.Fatalf("external stages: %v", stages)
	}
	var report struct {
		Start  string   `json:"window_start"`
		End    string   `json:"window_end"`
		Rows   []string `json:"rows"`
		Report string   `json:"report"`
	}
	b, e := os.ReadFile(filepath.Join(f.records, run+".report"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &report); e != nil {
		t.Fatal(e)
	}
	if report.Start != start || report.End != end || report.Report != "daily" || !reflect.DeepEqual(report.Rows, rows) {
		t.Fatalf("report: %s", b)
	}
}
func (f scheduleFixture) awaitAgent(t *testing.T, run string) {
	t.Helper()
	waitSchedule(t, func() bool {
		v := f.call(t, "run", "show", run, "--json")
		return v["stage"] == "agent" && len(v["steps"].([]any)) == 2
	})
}
func TestScenario_SC_16(t *testing.T) {
	f := newSchedule(t, "2026-09-10T00:00:00Z")
	f.events(t, "2026-09-10T00:00:01Z", "2026-09-10T00:00:10Z", "2026-09-10T00:00:31Z", "2026-09-10T00:00:40Z")
	f.register(t, "daily", everySchedule("10s"))
	f.call(t, "schedule", "enable", "daily")
	writeTest(t, filepath.Join(f.root, "hold"), nil, 0600)
	f.at(t, "2026-09-10T00:00:10Z")
	f.daemon(t)
	first := f.runAt(t, "daily", "2026-09-10T00:00:10Z")
	f.awaitAgent(t, first)
	before := f.call(t, "run", "show", first, "--json")["input"]
	verify(t, "V-01", func(t *testing.T) {
		f.at(t, "2026-09-10T00:00:30Z")
		v := f.observed(t, "daily", "2026-09-10T00:00:30Z")
		if v["latest_unaccepted_at"] != "2026-09-10T00:00:30Z" || v["skipped"] != float64(1) || f.count(t, "submissions") != 1 {
			t.Fatalf("pending: %v", v)
		}
		if !reflect.DeepEqual(before, f.call(t, "run", "show", first, "--json")["input"]) {
			t.Fatal("accepted input replaced")
		}
	})
	verify(t, "V-02", func(t *testing.T) {
		f.at(t, "2026-09-10T00:00:40Z")
		v := f.observed(t, "daily", "2026-09-10T00:00:40Z")
		if v["skipped"] != float64(2) || v["latest_unaccepted_at"] != "2026-09-10T00:00:40Z" {
			t.Fatalf("pending replacement counted incorrectly %v", v)
		}
		if e := os.Remove(filepath.Join(f.root, "hold")); e != nil {
			t.Fatal(e)
		}
		second := f.runAt(t, "daily", "2026-09-10T00:00:40Z")
		f.assertReport(t, first, "2026-09-10T00:00:00Z", "2026-09-10T00:00:10Z", []string{"1"})
		f.assertReport(t, second, "2026-09-10T00:00:30Z", "2026-09-10T00:00:40Z", []string{"3"})
		if f.count(t, "submissions") != 2 || f.show(t, "daily")["skipped"] != float64(2) {
			t.Fatal("unexpected catch-up submissions or double-counted skips")
		}
	})
}
func TestScenario_SC_17(t *testing.T) {
	verify(t, "V-01", func(t *testing.T) { scheduleCrash(t, "before_commit", 0) })
	verify(t, "V-02", func(t *testing.T) { scheduleCrash(t, "after_commit", 1) })
}
func scheduleCrash(t *testing.T, phase string, count int) {
	t.Helper()
	f := newSchedule(t, "2026-09-10T00:00:00Z")
	f.events(t, "2026-09-10T00:00:05Z")
	f.register(t, "daily", everySchedule("10s"))
	f.call(t, "schedule", "enable", "daily")
	f.at(t, "2026-09-10T00:00:10Z")
	barrier := filepath.Join(f.root, "barrier")
	if e := os.Mkdir(barrier, 0700); e != nil {
		t.Fatal(e)
	}
	writeTest(t, filepath.Join(barrier, phase), nil, 0600)
	t.Setenv("TODOABLE_TEST_SCHEDULE_BARRIER", barrier)
	stop := f.daemon(t)
	waitSchedule(t, func() bool { _, e := os.Stat(filepath.Join(barrier, phase+".reached")); return e == nil })
	stop(true)
	for _, table := range []string{"submissions", "runs", "scheduled_submissions"} {
		if n := f.count(t, table); n != count {
			t.Fatalf("%s count %d want %d", table, n, count)
		}
	}
	v := f.show(t, "daily")
	if count == 0 {
		if v["last_accepted_at"] != nil || v["observed_at"] != "2026-09-10T00:00:00Z" {
			t.Fatalf("partial cursor before commit: %v", v)
		}
	} else if v["last_accepted_at"] != "2026-09-10T00:00:10Z" || v["observed_at"] != "2026-09-10T00:00:10Z" {
		t.Fatalf("missing committed cursor: %v", v)
	}
	paths, e := filepath.Glob(filepath.Join(f.records, "*"))
	if e != nil || len(paths) != 0 {
		t.Fatalf("external work before initial schedule commit returns %v %v", paths, e)
	}
	if e = os.Remove(filepath.Join(barrier, phase)); e != nil {
		t.Fatal(e)
	}
	stop = f.daemon(t)
	run := f.runAt(t, "daily", "2026-09-10T00:00:10Z")
	f.assertReport(t, run, "2026-09-10T00:00:00Z", "2026-09-10T00:00:10Z", []string{"1"})
	stop(false)
	stop = f.daemon(t)
	time.Sleep(100 * time.Millisecond)
	stop(false)
	if f.count(t, "submissions") != 1 || f.count(t, "runs") != 1 || f.show(t, "daily")["last_accepted_at"] != "2026-09-10T00:00:10Z" {
		t.Fatal("restart duplicate or partial cursor")
	}
	f.assertReport(t, run, "2026-09-10T00:00:00Z", "2026-09-10T00:00:10Z", []string{"1"})
}
func TestScenario_SC_18(t *testing.T) {
	f := newSchedule(t, "2026-09-10T00:00:00Z")
	f.events(t, "2026-09-07T12:00:00Z", "2026-09-08T00:00:00Z", "2026-09-08T12:00:00Z", "2026-09-09T00:00:00Z")
	f.register(t, "daily", cronSchedule("0 0 * * *", "UTC"))
	var result map[string]any
	verify(t, "V-01", func(t *testing.T) {
		result = f.call(t, "schedule", "submit", "daily", "--at", "2026-09-09T00:00:00Z")
		duplicate := f.call(t, "schedule", "submit", "daily", "--at", "2026-09-09T02:00:00+02:00")
		if duplicate["deduplicated"] != true || duplicate["submission_id"] != result["submission_id"] {
			t.Fatalf("duplicate %v", duplicate)
		}
		f.daemon(t)
		f.assertReport(t, result["run_id"].(string), "2026-09-08T00:00:00Z", "2026-09-09T00:00:00Z", []string{"2", "3"})
	})
	verify(t, "V-02", func(t *testing.T) {
		f.rejected(t, 2, "validation_error", "schedule", "submit", "daily", "--at", "2026-09-09T00:00:01Z")
		duplicate := f.call(t, "schedule", "submit", "daily", "--at", "2026-09-09T00:00:00Z")
		if duplicate["deduplicated"] != true || duplicate["run_id"] != result["run_id"] || f.count(t, "submissions") != 1 {
			t.Fatal("invalid time created input or duplicate changed")
		}
		f.assertReport(t, result["run_id"].(string), "2026-09-08T00:00:00Z", "2026-09-09T00:00:00Z", []string{"2", "3"})
	})
}
func TestScenario_SC_19(t *testing.T) {
	verify(t, "V-01", func(t *testing.T) {
		f := newSchedule(t, "2026-03-07T08:00:00Z")
		f.events(t, "2026-03-07T07:30:00Z", "2026-03-08T07:30:00Z", "2026-03-09T06:30:00Z")
		f.register(t, "spring", cronSchedule("30 2 * * *", "America/New_York"))
		f.call(t, "schedule", "enable", "spring")
		f.at(t, "2026-03-08T08:00:00Z")
		f.daemon(t)
		v := f.observed(t, "spring", "2026-03-08T08:00:00Z")
		if v["last_accepted_at"] != nil || f.count(t, "submissions") != 0 {
			t.Fatalf("nonexistent 02:30 emitted: %v", v)
		}
		f.at(t, "2026-03-09T06:30:00Z")
		run := f.runAt(t, "spring", "2026-03-09T06:30:00Z")
		f.assertReport(t, run, "2026-03-07T07:30:00Z", "2026-03-09T06:30:00Z", []string{"1", "2"})
		if f.show(t, "spring")["skipped"] != float64(0) {
			t.Fatal("nonexistent local time counted as an opportunity")
		}
	})
	verify(t, "V-02", func(t *testing.T) {
		f := newSchedule(t, "2026-11-01T04:00:00Z")
		f.events(t, "2026-10-31T05:30:00Z", "2026-11-01T05:00:00Z", "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z")
		f.register(t, "fall", cronSchedule("30 1 * * *", "America/New_York"))
		f.call(t, "schedule", "enable", "fall")
		f.at(t, "2026-11-01T05:30:00Z")
		f.daemon(t)
		first := f.runAt(t, "fall", "2026-11-01T05:30:00Z")
		f.assertReport(t, first, "2026-10-31T05:30:00Z", "2026-11-01T05:30:00Z", []string{"1", "2"})
		f.at(t, "2026-11-01T06:30:00Z")
		second := f.runAt(t, "fall", "2026-11-01T06:30:00Z")
		f.assertReport(t, second, "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z", []string{"3"})
		f.at(t, "2026-11-01T05:00:00Z")
		time.Sleep(120 * time.Millisecond)
		if v := f.show(t, "fall"); v["observed_at"] != "2026-11-01T06:30:00Z" || v["last_accepted_at"] != "2026-11-01T06:30:00Z" || f.count(t, "submissions") != 2 {
			t.Fatalf("clock rewound: %v", v)
		}
		f.at(t, "2026-11-01T06:30:00Z")
		time.Sleep(100 * time.Millisecond)
		if f.count(t, "runs") != 2 {
			t.Fatal("equal timestamp issued duplicate")
		}
		f.at(t, "2026-11-01T07:00:00Z")
		f.observed(t, "fall", "2026-11-01T07:00:00Z")
		if f.count(t, "submissions") != 2 {
			t.Fatal("already accepted opportunities replayed")
		}
		f.assertReport(t, first, "2026-10-31T05:30:00Z", "2026-11-01T05:30:00Z", []string{"1", "2"})
		f.assertReport(t, second, "2026-11-01T05:30:00Z", "2026-11-01T06:30:00Z", []string{"3"})
		t.Run("pending_during_clock_rollback", checkPendingDuringClockRollback)
	})
}

func checkPendingDuringClockRollback(t *testing.T) {
	f := newSchedule(t, "2026-09-10T00:00:00Z")
	f.events(t, "2026-09-10T00:00:05Z", "2026-09-10T00:00:15Z", "2026-09-10T00:00:20Z", "2026-09-10T00:00:29Z", "2026-09-10T00:00:30Z")
	f.register(t, "daily", everySchedule("10s"))
	f.call(t, "schedule", "enable", "daily")
	writeTest(t, filepath.Join(f.root, "hold"), nil, 0600)
	f.at(t, "2026-09-10T00:00:10Z")
	f.daemon(t)
	first := f.runAt(t, "daily", "2026-09-10T00:00:10Z")
	f.awaitAgent(t, first)
	f.at(t, "2026-09-10T00:00:30Z")
	v := f.observed(t, "daily", "2026-09-10T00:00:30Z")
	if v["latest_unaccepted_at"] != "2026-09-10T00:00:30Z" || v["last_accepted_at"] != "2026-09-10T00:00:10Z" || v["skipped"] != float64(1) {
		t.Fatalf("pending opportunity before rollback: %v", v)
	}
	f.at(t, "2026-09-10T00:00:15Z")
	if e := os.Remove(filepath.Join(f.root, "hold")); e != nil {
		t.Fatal(e)
	}
	f.assertReport(t, first, "2026-09-10T00:00:00Z", "2026-09-10T00:00:10Z", []string{"1"})
	// Keep observing after the blocking run completes, including forward movement
	// that still falls short of the previously observed clock.
	for _, at := range []string{"2026-09-10T00:00:15Z", "2026-09-10T00:00:29Z"} {
		f.at(t, at)
		until := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(until) {
			v = f.show(t, "daily")
			if v["observed_at"] != "2026-09-10T00:00:30Z" || v["latest_unaccepted_at"] != "2026-09-10T00:00:30Z" || v["last_accepted_at"] != "2026-09-10T00:00:10Z" || v["skipped"] != float64(1) || f.count(t, "submissions") != 1 || f.count(t, "runs") != 1 {
				t.Fatalf("pending opportunity changed during clock rollback at %s: %v", at, v)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	f.at(t, "2026-09-10T00:00:30Z")
	second := f.runAt(t, "daily", "2026-09-10T00:00:30Z")
	f.assertReport(t, second, "2026-09-10T00:00:20Z", "2026-09-10T00:00:30Z", []string{"3", "4"})
	f.at(t, "2026-09-10T00:00:40Z")
	third := f.runAt(t, "daily", "2026-09-10T00:00:40Z")
	f.assertReport(t, third, "2026-09-10T00:00:30Z", "2026-09-10T00:00:40Z", []string{"5"})
	v = f.show(t, "daily")
	if v["observed_at"] != "2026-09-10T00:00:40Z" || v["last_accepted_at"] != "2026-09-10T00:00:40Z" || v["latest_unaccepted_at"] != nil || v["skipped"] != float64(1) || f.count(t, "submissions") != 3 || f.count(t, "runs") != 3 {
		t.Fatalf("clock recovery replayed or lost opportunities: %v", v)
	}
	f.assertReport(t, first, "2026-09-10T00:00:00Z", "2026-09-10T00:00:10Z", []string{"1"})
}

func TestScenario_SC_36(t *testing.T) {
	f := newSchedule(t, "2026-09-10T00:00:00Z")
	f.events(t, "2026-09-10T00:00:05Z", "2026-09-10T00:00:15Z", "2026-09-10T00:00:40Z", "2026-09-10T00:00:56Z")
	var first string
	verify(t, "V-01", func(t *testing.T) {
		for i, cron := range []string{"*/15 1-3 * * 1,5", "0 0 1 * 1", "0 0 29 2 *"} {
			f.register(t, fmt.Sprint("valid", i), cronSchedule(cron, "UTC"))
		}
		for i, cron := range []string{"* * * * * *", "@daily", "0 0 * JAN *", "0 0 L * *", "0 0 1W * *", "0 0 * * 1#2", "0 0 ? * *", "0 0 * * 7", "0 0 31 2 *", "*/0 * * * *", "60 * * * *"} {
			p := f.manifest(t, f.task(fmt.Sprint("invalid", i), cronSchedule(cron, "UTC")))
			f.rejected(t, 2, "validation_error", "task", "register", p)
		}
		for i, zone := range []string{"Local", "Bogus/Zone"} {
			p := f.manifest(t, f.task(fmt.Sprint("zone", i), cronSchedule("0 0 * * *", zone)))
			f.rejected(t, 2, "validation_error", "task", "register", p)
		}
		for i, every := range []string{"0s", "999ms", "8761h"} {
			p := f.manifest(t, f.task(fmt.Sprint("every", i), everySchedule(every)))
			f.rejected(t, 2, "validation_error", "task", "register", p)
		}
		f.register(t, "daily", everySchedule("10s"))
		if v := f.show(t, "daily"); v["enabled"] != false || f.count(t, "runs") != 0 {
			t.Fatalf("register enabled schedule: %v", v)
		}
		f.call(t, "schedule", "enable", "daily")
		writeTest(t, filepath.Join(f.root, "hold"), nil, 0600)
		f.at(t, "2026-09-10T00:00:09Z")
		f.daemon(t)
		f.observed(t, "daily", "2026-09-10T00:00:09Z")
		if f.count(t, "submissions") != 0 {
			t.Fatal("interval started before one period")
		}
		f.at(t, "2026-09-10T00:00:10Z")
		first = f.runAt(t, "daily", "2026-09-10T00:00:10Z")
		f.awaitAgent(t, first)
		f.at(t, "2026-09-10T00:00:25Z")
		before := f.observed(t, "daily", "2026-09-10T00:00:25Z")
		task := f.task("daily", everySchedule("10s"))
		task["prompt"] = "new prompt, same schedule"
		updated := f.call(t, "task", "update", f.manifest(t, task), "--if-version", "1")
		after := f.show(t, "daily")
		if updated["task_version"] != float64(2) || after["anchor"] != "2026-09-10T00:00:00Z" || after["observed_at"] != before["observed_at"] || after["latest_unaccepted_at"] != "2026-09-10T00:00:20Z" {
			t.Fatalf("same schedule reset: %v", after)
		}
		disabled := f.call(t, "schedule", "disable", "daily")
		if disabled["discarded"] != float64(1) || disabled["discard_reason"] != "disabled" || disabled["latest_unaccepted_at"] != nil {
			t.Fatalf("discard: %v", disabled)
		}
		f.at(t, "2026-09-10T00:00:35Z")
		time.Sleep(80 * time.Millisecond)
		if f.count(t, "submissions") != 1 {
			t.Fatal("disabled schedule emitted")
		}
		v := f.call(t, "schedule", "enable", "daily")
		if v["anchor"] != "2026-09-10T00:00:35Z" {
			t.Fatalf("reactivation anchor: %v", v)
		}
		f.at(t, "2026-09-10T00:00:36Z")
		task["schedule"] = everySchedule("20s")
		f.call(t, "task", "update", f.manifest(t, task), "--if-version", "2")
		v = f.show(t, "daily")
		if v["anchor"] != "2026-09-10T00:00:36Z" || v["enabled"] != true {
			t.Fatalf("changed schedule anchor: %v", v)
		}
		if e := os.Remove(filepath.Join(f.root, "hold")); e != nil {
			t.Fatal(e)
		}
		f.assertReport(t, first, "2026-09-10T00:00:00Z", "2026-09-10T00:00:10Z", []string{"1"})
		f.at(t, "2026-09-10T00:00:55Z")
		f.observed(t, "daily", "2026-09-10T00:00:55Z")
		if f.count(t, "submissions") != 1 {
			t.Fatal("changed interval started early")
		}
		f.at(t, "2026-09-10T00:00:56Z")
		second := f.runAt(t, "daily", "2026-09-10T00:00:56Z")
		f.assertReport(t, second, "2026-09-10T00:00:36Z", "2026-09-10T00:00:56Z", []string{"3"})
		if f.call(t, "run", "show", second, "--json")["task_version"] != float64(3) {
			t.Fatal("automatic submission used old version")
		}
		// Numeric day and weekday restrictions have OR semantics: Sept 7 is a
		// Monday but not the first, while Sept 1 is a Tuesday and the first.
		for _, period := range [][2]string{{"2026-08-31T00:00:00Z", "2026-09-01T00:00:00Z"}, {"2026-09-01T00:00:00Z", "2026-09-07T00:00:00Z"}} {
			result := f.call(t, "schedule", "submit", "valid1", "--at", period[1])
			f.assertReport(t, result["run_id"].(string), period[0], period[1], []string{})
		}
		f.rejected(t, 2, "validation_error", "schedule", "submit", "valid1", "--at", "2026-09-02T00:00:00Z")
		t.Run("wildcard-and-eight-year-boundaries", cronDateBoundaries)
	})
	verify(t, "V-02", func(t *testing.T) {
		f.daemon(t)
		var invalid int
		if e := f.db(t).QueryRow("SELECT count(*) FROM tasks WHERE id LIKE 'invalid%' OR id LIKE 'zone%' OR id LIKE 'every%'").Scan(&invalid); e != nil || invalid != 0 {
			t.Fatalf("invalid registrations %d %v", invalid, e)
		}
		old := f.call(t, "schedule", "submit", "daily", "--at", "2026-09-10T00:00:20Z", "--task-version", "1")
		f.assertReport(t, old["run_id"].(string), "2026-09-10T00:00:10Z", "2026-09-10T00:00:20Z", []string{"2"})
		if old["task_version"] != float64(1) {
			t.Fatal("old version lost")
		}
		f.rejected(t, 2, "validation_error", "schedule", "submit", "daily", "--at", "2026-09-10T00:00:30Z", "--task-version", "1")
		var versions, closed int
		if e := f.db(t).QueryRow("SELECT count(*) FROM task_versions WHERE task_id='daily'").Scan(&versions); e != nil {
			t.Fatal(e)
		}
		if e := f.db(t).QueryRow("SELECT count(*) FROM schedule_periods WHERE task_id='daily' AND end IS NOT NULL").Scan(&closed); e != nil {
			t.Fatal(e)
		}
		if versions != 3 || closed != 3 {
			t.Fatalf("history: versions %d closed %d", versions, closed)
		}
		f.call(t, "task", "disable", "daily")
		f.call(t, "task", "enable", "daily")
		if f.show(t, "daily")["enabled"] != false {
			t.Fatal("task enable re-enabled schedule")
		}
		before := f.count(t, "submissions")
		f.at(t, "2026-09-10T00:05:00Z")
		time.Sleep(100 * time.Millisecond)
		if f.count(t, "submissions") != before {
			t.Fatal("disabled schedule emitted after task enable")
		}
		f.assertReport(t, first, "2026-09-10T00:00:00Z", "2026-09-10T00:00:10Z", []string{"1"})
	})
}
func TestScenario_SC_37(t *testing.T) {
	verify(t, "V-01", func(t *testing.T) { scheduleEnvironment(t) })
	verify(t, "V-02", func(t *testing.T) { scheduleCapacity(t) })
}
func scheduleEnvironment(t *testing.T) {
	t.Helper()
	f := newSchedule(t, "2026-09-10T00:00:00Z")
	f.events(t, "2026-09-10T00:00:05Z", "2026-09-10T00:00:25Z")
	bad := f.task("bad", everySchedule("10s"))
	bad["inherit_env"] = []string{"SCHEDULE_REQUIRED"}
	f.call(t, "task", "register", f.manifest(t, bad))
	f.register(t, "good", everySchedule("10s"))
	f.register(t, "never", everySchedule("10s"))
	t.Setenv("SCHEDULE_REQUIRED", "")
	if e := os.Unsetenv("SCHEDULE_REQUIRED"); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"bad", "good"} {
		f.call(t, "schedule", "enable", id)
	}
	f.at(t, "2026-09-10T00:00:10Z")
	stop := f.daemon(t)
	v := f.observed(t, "bad", "2026-09-10T00:00:10Z")
	if v["latest_unaccepted_at"] != "2026-09-10T00:00:10Z" || v["last_error"] != "missing inherited environment: SCHEDULE_REQUIRED" || v["last_accepted_at"] != nil {
		t.Fatalf("environment error not retained: %v", v)
	}
	good := f.runAt(t, "good", "2026-09-10T00:00:10Z")
	f.assertReport(t, good, "2026-09-10T00:00:00Z", "2026-09-10T00:00:10Z", []string{"1"})
	f.at(t, "2026-09-10T00:00:30Z")
	v = f.observed(t, "bad", "2026-09-10T00:00:30Z")
	if v["skipped"] != float64(2) || v["latest_unaccepted_at"] != "2026-09-10T00:00:30Z" || v["last_error"] == "" {
		t.Fatalf("failed task stopped observation: %v", v)
	}
	good2 := f.runAt(t, "good", "2026-09-10T00:00:30Z")
	f.assertReport(t, good2, "2026-09-10T00:00:20Z", "2026-09-10T00:00:30Z", []string{"2"})
	stop(false)
	t.Setenv("SCHEDULE_REQUIRED", "restored")
	start := time.Now()
	f.daemon(t)
	recovered := f.runAt(t, "bad", "2026-09-10T00:00:30Z")
	f.assertReport(t, recovered, "2026-09-10T00:00:20Z", "2026-09-10T00:00:30Z", []string{"2"})
	if time.Since(start) >= 60*time.Second {
		t.Fatal("retry exceeded 60 seconds")
	}
	v = f.show(t, "bad")
	if v["last_error"] != "" || v["latest_unaccepted_at"] != nil || v["skipped"] != float64(2) {
		t.Fatalf("recovery cursor: %v", v)
	}
	for _, at := range []string{"2026-09-10T00:00:10", "2016-12-31T23:59:60Z", "2026-09-10T00:00:11Z", "2026-09-10T00:00:00Z", "2026-09-09T23:59:50Z"} {
		f.rejected(t, 2, "validation_error", "schedule", "submit", "bad", "--at", at)
	}
	f.rejected(t, 2, "validation_error", "schedule", "submit", "never", "--at", "2026-09-10T00:00:10Z")
	f.call(t, "schedule", "disable", "bad")
	f.at(t, "2026-09-10T00:00:35Z")
	f.call(t, "schedule", "enable", "bad")
	f.at(t, "2026-09-10T00:00:36Z")
	changed := f.task("bad", everySchedule("20s"))
	f.call(t, "task", "update", f.manifest(t, changed), "--if-version", "1")
	old := f.call(t, "schedule", "submit", "bad", "--at", "2026-09-10T00:00:20Z", "--task-version", "1")
	f.assertReport(t, old["run_id"].(string), "2026-09-10T00:00:10Z", "2026-09-10T00:00:20Z", []string{})
	f.rejected(t, 2, "validation_error", "schedule", "submit", "bad", "--at", "2026-09-10T00:00:40Z", "--task-version", "1")
	f.rejected(t, 2, "validation_error", "schedule", "submit", "bad", "--at", "2026-09-10T00:00:30Z", "--task-version", "2")
	f.rejected(t, 2, "validation_error", "schedule", "submit", "bad", "--at", "2026-09-10T00:00:35Z", "--task-version", "1")
}
func scheduleCapacity(t *testing.T) {
	t.Helper()
	f := newSchedule(t, "2026-09-10T00:00:00Z")
	f.events(t, "2026-09-10T00:00:05Z", "2026-09-10T00:00:25Z")
	filler := f.task("filler", nil)
	delete(filler, "schedule")
	f.call(t, "task", "register", f.manifest(t, filler))
	f.register(t, "daily", everySchedule("10s"))
	f.register(t, "never", everySchedule("10s"))
	var first map[string]any
	// Fill the actual default global limit through the public CLI. Each group
	// stays within the default per-key limit; no fixture inserts product rows.
	for i := 0; i < 1000; i++ {
		in := map[string]any{"task_id": "filler", "input_key": fmt.Sprint("key", i/100), "concurrency_key": fmt.Sprint("fill", i/100), "input": map[string]any{"nonce": i, "data": map[string]any{"report": "daily"}, "occurrence": map[string]any{"scheduled_at": "2026-09-10T00:00:10Z", "window_start": "2026-09-10T00:00:00Z", "window_end": "2026-09-10T00:00:10Z"}}}
		raw, e := json.Marshal(in)
		if e != nil {
			t.Fatal(e)
		}
		writeTest(t, f.input, raw, 0600)
		got := f.call(t, "run", "submit", f.input)
		if i == 0 {
			first = got
		}
	}
	firstID := first["run_id"].(string)
	before := f.call(t, "run", "show", firstID, "--json")["input"]
	writeTest(t, filepath.Join(f.root, "hold"), nil, 0600)
	f.call(t, "schedule", "enable", "daily")
	f.at(t, "2026-09-10T00:00:10Z")
	stop := f.daemon(t)
	f.awaitAgent(t, firstID)
	v := f.observed(t, "daily", "2026-09-10T00:00:10Z")
	if v["last_error"] != "pending submission limit reached" || v["latest_unaccepted_at"] != "2026-09-10T00:00:10Z" {
		t.Fatalf("capacity error: %v", v)
	}
	f.at(t, "2026-09-10T00:00:30Z")
	v = f.observed(t, "daily", "2026-09-10T00:00:30Z")
	if v["skipped"] != float64(2) || v["last_error"] != "pending submission limit reached" || v["latest_unaccepted_at"] != "2026-09-10T00:00:30Z" || f.count(t, "submissions") != 1000 {
		t.Fatalf("capacity pending %v", v)
	}
	if !reflect.DeepEqual(before, f.call(t, "run", "show", firstID, "--json")["input"]) {
		t.Fatal("queue-full observation overwrote accepted input")
	}
	// Release one real process while the remaining inputs stay gated. Its
	// completion frees one intake slot, which the same daemon must retry.
	writeTest(t, filepath.Join(f.root, "release-"+firstID), nil, 0600)
	waitSchedule(t, func() bool { return f.call(t, "run", "show", firstID, "--json")["state"] == "succeeded" })
	run := f.runAt(t, "daily", "2026-09-10T00:00:30Z")
	view := f.call(t, "run", "show", run, "--json")
	expected := map[string]any{"data": map[string]any{"report": "daily"}, "occurrence": map[string]any{"scheduled_at": "2026-09-10T00:00:30Z", "window_start": "2026-09-10T00:00:20Z", "window_end": "2026-09-10T00:00:30Z"}}
	if !reflect.DeepEqual(view["input"], expected) || f.count(t, "submissions") != 1001 || f.show(t, "daily")["last_error"] != "" {
		t.Fatalf("retry did not preserve latest window: %v", view)
	}
	var report struct {
		Rows []string `json:"rows"`
	}
	b, e := os.ReadFile(filepath.Join(f.records, firstID+".report"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &report); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(report.Rows, []string{"1"}) || !reflect.DeepEqual(before, f.call(t, "run", "show", firstID, "--json")["input"]) {
		t.Fatal("original report/input changed during saturation")
	}
	var records int
	paths, e := filepath.Glob(filepath.Join(f.records, "*.json"))
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range paths {
		var c map[string]any
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(b, &c); e != nil {
			t.Fatal(e)
		}
		if c["run_id"] == firstID {
			records++
			if !reflect.DeepEqual(c["input"], before) {
				t.Fatal("external process received replaced input")
			}
		}
	}
	if records != 3 {
		t.Fatalf("original external steps %d", records)
	}
	for _, at := range []string{"2026-09-10T00:00:10", "2016-12-31T23:59:60Z", "2026-09-10T00:00:11Z"} {
		f.rejected(t, 2, "validation_error", "schedule", "submit", "daily", "--at", at)
	}
	f.rejected(t, 2, "validation_error", "schedule", "submit", "never", "--at", "2026-09-10T00:00:10Z")
	if f.count(t, "submissions") != 1001 {
		t.Fatal("rejected manual period created input")
	}
	stop(false)
}

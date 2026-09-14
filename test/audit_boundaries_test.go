package test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func installSignalObserver(t *testing.T, f runtimeFixture) {
	t.Helper()
	observer := `if inp.get('signal_observer') and s=='before':
 def terminated(name):
  def handle(sig,frame):
   open(os.path.join(rd,name+'.term'),'w').write(str(sig))
   signal.signal(sig,signal.SIG_DFL);os.kill(os.getpid(),sig)
  return handle
 signal.signal(signal.SIGTERM,terminated('parent'))
 if os.fork()==0:
  signal.signal(signal.SIGTERM,terminated('child'))
  open(os.path.join(rd,'child.ready'),'w').write(str(os.getpid()))
  while os.path.isdir(rd):time.sleep(.01)
  sys.exit(0)
`
	writeTest(t, filepath.Join(f.root, "runner.py"), []byte(strings.Replace(orderScript, "sys.stdin.buffer.read()", observer+"sys.stdin.buffer.read()", 1)), 0700)
}

func awaitSignalObserver(t *testing.T, f runtimeFixture, runID string) func() {
	t.Helper()
	dir := filepath.Join(f.dir, "runs", runID)
	var child int
	waitUntil(t, func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "child.ready"))
		if err != nil {
			return false
		}
		child, err = strconv.Atoi(string(b))
		return err == nil && child > 0 && processExists(child)
	})
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	return func() {
		for _, name := range []string{"parent", "child"} {
			raw, err := os.ReadFile(filepath.Join(dir, name+".term"))
			if err != nil || string(raw) != "15" {
				t.Fatalf("%s did not observe group SIGTERM: %q %v", name, raw, err)
			}
		}
		if processExists(child) {
			t.Fatal("group child remained alive after shutdown")
		}
	}
}

func cronDateBoundaries(t *testing.T) {
	f := newSchedule(t, "2096-02-29T00:00:00Z")
	f.events(t, "2096-02-29T00:00:00Z", "2104-02-28T23:59:59Z", "2104-02-29T00:00:00Z")
	// 2100 is not a leap year. Exactly eight years must remain searchable,
	// both when accepting the definition and reconstructing the prior window.
	f.register(t, "century-leap", cronSchedule("0 0 29 2 *", "UTC"))
	f.call(t, "schedule", "enable", "century-leap")
	f.daemon(t)
	f.observed(t, "century-leap", "2096-02-29T00:00:00Z")
	f.at(t, "2104-02-29T00:00:00Z")
	run := f.runAt(t, "century-leap", "2104-02-29T00:00:00Z")
	f.assertReport(t, run, "2096-02-29T00:00:00Z", "2104-02-29T00:00:00Z", []string{"1", "2"})
	// A sparse active schedule must not monopolize the write transaction on
	// every poll. Unrelated registration, admission, observation and execution
	// must still progress while the leap schedule remains active.
	f.register(t, "unrelated", cronSchedule("0 0 * * *", "UTC"))
	other := f.call(t, "schedule", "submit", "unrelated", "--at", "2104-02-28T00:00:00Z")
	f.assertReport(t, other["run_id"].(string), "2104-02-27T00:00:00Z", "2104-02-28T00:00:00Z", []string{})
	f.call(t, "schedule", "disable", "century-leap")
	for _, tc := range []struct{ id, cron, at, previous, rejected string }{
		{"weekday-only", "0 0 * * 1", "2026-09-07T00:00:00Z", "2026-08-31T00:00:00Z", "2026-09-08T00:00:00Z"},
		{"monthday-only", "0 0 1 * *", "2026-09-01T00:00:00Z", "2026-08-01T00:00:00Z", "2026-09-02T00:00:00Z"},
	} {
		f.register(t, tc.id, cronSchedule(tc.cron, "UTC"))
		admitted := f.call(t, "schedule", "submit", tc.id, "--at", tc.at)
		f.assertReport(t, admitted["run_id"].(string), tc.previous, tc.at, []string{})
		before := f.count(t, "submissions")
		f.rejected(t, 2, "validation_error", "schedule", "submit", tc.id, "--at", tc.rejected)
		if f.count(t, "submissions") != before {
			t.Fatal("wildcard rejection created a submission")
		}
	}
	t.Run("sparse-schedule-admission-observation", sparseScheduleObservation)
}

func sparseScheduleObservation(t *testing.T) {
	f := newSchedule(t, "2096-02-29T00:00:00Z")
	f.events(t)
	// Several legal sparse schedules keep the maintenance loop scanning its
	// full range. Check repeated daemon observations, not one lucky lock gap.
	for i := range 8 {
		id := fmt.Sprintf("sparse-%d", i)
		f.register(t, id, cronSchedule("0 0 29 2 *", "UTC"))
		f.call(t, "schedule", "enable", id)
	}
	f.register(t, "probe", cronSchedule("0 0 * * *", "UTC"))
	f.daemon(t)
	f.observed(t, "sparse-7", "2096-02-29T00:00:00Z")
	db := f.db(t)
	for day := 20; day < 28; day++ {
		at := fmt.Sprintf("2096-02-%02dT00:00:00Z", day)
		before := fmt.Sprintf("2096-02-%02dT00:00:00Z", day-1)
		receipt := f.call(t, "schedule", "submit", "probe", "--at", at)
		acknowledged := time.Now()
		observed := false
		for time.Since(acknowledged) < time.Second {
			var n int
			// Only the daemon's scheduler populates run_schedule. Its row
			// proves the input change was seen, independent of Run/check slots
			// and process startup/completion latency.
			if err := db.QueryRow("SELECT count(*) FROM run_schedule WHERE run_id=?", receipt["run_id"]).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 1 {
				observed = time.Since(acknowledged) <= time.Second
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !observed {
			t.Fatalf("daemon did not observe acknowledged input %s within one second during sparse schedule scans", at)
		}
		f.assertReport(t, receipt["run_id"].(string), before, at, []string{})
	}
	var sparse int
	if err := db.QueryRow("SELECT count(*) FROM submissions WHERE task_id LIKE 'sparse-%'").Scan(&sparse); err != nil || sparse != 0 {
		t.Fatalf("sparse schedules emitted an activation-time occurrence: count=%d err=%v", sparse, err)
	}
}

func sqliteCLIDurability(t *testing.T) {
	f := orderFixture(t, 0, "0s", "0s")
	db := f.database(t)
	storageSQL(t, db, "CREATE TABLE observed_connection_pragmas(source TEXT, journal TEXT, sync INTEGER, fk INTEGER, busy INTEGER)")
	// These table-valued PRAGMAs execute inside each product connection's write,
	// not on the harness connection used to inspect the resulting observations.
	for _, trigger := range []string{
		"CREATE TRIGGER observe_cli_connection AFTER INSERT ON submissions BEGIN INSERT INTO observed_connection_pragmas SELECT NEW.input_key, (SELECT journal_mode FROM pragma_journal_mode), (SELECT synchronous FROM pragma_synchronous), (SELECT foreign_keys FROM pragma_foreign_keys), (SELECT timeout FROM pragma_busy_timeout); END",
		"CREATE TRIGGER observe_daemon_connection AFTER INSERT ON steps BEGIN INSERT INTO observed_connection_pragmas SELECT 'daemon', (SELECT journal_mode FROM pragma_journal_mode), (SELECT synchronous FROM pragma_synchronous), (SELECT foreign_keys FROM pragma_foreign_keys), (SELECT timeout FROM pragma_busy_timeout); END",
	} {
		storageSQL(t, db, trigger)
	}
	gate := filepath.Join(f.root, "release")
	submitOrder(t, f, map[string]any{"label": "first", "before_gate": gate}, "cli-one")
	submitOrder(t, f, map[string]any{"label": "second"}, "cli-two")
	storageDaemon(t, f)
	waitExternal(t, f, "first", "before", 1)
	for _, source := range []string{"cli-one", "cli-two", "daemon"} {
		var count, bad int
		if err := db.QueryRow("SELECT count(*),coalesce(sum(journal!='wal' OR sync!=2 OR fk!=1 OR busy!=5000),0) FROM observed_connection_pragmas WHERE source=?", source).Scan(&count, &bad); err != nil || count == 0 || bad != 0 {
			t.Fatalf("actual %s connection durability: observations=%d invalid=%d err=%v", source, count, bad, err)
		}
	}
}

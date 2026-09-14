package test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
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
	f.at(t, "2104-02-29T00:00:00Z")
	admitted := f.call(t, "schedule", "submit", "century-leap", "--at", "2104-02-29T00:00:00Z")
	f.daemon(t)
	f.assertReport(t, admitted["run_id"].(string), "2096-02-29T00:00:00Z", "2104-02-29T00:00:00Z", []string{"1", "2"})
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

package test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const reportImage = "chrislusf/seaweedfs@sha256:186de7ef977a20343ee9a5544073f081976a29e2d29ecf8379891e7bf177fbe9"
const reportSourceHash = "7918f9003306bd9ed0a3634dba9956e318c4d6bba582833eadacb50b9c890f25"

type reportDestination struct {
	endpoint, access, secret string
	mu                       sync.Mutex
	puts                     map[string]int
}

func reportCommand(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	b, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("report executable %s: %v\n%s", name, err, b)
	}
	return b
}
func reportRandom(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(t.TempDir()))
	return hex.EncodeToString(sum[:16])
}
func reportS3(t *testing.T) *reportDestination {
	t.Helper()
	d := &reportDestination{access: reportRandom(t), secret: reportRandom(t), puts: map[string]int{}}
	name := "todoable-report-" + reportRandom(t)
	reportCommand(t, "docker", "image", "inspect", reportImage)
	reportCommand(t, "docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::8333", "-e", "AWS_ACCESS_KEY_ID="+d.access, "-e", "AWS_SECRET_ACCESS_KEY="+d.secret, "-e", "S3_BUCKET=todoable-reports", reportImage, "mini", "-dir=/data", "-ip=127.0.0.1")
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := exec.Command("docker", "logs", "--tail", "50", name).CombinedOutput()
			t.Log(strings.NewReplacer(d.access, "<access>", d.secret, "<secret>").Replace(string(b)))
		}
		if b, e := exec.Command("docker", "rm", "-f", "-v", name).CombinedOutput(); e != nil {
			t.Errorf("test S3 cleanup: %v %s", e, b)
		}
	})
	address := strings.TrimSpace(string(reportCommand(t, "docker", "port", name, "8333/tcp")))
	if !strings.HasPrefix(address, "127.0.0.1:") {
		t.Fatalf("nonlocal S3 binding: %s", address)
	}
	target, err := url.Parse("http://" + address)
	if err != nil {
		t.Fatal(err)
	}
	// A transparent observer: every response comes from the real S3 server.
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == "PUT" && r.StatusCode >= 200 && r.StatusCode < 300 {
			d.mu.Lock()
			d.puts[r.Request.URL.Path]++
			d.mu.Unlock()
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	d.endpoint = server.URL
	var lastHead []byte
	ready := false
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		cmd := exec.Command("/usr/bin/curl", append(d.curlArgs(), "--fail", "-X", "PUT", d.endpoint+"/todoable-reports")...)
		var err error
		lastHead, err = cmd.CombinedOutput()
		if err == nil {
			ready = true
			break
		}
	}
	if !ready {
		direct, _ := exec.Command("/usr/bin/curl", append(d.curlArgs(), "-I", target.String()+"/todoable-reports")...).CombinedOutput()
		logs, _ := exec.Command("docker", "logs", "--tail", "20", name).CombinedOutput()
		clean := strings.NewReplacer(d.access, "<access>", d.secret, "<secret>").Replace(string(logs))
		t.Fatalf("actual S3 readiness failed proxy=%s direct=%s service=%s", lastHead, direct, clean)
	}

	head := string(reportCommand(t, "/usr/bin/curl", append(d.curlArgs(), "--fail", "-I", d.endpoint+"/todoable-reports")...))
	if !strings.Contains(head, "SeaweedFS") || !strings.Contains(head, "4.17") {
		t.Fatalf("unexpected actual destination %s", head)
	}
	// Bucket metadata can respond before the master's volume warmup ends.
	// Prove actual object storage is writable and readable before starting a Run.
	probe := filepath.Join(t.TempDir(), "probe")
	payload := strings.Repeat("S3 readiness data\n", 128)
	writeTest(t, probe, []byte(payload), 0600)
	args := append(d.curlArgs(), "--max-time", "60", "--fail", "-T", probe, d.endpoint+"/todoable-reports/readiness")
	reportCommand(t, "/usr/bin/curl", args...)
	args[len(args)-1] = d.endpoint + "/todoable-reports/readiness-second"
	reportCommand(t, "/usr/bin/curl", args...)
	got := reportCommand(t, "/usr/bin/curl", append(d.curlArgs(), "--fail", d.endpoint+"/todoable-reports/readiness")...)
	if string(got) != payload {
		t.Fatal("actual S3 readiness object differs")
	}
	return d
}
func (d *reportDestination) curlArgs() []string {
	return []string{"--silent", "--show-error", "--max-time", "10", "--aws-sigv4", "aws:amz:us-east-1:s3", "--user", d.access + ":" + d.secret}
}
func (d *reportDestination) count(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.puts["/todoable-reports/"+key+".json"]
}
func reportFixture(t *testing.T, d *reportDestination, escape bool) runtimeFixture {
	t.Helper()
	root := t.TempDir()
	f := runtimeFixture{intakeFixture: intakeFixture{bin: filepath.Join(root, "todoable"), dir: filepath.Join(root, "data"), input: filepath.Join(root, "input.json")}, root: root, records: filepath.Join(root, "records"), manifest: filepath.Join(root, "task.json")}
	for _, dir := range []string{f.records, f.dir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "build", "-race", "-o", f.bin, "./cmd/todoable")
	cmd.Dir = ".."
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	assets, e := filepath.Abs("testdata/report")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(assets, "commit-times.txt"))
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != reportSourceHash {
		t.Fatal("fixed actual history changed")
	}
	source := filepath.Join(root, "commit-times.txt")
	writeTest(t, source, raw, 0444)
	auth := filepath.Join(root, "s3-auth.json")
	b, _ := json.Marshal(map[string]string{"access": d.access, "secret": d.secret})
	writeTest(t, auth, b, 0600)
	data := map[string]any{"source": source, "source_sha256": reportSourceHash, "endpoint": d.endpoint, "auth_file": auth, "records": f.records, "hold_end": "2026-09-11T05:33:00Z", "release": filepath.Join(root, "release"), "child_pid": filepath.Join(root, "child-pid"), "escape": escape}
	f.task = map[string]any{"version": 1, "id": "report", "workdir": root, "prompt": "Aggregate exact half-open commit periods and publish verified report bytes", "agent": []string{"/usr/bin/python3", filepath.Join(assets, "report.py")}, "finish": map[string]any{"check": []string{"/usr/bin/python3", filepath.Join(assets, "report.py")}, "max_calls": 1}, "after": []string{"/usr/bin/python3", filepath.Join(assets, "publish.py")}, "schedule": map[string]any{"cron": "* * * * *", "timezone": "UTC", "input_key": "period", "concurrency_key": "report-destination", "input": data}}
	f.register(t)
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(root, "release"), nil, 0600) })
	return f
}
func reportSubmit(t *testing.T, f runtimeFixture, at string) map[string]any {
	return f.call(t, "schedule", "submit", "report", "--at", at)
}
func reportKey(task, start, end string) string {
	sum := sha256.Sum256([]byte(task + "\n" + reportSourceHash + "\n" + start + "\n" + end))
	return hex.EncodeToString(sum[:])
}
func assertReportDestination(t *testing.T, f runtimeFixture, d *reportDestination, task, start, end string) string {
	t.Helper()
	key := reportKey(task, start, end)
	body := reportCommand(t, "/usr/bin/curl", append(d.curlArgs(), "--fail", d.endpoint+"/todoable-reports/"+key+".json")...)
	headers := string(reportCommand(t, "/usr/bin/curl", append(d.curlArgs(), "--fail", "-I", d.endpoint+"/todoable-reports/"+key+".json")...))
	if !strings.Contains(strings.ToLower(headers), "x-amz-meta-effect-key: "+key) {
		t.Fatalf("missing destination effect metadata %s", headers)
	}
	first, e := time.Parse(time.RFC3339Nano, start)
	if e != nil {
		t.Fatal(e)
	}
	last, e := time.Parse(time.RFC3339Nano, end)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(f.root, "commit-times.txt"))
	if e != nil {
		t.Fatal(e)
	}
	expected := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Fields(line)
		at, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil {
			t.Fatal(e)
		}
		if at >= first.Unix() && at < last.Unix() {
			expected = append(expected, parts[0])
		}
	}
	sort.Strings(expected)
	var got struct {
		Start   string   `json:"window_start"`
		End     string   `json:"window_end"`
		Source  string   `json:"source_sha256"`
		Commits []string `json:"commits"`
		Count   int      `json:"count"`
	}
	if e = json.Unmarshal(body, &got); e != nil {
		t.Fatal(e)
	}
	if got.Start != start || got.End != end || got.Source != reportSourceHash || got.Count != len(expected) || !reflect.DeepEqual(got.Commits, expected) {
		t.Fatalf("actual published window differs: %s expected %v", body, expected)
	}
	if d.count(key) != 1 {
		t.Fatalf("effect published %d times", d.count(key))
	}
	return key
}
func reportNormal(t *testing.T, f runtimeFixture, d *reportDestination, at string) map[string]any {
	t.Helper()
	a := reportSubmit(t, f, at)
	v := f.await(t, a["run_id"].(string))
	if v["stage"] != "succeeded" || v["calls_used"] != float64(1) {
		t.Fatalf("real report failed %v", v)
	}
	end, _ := time.Parse(time.RFC3339, at)
	assertReportDestination(t, f, d, "report", end.Add(-time.Minute).Format(time.RFC3339), at)
	sameSubmission(t, a, reportSubmit(t, f, at))
	return v
}
func realReportFlow(t *testing.T, escape bool) {
	d := reportS3(t)
	f := reportFixture(t, d, escape)
	daemon := crashDaemon(t, f)
	reportNormal(t, f, d, "2026-09-11T05:32:00Z")
	a := reportSubmit(t, f, "2026-09-11T05:33:00Z")
	id := a["run_id"].(string)
	key := reportKey("report", "2026-09-11T05:32:00Z", "2026-09-11T05:33:00Z")
	waitUntil(t, func() bool {
		if d.count(key) == 1 {
			return true
		}
		v := f.call(t, "run", "show", id, "--json")
		if v["state"] == "failed" || v["state"] == "blocked" {
			t.Fatalf("publication failed before observation: %v", v)
		}
		return false
	})
	assertReportDestination(t, f, d, "report", "2026-09-11T05:32:00Z", "2026-09-11T05:33:00Z")
	var blocked map[string]any
	if !escape {
		killDaemon(t, daemon)
		daemon = crashDaemon(t, f)
	}
	blocked = f.await(t, id)
	if escape {
		// Recover the persisted untracked-process boundary as well as the lost
		// result boundary exercised by V-01.
		killDaemon(t, daemon)
		daemon = crashDaemon(t, f)
		blocked = f.await(t, id)
	}
	want := "blocked:outcome_unknown"
	if escape {
		want = "blocked:process_unknown"
	}
	if blocked["stage"] != want {
		t.Fatalf("publication recovery %v", blocked)
	}
	next := submitCore(t, f, "report", "independent-period", "report-destination", map[string]any{
		"data":       f.task["schedule"].(map[string]any)["input"],
		"occurrence": map[string]any{"scheduled_at": "2026-09-11T05:34:00Z", "window_start": "2026-09-11T05:33:00Z", "window_end": "2026-09-11T05:34:00Z"},
	})
	nextID := next["run_id"].(string)
	waitReady(t, f, nextID)
	time.Sleep(150 * time.Millisecond)
	nextView := f.call(t, "run", "show", nextID, "--json")
	if len(nextView["steps"].([]any)) != 0 || d.count(key) != 1 {
		t.Fatalf("unknown publication replayed or passed resource %v", nextView)
	}
	if escape {
		var pid int
		waitUntil(t, func() bool {
			b, e := os.ReadFile(filepath.Join(f.root, "child-pid"))
			if e != nil {
				return false
			}
			pid, e = strconv.Atoi(string(b))
			return e == nil && processExists(pid)
		})
		f.call(t, cancelArgs(a["submission_id"].(string), false, false)...)
		f.call(t, cancelArgs(a["submission_id"].(string), true, false)...)
		v := f.call(t, "run", "show", id, "--json")
		if v["stage"] != "blocked:process_unknown" || v["cancel_requested"] != true || !processExists(pid) || d.count(key) != 1 {
			t.Fatalf("effect acknowledgement replaced process stopping %v", v)
		}
		f.rejected(t, 6, "process_unknown", resumeArgs(id, currentStep(t, v), "confirm-success")...)
		writeTest(t, filepath.Join(f.root, "release"), nil, 0600)
		waitUntil(t, func() bool { return !processExists(pid) })
		f.call(t, cancelArgs(a["submission_id"].(string), true, true)...)
		v = f.call(t, "run", "show", id, "--json")
		if v["state"] != "cancelled" || v["effects_unknown"] != true || v["calls_used"] != float64(1) {
			t.Fatalf("acknowledged actual publication not settled %v", v)
		}
		audits := v["cancellations"].([]any)
		last := audits[len(audits)-1].(map[string]any)
		if last["acknowledge_effects"] != true || last["processes_stopped"] != true || last["reason"] != "destination checked; stop this input" {
			t.Fatalf("missing explicit cancellation evidence %v", audits)
		}
	} else {
		f.call(t, resumeArgs(id, currentStep(t, blocked), "confirm-success")...)
		v := f.call(t, "run", "show", id, "--json")
		if v["state"] != "succeeded" || v["calls_used"] != float64(1) {
			t.Fatalf("destination confirmation did not succeed %v", v)
		}

	}
	v := f.await(t, nextID)
	if v["state"] != "succeeded" {
		t.Fatalf("resource successor failed %v", v)
	}
	assertReportDestination(t, f, d, "report", "2026-09-11T05:33:00Z", "2026-09-11T05:34:00Z")
	assertReportDestination(t, f, d, "report", "2026-09-11T05:32:00Z", "2026-09-11T05:33:00Z")
	sameSubmission(t, a, reportSubmit(t, f, "2026-09-11T05:33:00Z"))
	// Reinvoke the actual publisher using its stored context: destination GET/HEAD
	// must suppress a second PUT even outside the engine's duplicate protection.
	if !escape {
		steps := v["steps"].([]any)
		after := steps[len(steps)-1].(map[string]any)["step_id"].(string)
		contextPath := filepath.Join(f.dir, "runs", nextID, "steps", after, "context.json")
		script, _ := filepath.Abs("testdata/report/publish.py")
		cmd := exec.Command("/usr/bin/python3", script)
		cmd.Env = append(os.Environ(), "TODOABLE_CONTEXT_PATH="+contextPath)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("real publisher retry: %v %s", e, b)
		}
		assertReportDestination(t, f, d, "report", "2026-09-11T05:33:00Z", "2026-09-11T05:34:00Z")
		realPeriodicReport(t, f, d)
	}
	_ = daemon
}
func realPeriodicReport(t *testing.T, f runtimeFixture, d *reportDestination) {
	t.Helper()
	task := map[string]any{}
	for k, v := range f.task {
		task[k] = v
	}
	task["id"] = "tick-report"
	data := f.task["schedule"].(map[string]any)["input"]
	task["schedule"] = map[string]any{"every": "1s", "input_key": "tick", "concurrency_key": "ticks", "input": data}
	raw, _ := json.Marshal(task)
	path := filepath.Join(f.root, "tick.json")
	writeTest(t, path, raw, 0600)
	f.call(t, "task", "register", path)
	enabledBefore := time.Now()
	f.call(t, "schedule", "enable", "tick-report")
	enabledAfter := time.Now()
	var id string
	waitUntil(t, func() bool {
		return f.database(t).QueryRow("SELECT r.id FROM runs r JOIN submissions s ON s.id=r.submission_id WHERE s.task_id='tick-report' ORDER BY s.seq LIMIT 1").Scan(&id) == nil
	})
	f.call(t, "schedule", "disable", "tick-report")
	v := f.await(t, id)
	if v["state"] != "succeeded" {
		t.Fatalf("real clock publication %v", v)
	}
	occ := v["input"].(map[string]any)["occurrence"].(map[string]any)
	start, e := time.Parse(time.RFC3339Nano, occ["window_start"].(string))
	if e != nil {
		t.Fatal(e)
	}
	end, e := time.Parse(time.RFC3339Nano, occ["window_end"].(string))
	if e != nil {
		t.Fatal(e)
	}
	if start.Before(enabledBefore) || start.After(enabledAfter) || end.Sub(start) != time.Second || v["scheduled_at"] != occ["window_end"] {
		t.Fatalf("periodic window does not match independently timed enable: %v between %v and %v", occ, enabledBefore, enabledAfter)
	}
	assertReportDestination(t, f, d, "tick-report", start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
}
func TestScenario_SC_30(t *testing.T) {
	if os.Getenv("E2E_DEEP") != "1" {
		t.Skip("L3 actual SQLite aggregation and SeaweedFS S3 publication requires scripts/verify.sh --deep")
	}
	verify(t, "V-01", func(t *testing.T) { realReportFlow(t, false) })
	verify(t, "V-02", func(t *testing.T) { realReportFlow(t, true) })
}

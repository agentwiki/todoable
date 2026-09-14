package test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScenario_SC_29(t *testing.T) {
	if os.Getenv("E2E_DEEP") != "1" {
		t.Skip("L3 real ImageMagick/Pillow conversion requires scripts/verify.sh --deep")
	}
	verify(t, "V-01", func(t *testing.T) { realConversion(t, false) })
	verify(t, "V-02", func(t *testing.T) { realConversion(t, true) })
}

func realCommand(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	b, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("real executable %s failed: %v\n%s", name, err, b)
	}
	return b
}

func realFixture(t *testing.T) runtimeFixture {
	t.Helper()
	root := t.TempDir()
	f := runtimeFixture{intakeFixture: intakeFixture{bin: filepath.Join(root, "todoable"), dir: filepath.Join(root, "data"), input: filepath.Join(root, "input.json")}, root: root, manifest: filepath.Join(root, "task.json"), records: filepath.Join(root, "records")}
	for _, dir := range []string{f.dir, f.records, filepath.Join(root, "outputs"), filepath.Join(root, "sources")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	build := exec.Command("go", "build", "-race", "-o", f.bin, "./cmd/todoable")
	build.Dir = ".."
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	return f
}

func realHash(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func conversionRecords(t *testing.T, f runtimeFixture) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.records, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	result := []map[string]any{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var item map[string]any
		if err := json.Unmarshal(b, &item); err != nil {
			t.Fatal(err)
		}
		result = append(result, item)
	}
	return result
}

func realConversion(t *testing.T, withFailure bool) {
	if b := realCommand(t, "/usr/bin/magick", "-version"); !strings.Contains(string(b), "ImageMagick 7.1.1-43 Q16") {
		t.Fatalf("converter version differs from frozen corpus: %s", b)
	}
	if b := realCommand(t, "/usr/bin/python3", "-c", "import PIL; print(PIL.__version__)"); string(b) != "11.1.0\n" {
		t.Fatalf("independent decoder version differs: %s", b)
	}
	f := realFixture(t)
	assets, err := filepath.Abs("testdata/conversion")
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(f.dir, "config.yaml"), []byte("max_running_runs: 2\nmax_check_processes: 2\n"), 0600)
	f.task = map[string]any{"version": 1, "id": "convert", "workdir": f.root, "prompt": "Convert the immutable source to exact lossless WebP", "agent": []string{"/usr/bin/python3", filepath.Join(assets, "convert.py")}, "finish": map[string]any{"check": []string{"/usr/bin/python3", filepath.Join(assets, "check.py")}, "max_calls": 1}}
	f.register(t)
	names := []string{"git-logo.png", "htop.png", "pngtest.png"}
	if withFailure {
		names = append([]string{"corrupt.png"}, names...)
	}
	requests := []map[string]any{}
	admitted := []map[string]any{}
	for _, name := range names {
		source := filepath.Join(f.root, "sources", name)
		data := []byte("not a valid PNG\n")
		if name != "corrupt.png" {
			data, err = os.ReadFile(filepath.Join(assets, name))
			if err != nil {
				t.Fatal(err)
			}
		}
		writeTest(t, source, data, 0444)
		input := map[string]any{"source": source, "source_sha256": realHash(t, source), "format": "webp", "lossless": true, "exact": true, "output": filepath.Join(f.root, "outputs", name+".webp"), "records": f.records, "release": filepath.Join(f.root, "release")}
		requests = append(requests, input)
		admitted = append(admitted, submitCore(t, f, "convert", name, input["output"].(string), input))
	}
	realCommand(t, "/usr/bin/python3", filepath.Join(assets, "oracle.py"), filepath.Join(assets, "manifest.json"), filepath.Join(f.root, "sources"))
	stop := configDaemon(t, f)
	converterPath, err := filepath.EvalSymlinks("/usr/bin/magick")
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return len(conversionRecords(t, f)) >= 2 })
	// Both real ImageMagick processes are alive awaiting source bytes. A third
	// invocation would violate the cap before the harness releases any bytes.
	for deadline := time.Now().Add(250 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		records := conversionRecords(t, f)
		if len(records) != 2 {
			t.Fatalf("global cap 2 admitted %d real converters", len(records))
		}
		for _, record := range records {
			pid := int(record["pid"].(float64))
			exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
			if err != nil || exe != converterPath {
				t.Fatalf("real converter not alive: pid=%d exe=%s err=%v", pid, exe, err)
			}
		}
	}
	writeTest(t, filepath.Join(f.root, "release"), nil, 0600)
	for index, receipt := range admitted {
		view := f.await(t, receipt["run_id"].(string))
		want := "succeeded"
		if names[index] == "corrupt.png" {
			want = "failed:max_calls"
		}
		if view["stage"] != want || view["calls_used"] != float64(1) || view["task_version"] != float64(1) || !reflect.DeepEqual(view["input"], requests[index]) {
			t.Fatalf("source outcome/version not preserved: %v", view)
		}
		steps := view["steps"].([]any)
		if len(steps) != 3 || steps[0].(map[string]any)["stage"] != "finish_check" || steps[1].(map[string]any)["stage"] != "agent" || steps[2].(map[string]any)["stage"] != "finish_check" {
			t.Fatalf("missing real conversion/check history: %v", steps)
		}
		if want != "succeeded" {
			step := steps[1].(map[string]any)
			result := step["result"].(map[string]any)
			if result["kind"] != "exited" || result["exit_code"] != float64(1) {
				t.Fatal("corrupt source did not retain converter failure", result)
			}
			log, err := os.ReadFile(filepath.Join(f.dir, "runs", receipt["run_id"].(string), "steps", step["step_id"].(string), "stderr"))
			if err != nil || !strings.Contains(string(log), "improper image header") {
				t.Fatalf("converter failure log missing: %v %s", err, log)
			}
		}
	}
	realCommand(t, "/usr/bin/python3", filepath.Join(assets, "oracle.py"), filepath.Join(assets, "manifest.json"), filepath.Join(f.root, "sources"), filepath.Join(f.root, "outputs"))
	before := f.durableAdmission(t)
	artifacts := map[string]string{}
	for _, name := range []string{"git-logo.png", "htop.png", "pngtest.png"} {
		path := filepath.Join(f.root, "outputs", name+".webp")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		artifacts[path] = realHash(t, path) + info.ModTime().String()
	}
	for index, input := range requests {
		sameSubmission(t, admitted[index], submitCore(t, f, "convert", names[index], input["output"].(string), input))
	}
	time.Sleep(1100 * time.Millisecond)
	if before != f.durableAdmission(t) || len(conversionRecords(t, f)) != len(names) {
		t.Fatal("retransmission changed durable history or started another converter")
	}
	for path, expected := range artifacts {
		info, err := os.Stat(path)
		if err != nil || realHash(t, path)+info.ModTime().String() != expected {
			t.Fatal("retransmission rewrote output", path, err)
		}
	}
	stop()
}

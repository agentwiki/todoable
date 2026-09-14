package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestScenario_SC_28(t *testing.T) {
	if os.Getenv("E2E_DEEP") != "1" {
		t.Skip("L3 real Forgejo webhooks and authenticated Codex execution require scripts/verify.sh --deep")
	}
	verify(t, "V-01", func(t *testing.T) { realIssues(t, "events") })
	verify(t, "V-02", func(t *testing.T) { realIssues(t, "boundaries") })
}

func realIssues(t *testing.T, mode string) {
	f := issuesFixture(t)
	assets, err := filepath.Abs("testdata/issues")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/python3", filepath.Join(assets, "workflow.py"), f.bin, f.root, mode)
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real issue workflow: %v\n%s", err, output)
	} else {
		t.Logf("%s", output)
	}
}

func issuesFixture(t *testing.T) runtimeFixture {
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

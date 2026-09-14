package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnfinishedOutputKeepsChunksAndAttribution(t *testing.T) {
	var input bytes.Buffer
	enc := json.NewEncoder(&input)
	for _, e := range []event{
		{Action: "run", Package: "a", Test: "TestSame"},
		{Action: "run", Package: "b", Test: "TestSame"},
		{Action: "output", Package: "a", Test: "TestSame", Output: "panic: broken\n\ngoroutine 7 [running]:\n"},
		{Action: "output", Package: "b", Test: "TestSame", Output: "pass noise\n"},
		{Action: "output", Package: "a", Test: "TestSame", Output: "example.TestSame()\n\t/a_test.go:19 +0x123"},
		{Action: "pass", Package: "b", Test: "TestSame"},
		{Action: "pass", Package: "b"},
		{Action: "output", Package: "a", Output: "exit status 2\n"},
		{Action: "fail", Package: "a"},
	} {
		if e := enc.Encode(e); e != nil {
			t.Fatal(e)
		}
	}
	code, out := report(t, input.String(), true, true)
	expected := "   a.TestSame\npanic: broken\n\ngoroutine 7 [running]:\nexample.TestSame()\n\t/a_test.go:19 +0x123\n"
	if code != 1 || !strings.Contains(out, expected) || !strings.Contains(out, "   a\nexit status 2\n") || strings.Contains(out, "pass noise") {
		t.Fatalf("unfinished diagnostic lost or misattributed, code=%d:\n%s", code, out)
	}
}

// Exercise real go test events: assertions, panic stacks, race reports, and
// TestMain exits have different terminal events and output attribution.
func TestRealGoFailuresKeepDiagnostics(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module diagnostics.fixture\n\ngo 1.22\n",
		"assert/a_test.go": `package assert
import "testing"
func TestSame(t *testing.T) { t.Run("child",func(t *testing.T){t.Fatal("assertion literal: wanted 7, got 9")}) }
`,
		"panic/p_test.go": `package paniccase
import "testing"
func TestSame(t *testing.T) { panic("panic literal") }
`,
		"main/m_test.go": `package maincase
import ("testing";"fmt";"os")
func TestSame(t *testing.T) {}
func TestMain(m *testing.M) { m.Run();fmt.Fprintln(os.Stderr,"TestMain literal exit");os.Exit(2) }
`,
		"race/r_test.go": `package racecase
import ("testing";"sync")
var shared int
func TestSame(t *testing.T) { var wg sync.WaitGroup;wg.Add(2);for i:=0;i<2;i++ {go func(){defer wg.Done();shared++}()};wg.Wait() }
`,
		"pass/ok_test.go": `package pass
import "testing"
func TestSame(t *testing.T) { t.Log("passing package noise") }
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
	}
	cmd := exec.Command("go", "test", "-race", "-json", "-count=1", "./...")
	cmd.Dir = dir
	raw, e := cmd.Output()
	if e == nil {
		t.Fatal("deliberately failing go tests succeeded")
	}
	if exit, ok := e.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("go test did not run expected failures: %v\n%s", e, raw)
	}
	rep, e := Parse(bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if code := Summary(rep, true, true, &out); code != 1 {
		t.Fatalf("failure report code %d", code)
	}
	for _, want := range []string{
		"diagnostics.fixture/assert.TestSame/child\n", "a_test.go:3: assertion literal: wanted 7, got 9",
		"diagnostics.fixture/panic.TestSame\n", "panic: panic literal", "p_test.go:3",
		"diagnostics.fixture/race.TestSame\n", "WARNING: DATA RACE\n", "Previous ", "r_test.go:4",
		"   diagnostics.fixture/main\n", "TestMain literal exit\n",
		"실패한 패키지 4 개",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "테스트 밖 실패") || strings.Contains(out.String(), "passing package noise") {
		t.Fatalf("misleading failure report:\n%s", out.String())
	}
	// Same test names from interleaved packages must retain their own output.
	for _, item := range rep.Tests {
		if strings.Contains(item.Output, "panic literal") && item.Name != "diagnostics.fixture/panic.TestSame" {
			t.Fatalf("panic attributed to %s", item.Name)
		}
		if strings.Contains(item.Output, "WARNING: DATA RACE") && item.Name != "diagnostics.fixture/race.TestSame" {
			t.Fatalf("race attributed to %s", item.Name)
		}
	}
}

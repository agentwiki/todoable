package main

import (
	"bytes"
	"strings"
	"testing"
)

// 게이트 자신을 검증한다. 이 도구가 틀리면 미구현과 프로세스 실패가 통과로 위장된다.

func lines(ss ...string) string { return strings.Join(ss, "\n") + "\n" }

func ev(action, pkg, test, output string) string {
	s := `{"Action":"` + action + `","Package":"` + pkg + `"`
	if test != "" {
		s += `,"Test":"` + test + `"`
	}
	if output != "" {
		s += `,"Output":"` + output + `"`
	}
	return s + "}"
}

func report(t *testing.T, input string, todoFails, requireTests bool) (int, string) {
	t.Helper()
	rep, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	code := Summary(rep, todoFails, requireTests, &buf)
	return code, buf.String()
}

func TestPassOnly(t *testing.T) {
	in := lines(ev("run", "p", "TestA", ""), ev("pass", "p", "TestA", ""), ev("pass", "p", "", ""))
	if code, out := report(t, in, true, true); code != 0 || !strings.Contains(out, "확인 1, 실패 0, 미구현 0, 건너뜀 0") {
		t.Fatalf("code=%d\n%s", code, out)
	}
}

// t.Skip 은 go test 를 성공으로 끝낸다. 미구현 표식이 있으면 -todo-fails 에서 실패여야 한다.
func TestTodoIsDistinguishedFromEnvSkip(t *testing.T) {
	in := lines(
		ev("run", "p", "TestScenario_SC_01", ""),
		ev("output", "p", "TestScenario_SC_01", "    sc_test.go:9: SCENARIO_TODO SC-01\\n"),
		ev("skip", "p", "TestScenario_SC_01", ""),
		ev("run", "p", "TestDeepDB", ""),
		ev("output", "p", "TestDeepDB", "    db_test.go:12: DB_URL is not configured\\n"),
		ev("skip", "p", "TestDeepDB", ""),
		ev("pass", "p", "", ""),
	)
	code, out := report(t, in, false, false)
	if code != 0 {
		t.Fatalf("todo-fails 가 꺼진 상태에서 실패했다:\n%s", out)
	}
	if !strings.Contains(out, "미구현 1, 건너뜀 1") {
		t.Fatalf("미구현과 환경 부족이 구분되지 않았다:\n%s", out)
	}
	if !strings.Contains(out, "DB_URL is not configured") {
		t.Fatalf("건너뜀 사유가 출력되지 않았다:\n%s", out)
	}
	if code, _ := report(t, in, true, false); code != 1 {
		t.Fatal("-todo-fails 인데 미구현이 통과했다")
	}
}

// TestMain 의 os.Exit(2) 는 `--- FAIL` 을 찍지 않는다. 패키지 실패 이벤트로 잡아야 한다.
func TestPackageFailureWithoutTestFail(t *testing.T) {
	in := lines(ev("output", "p", "", "exit status 2\\n"), ev("fail", "p", "", ""))
	if code, out := report(t, in, false, false); code != 1 || !strings.Contains(out, "실패한 패키지 1") {
		t.Fatalf("패키지 단위 실패가 통과했다(code=%d):\n%s", code, out)
	}
}

// run 은 있는데 pass/fail/skip 이 없으면 패닉이나 os.Exit 로 끝난 것이다. 통과가 아니다.
func TestUnfinishedTestIsFailure(t *testing.T) {
	in := lines(ev("run", "p", "TestA", ""), ev("output", "p", "TestA", "panic: boom\\n"))
	if code, out := report(t, in, false, false); code != 1 || !strings.Contains(out, "끝나지 않은") {
		t.Fatalf("끝나지 않은 테스트가 통과했다(code=%d):\n%s", code, out)
	}
}

func TestRequireTests(t *testing.T) {
	in := lines(ev("pass", "p", "", ""))
	if code, _ := report(t, in, false, true); code != 1 {
		t.Fatal("테스트가 없는데 -require-tests 가 통과시켰다")
	}
	if code, _ := report(t, in, false, false); code != 0 {
		t.Fatal("테스트 없는 패키지가 기본 설정에서 실패했다")
	}
}

func TestNonJSONLineIsError(t *testing.T) {
	if _, err := Parse(strings.NewReader("ok  \tp\t0.01s\n")); err == nil {
		t.Fatal("JSON 이 아닌 입력이 오류 없이 지나갔다. -json 없이 파이프된 것을 못 알아챈다")
	}
}

// 정적 대응(파일에 함수가 있다)과 실행(go test 가 돌렸다)은 다른 주장이다.
// tests.e2e 가 다른 곳을 가리키거나 빌드 태그로 파일이 배제되면 정적 대응은 만족하고 실행은 0 번이다.
func TestScenarioSpec_UnobservedScenarioFails(t *testing.T) {
	spec, err := ParseScenarioSpec("SC-01=1 SC-02=1")
	if err != nil {
		t.Fatal(err)
	}
	in := lines(
		ev("run", "example.com/app/test", "TestScenario_SC_01", ""),
		ev("run", "example.com/app/test", "TestScenario_SC_01/V-01", ""),
		ev("pass", "example.com/app/test", "TestScenario_SC_01/V-01", ""),
		ev("pass", "example.com/app/test", "TestScenario_SC_01", ""),
		ev("pass", "example.com/app/test", "", ""),
	)
	rep, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := SummaryWithScenarios(rep, true, true, spec, &buf); code != 1 || !strings.Contains(buf.String(), "TestScenario_SC_02") {
		t.Fatalf("실행되지 않은 SC-02 가 통과했다(code=%d):\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "시나리오 2 중 실행 1") {
		t.Fatalf("요약에 시나리오 실행 수가 없다:\n%s", buf.String())
	}
}

// `_ = "V-01"` 은 정적 대응은 만족하지만 하위 테스트를 만들지 않는다. 검증 줄은 실행된 하위 테스트여야 한다.
func TestScenarioSpec_VerifyMarkerMustBeExecutedSubtest(t *testing.T) {
	spec, _ := ParseScenarioSpec("SC-01=2")
	in := lines(
		ev("run", "p", "TestScenario_SC_01", ""),
		ev("run", "p", "TestScenario_SC_01/V-01", ""),
		ev("pass", "p", "TestScenario_SC_01/V-01", ""),
		ev("pass", "p", "TestScenario_SC_01", ""),
		ev("pass", "p", "", ""),
	)
	rep, _ := Parse(strings.NewReader(in))
	var buf bytes.Buffer
	if code := SummaryWithScenarios(rep, true, true, spec, &buf); code != 1 || !strings.Contains(buf.String(), "TestScenario_SC_01/V-02") {
		t.Fatalf("실행되지 않은 V-02 가 통과했다(code=%d):\n%s", code, buf.String())
	}
}

// 완전한 실행은 통과다. 미구현(todo)은 관측된 것으로 치고 -todo-fails 가 따로 실패시킨다.
func TestScenarioSpec_CompleteRunPassesAndTodoIsObserved(t *testing.T) {
	spec, _ := ParseScenarioSpec("SC-01=1 SC-02=1")
	in := lines(
		ev("run", "p", "TestScenario_SC_01", ""),
		ev("run", "p", "TestScenario_SC_01/V-01", ""),
		ev("pass", "p", "TestScenario_SC_01/V-01", ""),
		ev("pass", "p", "TestScenario_SC_01", ""),
		ev("run", "p", "TestScenario_SC_02", ""),
		ev("output", "p", "TestScenario_SC_02", "    sc_test.go:9: SCENARIO_TODO SC-02\\n"),
		ev("skip", "p", "TestScenario_SC_02", ""),
		ev("pass", "p", "", ""),
	)
	rep, _ := Parse(strings.NewReader(in))
	var buf bytes.Buffer
	if code := SummaryWithScenarios(rep, false, true, spec, &buf); code != 0 {
		t.Fatalf("미구현을 실행되지 않은 것으로 봤다(code=%d):\n%s", code, buf.String())
	}
	buf.Reset()
	if code := SummaryWithScenarios(rep, true, true, spec, &buf); code != 1 {
		t.Fatal("-todo-fails 인데 미구현이 통과했다")
	}
	spec2, _ := ParseScenarioSpec("SC-01=1")
	buf.Reset()
	if code := SummaryWithScenarios(rep, false, true, spec2, &buf); code != 0 {
		t.Fatalf("완전히 실행된 시나리오가 실패했다(code=%d):\n%s", code, buf.String())
	}
}

// 명세 형식이 어긋나면 오류다. 조용히 빈 명세가 되면 검사가 꺼진다.
func TestScenarioSpec_MalformedIsError(t *testing.T) {
	for _, bad := range []string{"SC-01", "SC-01=x", "01=2", "SC-01=1 SC-01=1"} {
		if _, err := ParseScenarioSpec(bad); err == nil {
			t.Errorf("%q 가 오류 없이 지나갔다", bad)
		}
	}
	if spec, err := ParseScenarioSpec(""); err != nil || len(spec.order) != 0 {
		t.Fatal("빈 명세(시나리오 검사 없음)가 거부됐다")
	}
}

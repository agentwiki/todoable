// Command testreport 는 `go test -json` 출력을 읽어 통과, 실패, 건너뜀, 미구현을 구분해 보고한다.
//
// verify.sh 가 go test 의 종료 코드만 보면 두 가지가 통과로 위장된다.
//
//   - t.Skip 은 명령 전체를 성공으로 끝낸다. 시나리오가 전부 미구현이어도 게이트가 녹색이다
//   - `--- FAIL` 문자열을 grep 하면 TestMain 의 os.Exit(2) 나 초기화 실패처럼
//     그 형식이 안 나오는 실패를 놓친다
//
// 그래서 문자열이 아니라 이벤트 스트림을 읽고, 건너뜀을 두 종류로 가른다.
//
//   - 미구현: 건너뜀 사유에 SCENARIO_TODO 표식이 있는 것. -todo-fails 면 실패로 친다
//   - 환경 부족: 그 밖의 건너뜀. 이름과 사유를 나열하고 개수를 요약에 찍는다
//
// 그리고 시나리오가 실제로 실행됐는지를 본다. archgate 는 문서와 테스트 파일의 정적 대응을 보지만,
// 파일에 함수가 있다는 것과 go test 가 그것을 돌렸다는 것은 다른 주장이다. tests.e2e 가 다른 곳을
// 가리키거나 빌드 태그로 파일이 배제되면 정적 대응은 만족하면서 실행은 0 번이었다. 실제로 뚫린 구멍이다.
// -scenarios 에 archgate -print scenarios 의 출력("SC-01=2 SC-02=1")을 넘기면
// 시나리오마다 TestScenario_SC_xx 가 관측됐고 통과했는지, 그리고 검증 줄마다 하위 테스트
// TestScenario_SC_xx/V-nn 이 관측됐고 통과했는지 대조한다. `_ = "V-01"` 같은 문자열 리터럴은
// 정적 대응은 만족시키지만 하위 테스트를 만들지 않으므로 여기서 걸린다.
//
// 사용법:
//
//	go test -json ./test/... | go run ./tools/testreport [-todo-fails] [-require-tests] [-scenarios "SC-01=2 SC-02=1"]
//
// 종료 코드: 0 전부 확인, 1 실패나 미구현(-todo-fails 일 때)이나 실행되지 않은 시나리오 있음, 2 입력을 읽지 못함.
// 파이프 앞의 go test 종료 코드는 셸의 pipefail 이 따로 본다. 둘 다 0 이어야 통과다.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// TodoMarker 는 미구현 시나리오의 skip 사유에 들어가는 고정 표식이다.
// 테스트 쪽 todo 헬퍼가 이것을 찍는다. 문자열이 어긋나면 미구현이 환경 부족으로 보인다.
const TodoMarker = "SCENARIO_TODO"

type event struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// Result 는 테스트 하나 또는 패키지 하나의 결과다.
type Result struct {
	Name   string // pkg 또는 pkg.Test
	Status string // pass, fail, skip, todo, unknown
	Reason string // skip 과 todo 의 사유
}

// Report 는 이벤트 스트림을 분류한 결과다.
type Report struct {
	Tests    []Result
	Packages []Result
}

// Parse 는 go test -json 이벤트를 읽는다. 잘못된 줄은 그냥 지나가지 않고 오류다.
// 빌드 실패 같은 비 JSON 출력이 섞이면 go test 자체의 종료 코드가 잡으므로 여기서는 형식만 본다.
func Parse(r io.Reader) (Report, error) {
	type key struct{ pkg, test string }
	status := map[key]string{}
	output := map[key][]string{}
	var order []key
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			return Report{}, fmt.Errorf("JSON 이 아닌 줄: %q", string(line))
		}
		k := key{ev.Package, ev.Test}
		switch ev.Action {
		case "run", "start":
			if _, seen := status[k]; !seen {
				status[k] = "unknown"
				order = append(order, k)
			}
		case "output":
			output[k] = append(output[k], ev.Output)
		case "pass", "fail", "skip":
			if _, seen := status[k]; !seen {
				order = append(order, k)
			}
			status[k] = ev.Action
		}
	}
	if err := sc.Err(); err != nil {
		return Report{}, err
	}
	var rep Report
	for _, k := range order {
		res := Result{Status: status[k]}
		if k.test == "" {
			res.Name = k.pkg
			rep.Packages = append(rep.Packages, res)
			continue
		}
		res.Name = k.pkg + "." + k.test
		if res.Status == "skip" {
			res.Reason = skipReason(output[k])
			if strings.Contains(res.Reason, TodoMarker) {
				res.Status = "todo"
			}
		}
		rep.Tests = append(rep.Tests, res)
	}
	return rep, nil
}

// skipReason 은 테스트 출력에서 사유 줄을 고른다. `--- SKIP` 과 `=== RUN` 은 사유가 아니다.
// -v 출력에서 사유는 `    x_test.go:12: 사유` 형태로 SKIP 줄보다 먼저 온다.
func skipReason(lines []string) string {
	var reasons []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "===") || strings.HasPrefix(t, "---") {
			continue
		}
		if i := strings.Index(t, ".go:"); i >= 0 {
			if j := strings.Index(t[i:], ": "); j >= 0 {
				t = t[i+j+2:]
			}
		}
		reasons = append(reasons, t)
	}
	return strings.Join(reasons, " / ")
}

// ScenarioSpec 은 시나리오 ID → 검증 줄 수다. archgate -print scenarios 의 출력을 읽는다.
type ScenarioSpec struct {
	order   []string
	verifys map[string]int
}

// ParseScenarioSpec 은 "SC-01=2 SC-02=1" 을 읽는다. 형식이 어긋나면 오류다. 조용히 빈 명세가 되면 검사가 꺼진다.
func ParseScenarioSpec(s string) (ScenarioSpec, error) {
	spec := ScenarioSpec{verifys: map[string]int{}}
	for _, f := range strings.Fields(s) {
		id, n, ok := strings.Cut(f, "=")
		if !ok || !strings.HasPrefix(id, "SC-") {
			return spec, fmt.Errorf("시나리오 명세 %q 는 SC-nn=개수 형식이어야 한다", f)
		}
		var count int
		if _, err := fmt.Sscanf(n, "%d", &count); err != nil || count < 0 {
			return spec, fmt.Errorf("시나리오 명세 %q 의 개수를 읽을 수 없다", f)
		}
		if _, dup := spec.verifys[id]; dup {
			return spec, fmt.Errorf("시나리오 명세에 %s 가 두 번 있다", id)
		}
		spec.order = append(spec.order, id)
		spec.verifys[id] = count
	}
	return spec, nil
}

// CheckScenarios 는 명세의 시나리오와 검증 줄이 실제로 실행됐고 통과했는지 대조한다.
// 관측되지 않은 것과 통과하지 않은 것을 구분해 이름을 돌려준다. 통과하지 않은 것은 Summary 가
// 이미 실패로 세지만, 여기서도 이름을 들어 "이 시나리오가 완료가 아니다" 를 분명히 한다.
// 미구현(todo)은 관측된 것으로 친다. -todo-fails 가 따로 실패시킨다.
func CheckScenarios(rep Report, spec ScenarioSpec) (unobserved, notPassed []string) {
	status := map[string]string{}
	for _, t := range rep.Tests {
		// pkg.Test 에서 Test 이름만 뗀다. 패키지 경로에는 점이 있을 수 있으므로 마지막 "." 뒤가 아니라
		// 첫 "TestScenario_" 부터 본다.
		if i := strings.Index(t.Name, ".TestScenario_"); i >= 0 {
			status[t.Name[i+1:]] = t.Status
		}
	}
	for _, id := range spec.order {
		fn := "TestScenario_" + strings.ReplaceAll(id, "-", "_")
		st, ok := status[fn]
		switch {
		case !ok:
			unobserved = append(unobserved, fn)
			continue
		case st == "todo":
			continue
		case st != "pass":
			notPassed = append(notPassed, fn)
			continue
		}
		for i := 1; i <= spec.verifys[id]; i++ {
			sub := fmt.Sprintf("%s/V-%02d", fn, i)
			switch st, ok := status[sub]; {
			case !ok:
				unobserved = append(unobserved, sub)
			case st != "pass":
				notPassed = append(notPassed, sub)
			}
		}
	}
	return unobserved, notPassed
}

// Summary 는 사람이 읽는 요약이다. 건너뛴 것과 확인한 것이 같은 줄에 같은 색으로 끝나면 안 된다.
func Summary(rep Report, todoFails, requireTests bool, w io.Writer) int {
	return SummaryWithScenarios(rep, todoFails, requireTests, ScenarioSpec{}, w)
}

// SummaryWithScenarios 는 Summary 에 시나리오 실행 대조를 더한 것이다.
func SummaryWithScenarios(rep Report, todoFails, requireTests bool, spec ScenarioSpec, w io.Writer) int {
	count := map[string]int{}
	for _, t := range rep.Tests {
		count[t.Status]++
	}
	failedPkgs := 0
	for _, p := range rep.Packages {
		if p.Status == "fail" {
			failedPkgs++
		}
	}
	code := 0
	if count["fail"] > 0 || count["unknown"] > 0 || failedPkgs > 0 {
		code = 1
	}
	if todoFails && count["todo"] > 0 {
		code = 1
	}
	if requireTests && len(rep.Tests) == 0 {
		fmt.Fprintln(w, "테스트가 하나도 돌지 않았다. 패턴이 빈 곳을 가리키거나 전부 필터에 걸렸다")
		code = 1
	}
	unobserved, notPassed := CheckScenarios(rep, spec)
	if len(unobserved) > 0 {
		fmt.Fprintln(w, "\n실행되지 않은 시나리오 테스트(문서에는 있는데 go test 가 돌리지 않았다. tests.e2e 범위, 빌드 태그, verify 하위 테스트 누락을 본다)")
		for _, n := range unobserved {
			fmt.Fprintf(w, "   %s\n", n)
		}
		code = 1
	}
	if len(notPassed) > 0 {
		fmt.Fprintln(w, "\n통과하지 않은 시나리오 테스트")
		for _, n := range notPassed {
			fmt.Fprintf(w, "   %s\n", n)
		}
		code = 1
	}

	list := func(status, title string) {
		var items []Result
		for _, t := range rep.Tests {
			if t.Status == status {
				items = append(items, t)
			}
		}
		if len(items) == 0 {
			return
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		fmt.Fprintf(w, "\n%s\n", title)
		for _, it := range items {
			if it.Reason != "" {
				fmt.Fprintf(w, "   %s  %s\n", it.Name, it.Reason)
			} else {
				fmt.Fprintf(w, "   %s\n", it.Name)
			}
		}
	}
	list("fail", "실패")
	list("unknown", "끝나지 않은 테스트(패닉이나 os.Exit)")
	list("todo", "미구현 시나리오")
	list("skip", "건너뛴 항목(환경 부족)")
	if failedPkgs > 0 {
		fmt.Fprintf(w, "\n실패한 패키지 %d 개 (테스트 밖 실패. TestMain, 초기화, 빌드)\n", failedPkgs)
	}
	fmt.Fprintf(w, "\n확인 %d, 실패 %d, 미구현 %d, 건너뜀 %d",
		count["pass"], count["fail"]+count["unknown"], count["todo"], count["skip"])
	if len(spec.order) > 0 {
		ran := len(spec.order)
		for _, n := range unobserved {
			if !strings.Contains(n, "/") {
				ran-- // 하위 테스트가 아니라 시나리오 자체가 안 돈 것
			}
		}
		fmt.Fprintf(w, ", 시나리오 %d 중 실행 %d", len(spec.order), ran)
	}
	fmt.Fprintln(w)
	return code
}

func main() {
	todoFails := flag.Bool("todo-fails", false, "미구현(SCENARIO_TODO) 시나리오를 실패로 친다")
	requireTests := flag.Bool("require-tests", false, "테스트가 하나도 안 돌면 실패로 친다")
	scenarios := flag.String("scenarios", "", "archgate -print scenarios 의 출력. 각 시나리오와 검증 줄이 실제로 실행되고 통과했는지 대조한다")
	flag.Parse()

	spec, err := ParseScenarioSpec(*scenarios)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testreport:", err)
		os.Exit(2)
	}
	rep, err := Parse(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testreport:", err)
		os.Exit(2)
	}
	os.Exit(SummaryWithScenarios(rep, *todoFails, *requireTests, spec, os.Stdout))
}

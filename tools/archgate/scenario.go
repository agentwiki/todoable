package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 시나리오 문서와 E2E 테스트의 대응을 기계로 본다.
//
// 이 방법의 완료 판정은 "시나리오의 검증 줄이 그대로 E2E 단정문이 되었는가" 인데,
// 그 대응을 리뷰어가 눈으로만 확인하면 사람 리뷰 부담이 에이전트 리뷰 부담으로 옮겨갔을 뿐이다.
// 단정문의 의미까지 기계가 볼 수는 없지만, 존재와 개수는 볼 수 있다.
//
//   - 문서의 `## SC-01.` 마다 `func TestScenario_SC_01(` 이 정확히 하나 있어야 한다
//   - 문서에 없는 SC 의 테스트(고아)가 있으면 안 된다. 삭제된 시나리오의 테스트가 남는 것을 막는다
//   - 시나리오의 `검증:` 줄이 n 개면 테스트 본문에 "V-01" … "V-0n" 문자열 리터럴이 전부 있어야 한다.
//     각 단정문 앞에 verify(t, "V-02") 같은 표식을 두는 규약이다. 표식이 없는 검증 줄은
//     옮겨지지 않은 검증 줄이다
//
// 여기서 보는 것은 정적 존재다. 실행은 별개의 주장이고 그것은 testreport -scenarios 가 본다.
// 이 파일의 Spec() 이 문서 쪽 기대치("SC-01=2 SC-02=1")를 내고, testreport 가 go test -json 의 관측과 대조한다.
// 문자열 리터럴 하나로 정적 대응을 만족시켜도 하위 테스트 TestScenario_SC_xx/V-nn 이 돌지 않으면 실행 대조에서 걸린다.
//
// 검증 줄이 옮겨졌다는 것과 올바르게 옮겨졌다는 것은 다르다. 후자는 여전히 리뷰어 몫이다.

var (
	scenarioHeading = regexp.MustCompile(`^##\s+SC-(\d+)\b`)
	verifyLine      = regexp.MustCompile(`^\s*(?:\d+\.\s+)?검증:`)
	scenarioTest    = regexp.MustCompile(`^TestScenario_SC_(\d+)$`)
	verifyMarker    = regexp.MustCompile(`^V-(\d+)$`)
)

// scenarioDoc 은 문서에서 뽑은 시나리오 ID → 검증 줄 수다.
type scenarioDoc struct {
	order   []string
	verifys map[string]int
}

// Spec 은 문서가 기대하는 시나리오와 검증 줄 수를 "SC-01=2 SC-02=1" 형식으로 돌려준다.
// testreport -scenarios 의 입력이다. 여기서 만든 형식을 testreport 가 그대로 읽는다.
func (d scenarioDoc) Spec() string {
	var parts []string
	for _, id := range d.order {
		parts = append(parts, fmt.Sprintf("%s=%d", id, d.verifys[id]))
	}
	return strings.Join(parts, " ")
}

// ParseScenarioDoc 은 Markdown 에서 SC ID 와 검증 줄 수를 뽑는다.
func ParseScenarioDoc(src string) (scenarioDoc, error) {
	doc := scenarioDoc{verifys: map[string]int{}}
	current := ""
	for _, line := range strings.Split(src, "\n") {
		if m := scenarioHeading.FindStringSubmatch(line); m != nil {
			current = "SC-" + m[1]
			if _, dup := doc.verifys[current]; dup {
				return doc, fmt.Errorf("시나리오 %s 가 문서에 두 번 있다", current)
			}
			doc.order = append(doc.order, current)
			doc.verifys[current] = 0
			continue
		}
		if strings.HasPrefix(line, "## ") {
			current = "" // 시나리오가 아닌 절
			continue
		}
		if current != "" && verifyLine.MatchString(line) {
			doc.verifys[current]++
		}
	}
	return doc, nil
}

// ScenarioTests 는 테스트 파일에서 찾은 시나리오 테스트 → 본문의 V 표식 집합이다.
type ScenarioTests struct {
	// 같은 SC 를 겨냥한 테스트가 여럿이면 여기 여럿이 쌓인다.
	byID map[string][]scenarioTestInfo
}

type scenarioTestInfo struct {
	pos     string
	markers map[string]bool
}

// ParseScenarioTests 는 파일 이름 → 소스 맵에서 TestScenario_SC_xx 함수와 그 본문의 V 표식을 뽑는다.
func ParseScenarioTests(files map[string]string) (ScenarioTests, error) {
	st := ScenarioTests{byID: map[string][]scenarioTestInfo{}}
	for _, name := range sortedKeys(files) {
		if !strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, files[name], parser.SkipObjectResolution)
		if err != nil {
			return st, err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			m := scenarioTest.FindStringSubmatch(fn.Name.Name)
			if m == nil {
				continue
			}
			id := "SC-" + m[1]
			info := scenarioTestInfo{
				pos:     fmt.Sprintf("%s:%d", name, fset.Position(fn.Pos()).Line),
				markers: map[string]bool{},
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(lit.Value)
				if err == nil && verifyMarker.MatchString(text) {
					info.markers[text] = true
				}
				return true
			})
			st.byID[id] = append(st.byID[id], info)
		}
	}
	return st, nil
}

// CheckScenarios 는 문서와 테스트의 대응을 대조한다.
func CheckScenarios(doc scenarioDoc, tests ScenarioTests, docName string) []Violation {
	var out []Violation
	for _, id := range doc.order {
		infos := tests.byID[id]
		switch len(infos) {
		case 0:
			out = append(out, Violation{"sc", docName, fmt.Sprintf("%s 의 테스트 TestScenario_%s 가 없다", id, strings.ReplaceAll(id, "-", "_"))})
			continue
		case 1:
		default:
			out = append(out, Violation{"sc", infos[1].pos, fmt.Sprintf("%s 의 테스트가 %d 개다. 시나리오 하나 = 테스트 하나", id, len(infos))})
		}
		info := infos[0]
		n := doc.verifys[id]
		if n == 0 {
			out = append(out, Violation{"sc", docName, fmt.Sprintf("%s 에 검증: 줄이 없다. 검증 없는 시나리오는 완료 판정을 할 수 없다", id)})
		}
		var missing []string
		for i := 1; i <= n; i++ {
			v := fmt.Sprintf("V-%02d", i)
			if !info.markers[v] {
				missing = append(missing, v)
			}
		}
		if len(missing) > 0 {
			out = append(out, Violation{"sc", info.pos,
				fmt.Sprintf("%s 의 검증 줄 %s 표식이 테스트에 없다. 옮겨지지 않은 검증 줄이다", id, strings.Join(missing, ", "))})
		}
		for m := range info.markers {
			idx, _ := strconv.Atoi(strings.TrimPrefix(m, "V-"))
			if idx > n || idx == 0 {
				out = append(out, Violation{"sc", info.pos, fmt.Sprintf("%s 의 테스트에 문서에 없는 표식 %s 가 있다", id, m)})
			}
		}
	}
	for _, id := range sortedKeys(tests.byID) {
		if _, ok := doc.verifys[id]; !ok {
			out = append(out, Violation{"sc", tests.byID[id][0].pos, fmt.Sprintf("%s 는 문서에 없는 시나리오다. 고아 테스트다", id)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pos+out[i].Msg < out[j].Pos+out[j].Msg })
	return out
}

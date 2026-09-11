// Command archgate 는 설계 규약을 커밋 시점에 강제하는 게이트다.
//
// 규칙은 archgate.json 에 선언하고, 판정은 순수 함수가 맡는다.
// 파일시스템, go 명령, 프로세스 종료는 이 파일에만 있다. 그래야 게이트 자신을 픽스처로 검증할 수 있다.
//
// 사용법:
//
//	archgate [-config archgate.json] [-root .]
//	archgate -config archgate.json -print tests.fast   verify.sh 가 설정값을 읽을 때
//	archgate -config archgate.json -print scenarios    문서의 시나리오 ID 와 검증 줄 수. testreport -scenarios 에 넘긴다
//	archgate -config archgate.json -print build.tags   buildTags. verify.sh 가 go 명령에 -tags 로 전달한다
//
// 종료 코드: 0 위반 없음, 1 위반 있음, 2 게이트 자체가 판정하지 못함(설정 오류, 타입 검사 실패).
// 2 는 1 보다 나쁘다. 게이트가 눈을 감은 것이므로 verify.sh 는 둘 다 실패로 다룬다.
//
// 검사는 두 층이다.
//
//   - 구문 검사: 금지 리터럴, 금지 import, init, 직접 import 계층. 빌드 컨텍스트와 무관하게
//     모든 .go 파일(빌드 태그로 배제된 파일, cgo 파일, 테스트 파일 포함)에 적용된다.
//   - 타입 검사: 금지 선택자, 규칙 A, 규칙 B, 의존 폐포. 현재 GOOS/GOARCH 에서 컴파일되는
//     파일에 적용된다. archgate.json 의 buildTargets 에 다른 조합을 적으면 그 조합마다
//     자기 자신을 다시 실행해 타입 검사를 반복한다.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/build"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	cfgPath := flag.String("config", "archgate.json", "설정 파일")
	root := flag.String("root", ".", "검사할 모듈 루트")
	one := flag.Bool("one", false, "내부용. 현재 GOOS/GOARCH 의 타입 검사만 하고 JSON 으로 출력한다")
	print := flag.String("print", "", "설정값 하나를 출력하고 끝낸다. tests.fast, tests.e2e, build.tags, scenarios")
	flag.Parse()

	if *print != "" {
		os.Exit(printValue(*cfgPath, *root, *print, os.Stdout, os.Stderr))
	}
	os.Exit(run(*cfgPath, *root, *one, os.Stdout, os.Stderr))
}

// printValue 는 verify.sh 가 계층 경로를 두 번 적지 않도록 설정값을 내보낸다.
func printValue(cfgPath, root, key string, stdout, stderr io.Writer) int {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "archgate:", err)
		return 2
	}
	var vals []string
	switch key {
	case "tests.fast":
		vals = cfg.Tests.Fast
	case "tests.e2e":
		vals = cfg.Tests.E2E
	case "build.tags":
		vals = cfg.BuildTags
	case "scenarios":
		// 문서가 기대하는 시나리오와 검증 줄 수. "SC-01=2 SC-02=1" 형식이다.
		// testreport 가 이것을 받아 실제로 실행된 TestScenario_SC_xx 와 그 하위 V-nn 을 대조한다.
		// 정적 대응(파일에 함수가 있다)과 실행(go test 가 그 함수를 돌렸다)은 다른 주장이다.
		if cfg.Scenarios.Doc == "" {
			return 0
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(cfg.Scenarios.Doc)))
		if err != nil {
			fmt.Fprintln(stderr, "archgate: 시나리오 문서를 읽을 수 없다:", err)
			return 2
		}
		doc, err := ParseScenarioDoc(string(raw))
		if err != nil {
			fmt.Fprintln(stderr, "archgate:", err)
			return 2
		}
		vals = strings.Fields(doc.Spec())
	default:
		fmt.Fprintf(stderr, "archgate: -print 는 tests.fast, tests.e2e, build.tags, scenarios 중 하나다. %q 는 모른다\n", key)
		return 2
	}
	fmt.Fprintln(stdout, strings.Join(vals, " "))
	return 0
}

// run 은 main 의 본체다. 종료 코드를 돌려주고 os.Exit 는 main 만 부른다.
func run(cfgPath, root string, one bool, stdout, stderr io.Writer) int {
	absCfg, err := filepath.Abs(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "archgate:", err)
		return 2
	}
	cfg, err := LoadConfig(absCfg)
	if err != nil {
		fmt.Fprintln(stderr, "archgate:", err)
		return 2
	}
	// go/importer 는 현재 디렉터리에서 go list 를 부른다. 모듈 루트로 옮긴다.
	if err := os.Chdir(root); err != nil {
		fmt.Fprintln(stderr, "archgate:", err)
		return 2
	}
	mod, err := ReadModulePath(".")
	if err != nil {
		fmt.Fprintln(stderr, "archgate:", err)
		return 2
	}
	if mod != cfg.Module {
		fmt.Fprintf(stderr, "archgate: go.mod 의 module %q 과 설정의 module %q 이 다르다. 설정이 가리키는 모듈이 없으면 모든 패키지가 검사 밖이 된다\n", mod, cfg.Module)
		return 2
	}
	// 계층 path 는 실제 디렉터리여야 한다. 오타 난 path 는 그 계층을 조용히 비운다.
	for _, name := range cfg.LayerNames() {
		p := filepath.FromSlash(cfg.Layers[name].Path)
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			fmt.Fprintf(stderr, "archgate: 계층 %q 의 path %q 가 디렉터리가 아니다. 아직 없는 계층이면 mkdir 로 만들어 둔다\n", name, cfg.Layers[name].Path)
			return 2
		}
	}

	violations, err := evaluate(cfg, ".", one)
	if err != nil {
		fmt.Fprintln(stderr, "archgate:", err)
		return 2
	}

	if one {
		enc := json.NewEncoder(stdout)
		for _, v := range violations {
			if err := enc.Encode(v); err != nil {
				fmt.Fprintln(stderr, "archgate:", err)
				return 2
			}
		}
		if len(violations) > 0 {
			return 1
		}
		return 0
	}

	current := build.Default.GOOS + "/" + build.Default.GOARCH
	for _, target := range cfg.BuildTargets {
		if target == current {
			continue
		}
		vs, err := runTarget(absCfg, target)
		if err != nil {
			fmt.Fprintln(stderr, "archgate:", err)
			return 2
		}
		violations = append(violations, vs...)
	}

	violations = dedupe(violations)
	if len(violations) == 0 {
		fmt.Fprintln(stdout, "archgate: 위반 없음")
		return 0
	}
	fmt.Fprintf(stderr, "archgate: 위반 %d 건\n", len(violations))
	for _, v := range violations {
		fmt.Fprintln(stderr, "  "+v.String())
	}
	return 1
}

// runTarget 은 다른 GOOS/GOARCH 로 자기 자신을 다시 실행한다.
func runTarget(cfgPath, target string) ([]Violation, error) {
	goos, goarch, _ := strings.Cut(target, "/")
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "-config", cfgPath, "-root", ".", "-one")
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		err = nil // 위반이 있을 뿐이다
	}
	if err != nil {
		return nil, fmt.Errorf("대상 %s 검사 실패: %w", target, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var vs []Violation
	for {
		var v Violation
		if err := dec.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("대상 %s 출력을 읽을 수 없다: %w", target, err)
		}
		vs = append(vs, v)
	}
	return vs, nil
}

// evaluate 는 판정 전체다. typedOnly 가 참이면 구문 검사를 건너뛴다(다른 대상 조합 실행용).
func evaluate(cfg *Config, root string, typedOnly bool) ([]Violation, error) {
	ctx := build.Default
	ctx.BuildTags = append([]string{}, cfg.BuildTags...)
	pkgs, err := Discover(root, cfg.Module, &ctx)
	if err != nil {
		return nil, err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	rel := func(dir, name string) string {
		r, err := filepath.Rel(absRoot, filepath.Join(dir, name))
		if err != nil {
			return name
		}
		return filepath.ToSlash(r)
	}
	read := func(p PackageFiles, names []string) ([]ParsedFile, error) {
		var out []ParsedFile
		for _, n := range names {
			src, err := os.ReadFile(filepath.Join(p.Dir, n))
			if err != nil {
				return nil, err
			}
			out = append(out, ParsedFile{Name: rel(p.Dir, n), Src: src})
		}
		return out, nil
	}

	// 모듈 안의 모든 패키지는 정확히 하나의 계층에 속하거나 excludedPaths 에 있어야 한다.
	// 미선언 패키지를 건너뛰면 "검사 대상이 아님" 이 곧 면제가 된다. 디렉터리 하나로 게이트 전체를 우회했다.
	var unlayered []string
	for _, p := range pkgs {
		if cfg.LayerOf(p.ImportPath) == unknownLayer && !cfg.IsExcluded(p.ImportPath) {
			unlayered = append(unlayered, strings.TrimPrefix(p.ImportPath, cfg.Module+"/"))
		}
	}
	if len(unlayered) > 0 {
		return nil, fmt.Errorf("어느 계층에도 속하지 않는 패키지가 있다: %s\n  layers 에 넣거나, 게이트 밖이면 excludedPaths 에 근거와 함께 적는다. 미선언은 면제가 아니라 설정 오류다", strings.Join(unlayered, ", "))
	}

	imp, err := exportImporter(cfg)
	if err != nil {
		return nil, err
	}
	var violations []Violation
	var inScope []*Checked
	graph := map[string][]string{}

	for _, p := range pkgs {
		layer := cfg.LayerOf(p.ImportPath)
		if layer == unknownLayer {
			continue // excludedPaths. 위에서 미선언은 걸러졌다
		}
		graph[p.ImportPath] = p.Imports

		if !typedOnly {
			kinds := []struct {
				names  []string
				isTest bool
				typed  bool
			}{
				{p.Built(), false, true},
				{p.IgnoredGo, false, false},
				{p.TestGo, true, true},
				{p.XTestGo, true, true},
			}
			for _, k := range kinds {
				files, err := read(p, k.names)
				if err != nil {
					return nil, err
				}
				for _, f := range files {
					in := FileInput{Filename: f.Name, Src: f.Src, Pkg: p.ImportPath, Layer: layer, IsTest: k.isTest}
					vs, err := CheckFileSyntax(cfg, in)
					if err != nil {
						return nil, err
					}
					violations = append(violations, vs...)
					if !k.typed {
						vs, err := CheckSelectorsSyntactic(cfg, in)
						if err != nil {
							return nil, err
						}
						violations = append(violations, vs...)
					}
				}
			}
		}

		built := p.Built()
		if len(built) == 0 {
			continue
		}
		files, err := read(p, built)
		if err != nil {
			return nil, err
		}
		ch, err := TypeCheck(p.ImportPath, files, imp, true)
		if err != nil {
			return nil, fmt.Errorf("%w\n  빌드는 통과했는데 게이트가 타입을 붙이지 못했다. 조용히 통과시키지 않는다", err)
		}
		ch.Layer = layer
		violations = append(violations, CheckSelectorsTyped(cfg, ch, nil)...)
		violations = append(violations, CheckSignatureShape(cfg, ch)...)
		if cfg.InScope(layer) {
			inScope = append(inScope, ch)
		}

		// 테스트 변형. 금지 선택자만 본다. 규칙 A/B 의 대상은 비테스트 파일이다.
		if len(p.TestGo) > 0 {
			tests, err := read(p, p.TestGo)
			if err != nil {
				return nil, err
			}
			only := map[string]bool{}
			for _, t := range tests {
				only[t.Name] = true
			}
			tc, err := TypeCheck(p.ImportPath, append(files, tests...), imp, false)
			if err != nil {
				return nil, err
			}
			tc.Layer = layer
			violations = append(violations, CheckSelectorsTyped(cfg, tc, only)...)
		}
		if len(p.XTestGo) > 0 {
			xtests, err := read(p, p.XTestGo)
			if err != nil {
				return nil, err
			}
			xc, err := TypeCheck(p.ImportPath+"_test", xtests, imp, false)
			if err != nil {
				return nil, err
			}
			xc.Layer = layer
			violations = append(violations, CheckSelectorsTyped(cfg, xc, nil)...)
		}
	}

	violations = append(violations, CheckDepClosure(cfg, graph)...)
	if !cfg.ShapeRulesDisabled {
		violations = append(violations, CheckParamStructs(cfg, inScope)...)
	}
	if !typedOnly && cfg.Scenarios.Doc != "" {
		vs, err := checkScenarioFiles(cfg, root, &ctx)
		if err != nil {
			return nil, err
		}
		violations = append(violations, vs...)
	}
	return violations, nil
}

// checkScenarioFiles 는 시나리오 문서와 테스트 디렉터리를 읽어 대응을 대조한다.
//
// 빌드 제약으로 현재 컨텍스트에서 배제된 파일은 대응에 세지 않는다. 그런 파일의 TestScenario 는
// 존재하지만 실행되지 않으므로, 대응된 것으로 인정하면 한 번도 돌지 않은 시나리오가 완료가 된다.
// 대신 그런 파일에 시나리오 테스트가 있으면 그 자체를 위반으로 보고한다.
// 최종 보루는 testreport -scenarios 의 실행 대조이고, 이것은 그 앞의 정적 경고다.
func checkScenarioFiles(cfg *Config, root string, ctx *build.Context) ([]Violation, error) {
	docPath := filepath.Join(root, filepath.FromSlash(cfg.Scenarios.Doc))
	raw, err := os.ReadFile(docPath)
	if err != nil {
		return nil, fmt.Errorf("시나리오 문서를 읽을 수 없다: %w", err)
	}
	doc, err := ParseScenarioDoc(string(raw))
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	excluded := map[string]string{}
	testDir := filepath.Join(root, filepath.FromSlash(cfg.Scenarios.TestDir))
	err = filepath.WalkDir(testDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = p
		}
		match, err := ctx.MatchFile(filepath.Dir(p), filepath.Base(p))
		if err != nil {
			return err
		}
		if match {
			files[filepath.ToSlash(rel)] = string(src)
		} else {
			excluded[filepath.ToSlash(rel)] = string(src)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("시나리오 테스트 디렉터리를 읽을 수 없다: %w", err)
	}
	tests, err := ParseScenarioTests(files)
	if err != nil {
		return nil, err
	}
	out := CheckScenarios(doc, tests, cfg.Scenarios.Doc)
	hidden, err := ParseScenarioTests(excluded)
	if err != nil {
		return nil, err
	}
	for _, id := range sortedKeys(hidden.byID) {
		for _, info := range hidden.byID[id] {
			out = append(out, Violation{"sc", info.pos,
				fmt.Sprintf("%s 의 테스트가 빌드 제약으로 현재 컨텍스트에서 배제돼 있다. 존재하지만 실행되지 않는 시나리오 테스트는 대응이 아니다", id)})
		}
	}
	return out, nil
}

// exportImporter 는 go list -export 가 만든 export 데이터로 import 를 푸는 importer 다.
//
// go/importer 의 기본 importer 는 GOROOT 밖 패키지를 GOPATH 방식으로만 찾는다.
// 모듈 모드에서는 go list 가 위치와 export 파일을 알려 줘야 한다. -deps -test 로 테스트 변형의
// 의존까지 한 번에 받는다. 현재 GOOS/GOARCH 환경을 그대로 따르므로 대상 조합별 재실행과 맞물린다.
//
// buildTags 는 여기에도 전달한다. 파일 발견과 타입 검사 입력은 태그를 적용하고 export 데이터만
// 적용하지 않으면, 태그 뒤의 선언을 참조하는 패키지가 "undefined" 로 판정 불능(2)이 된다.
// 잘못된 녹색은 아니지만 잘못된 빨간색이고, 그것도 게이트를 약화시킨다.
func exportImporter(cfg *Config) (types.Importer, error) {
	args := []string{"list", "-export", "-deps", "-test", "-buildvcs=false"}
	if len(cfg.BuildTags) > 0 {
		args = append(args, "-tags="+strings.Join(cfg.BuildTags, ","))
	}
	args = append(args, "-f", "{{.ImportPath}}\t{{.Export}}", "./...")
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -export 실패: %w", err)
	}
	exports := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, file, ok := strings.Cut(line, "\t")
		if !ok || file == "" || strings.Contains(path, " [") {
			continue // 테스트 변형은 이름에 [pkg.test] 가 붙는다. 비테스트 변형만 쓴다
		}
		exports[path] = file
	}
	lookup := func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("%s 의 export 데이터가 없다", path)
		}
		return os.Open(file)
	}
	return importer.ForCompiler(token.NewFileSet(), "gc", lookup), nil
}

func dedupe(vs []Violation) []Violation {
	seen := map[string]bool{}
	var out []Violation
	for _, v := range vs {
		// 구문 판정과 타입 판정이 같은 자리를 잡으면 하나로 합친다.
		k := v.Rule + "|" + v.Pos + "|" + strings.ReplaceAll(v.Msg, "(구문 판정)", "")
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		if out[i].Pos != out[j].Pos {
			return out[i].Pos < out[j].Pos
		}
		return out[i].Msg < out[j].Msg
	})
	return out
}

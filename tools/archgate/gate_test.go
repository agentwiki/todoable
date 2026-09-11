package main

import (
	"go/importer"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 게이트 자신을 검증한다. 규칙마다 잡아야 할 것과 잡으면 안 될 것을 짝으로 둔다.
//
// 검증되지 않은 게이트는 아무것도 막지 못하고, 조용히 무력화되면 알아차릴 사람이 없다.
// 판정 함수가 순수해야 이 검증이 가능하다. 그래서 파일시스템과 프로세스 종료는 main.go 에만 둔다.
//
// 아래 픽스처의 절반은 실제로 뚫렸던 우회 형태다. 우회를 하나 찾을 때마다 여기 픽스처를 더한다.

func testConfig() *Config {
	c := &Config{
		Module:    "example.com/app",
		MaxParams: 5,
		Layers: map[string]Layer{
			"domain":   {Path: "internal/domain"},
			"ports":    {Path: "internal/ports", Allow: []string{"domain"}},
			"usecases": {Path: "internal/usecases", Allow: []string{"domain", "ports"}},
			"adapters": {Path: "internal/adapters", Allow: []string{"domain", "ports"}, NoSiblings: true},
			"app":      {Path: "internal/app", Allow: []string{"domain", "ports", "usecases"}},
			"main":     {Path: "cmd", Allow: []string{"*"}},
		},
		ShapeScope:  []string{"usecases", "app"},
		InfraLayers: []string{"adapters"},
		ForbiddenSelectors: map[string]string{
			"time.Now":       "현재 시각을 직접 읽는다. Clock 포트를 쓴다",
			"time.Sleep":     "실시간으로 대기한다. Clock 포트를 쓴다",
			"math/rand/v2.N": "난수를 직접 만든다. Random 포트를 쓴다",
		},
		ForbiddenImports: map[string]string{
			"math/rand": "난수를 직접 만든다. Random 포트를 쓴다",
		},
		ForbiddenLiterals: []string{`/etc/secret`},
		Tests:             TestPackages{Fast: []string{"./internal/...", "./cmd/..."}, E2E: []string{"./test/..."}},
	}
	if err := c.Validate(); err != nil {
		panic(err)
	}
	return c
}

func hasRule(vs []Violation, rule string) bool {
	for _, v := range vs {
		if v.Rule == rule {
			return true
		}
	}
	return false
}

func msgs(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(v.String())
		b.WriteString("\n")
	}
	return b.String()
}

// --- 픽스처 타입 검사 ---

// fixturePkg 는 인메모리 패키지 하나다. 이전 픽스처 패키지를 import 할 수 있다.
type fixturePkg struct {
	path  string
	layer string
	files map[string]string
}

type fixtureImporter struct {
	pkgs     map[string]*types.Package
	fallback types.Importer
}

func (f *fixtureImporter) Import(path string) (*types.Package, error) {
	if p, ok := f.pkgs[path]; ok {
		return p, nil
	}
	return f.fallback.Import(path)
}

// typed 는 픽스처들을 순서대로 타입 검사한다. 타입 오류는 픽스처 오류이므로 즉시 실패한다.
func typed(t *testing.T, pkgs ...fixturePkg) []*Checked {
	t.Helper()
	imp := &fixtureImporter{pkgs: map[string]*types.Package{}, fallback: importer.Default()}
	var out []*Checked
	for _, p := range pkgs {
		var files []ParsedFile
		for _, name := range sortedKeys(p.files) {
			files = append(files, ParsedFile{Name: name, Src: []byte(p.files[name])})
		}
		ch, err := TypeCheck(p.path, files, imp, true)
		if err != nil {
			t.Fatalf("픽스처 타입 검사 실패: %v", err)
		}
		ch.Layer = p.layer
		imp.pkgs[p.path] = ch.Pkg
		out = append(out, ch)
	}
	return out
}

func uc(src string) fixturePkg {
	return fixturePkg{path: "example.com/app/internal/usecases", layer: "usecases", files: map[string]string{"u.go": src}}
}

func shape(t *testing.T, src string) []Violation {
	t.Helper()
	return CheckSignatureShape(testConfig(), typed(t, uc(src))[0])
}

func params(t *testing.T, pkgs ...fixturePkg) []Violation {
	t.Helper()
	return CheckParamStructs(testConfig(), typed(t, pkgs...))
}

func selectors(t *testing.T, layer, src string) []Violation {
	t.Helper()
	p := fixturePkg{path: "example.com/app/internal/" + layer, layer: layer, files: map[string]string{"f.go": src}}
	return CheckSelectorsTyped(testConfig(), typed(t, p)[0], nil)
}

func syntax(t *testing.T, in FileInput) []Violation {
	t.Helper()
	if in.Pkg == "" {
		in.Pkg = "example.com/app/internal/" + in.Layer
	}
	if in.Filename == "" {
		in.Filename = "f.go"
	}
	vs, err := CheckFileSyntax(testConfig(), in)
	if err != nil {
		t.Fatalf("픽스처 파싱 실패: %v", err)
	}
	return vs
}

// --- 설정 ---

func TestConfig_LayerOfPrefersLongestPath(t *testing.T) {
	c := testConfig()
	c.Layers["deep"] = Layer{Path: "internal/domain/deep"}
	if got := c.LayerOf("example.com/app/internal/domain/deep/x"); got != "deep" {
		t.Fatalf("더 긴 경로가 이겨야 한다. got=%q", got)
	}
	if got := c.LayerOf("example.com/other/internal/domain"); got != "" {
		t.Fatalf("모듈 밖은 빈 계층이어야 한다. got=%q", got)
	}
}

// 점 없는 모듈 경로(`module app`)도 유효하다. 표준 라이브러리 추정으로 모듈 안 패키지를 건너뛰면 안 된다.
func TestConfig_DotlessModulePathIsStillChecked(t *testing.T) {
	c := testConfig()
	c.Module = "app"
	if why := c.importViolation("app/internal/usecases", "app/internal/adapters/fs"); why == "" {
		t.Fatal("점 없는 모듈 경로의 계층 위반이 잡히지 않았다")
	}
	if why := c.importViolation("app/internal/usecases", "os"); why != "" {
		t.Fatalf("표준 라이브러리가 위반으로 잡혔다: %s", why)
	}
}

func TestConfig_RejectsSilentlyDisablingSettings(t *testing.T) {
	cases := map[string]func(*Config){
		"빈 layers":                 func(c *Config) { c.Layers = map[string]Layer{} },
		"중복 path":                  func(c *Config) { c.Layers["dup"] = Layer{Path: "internal/domain"} },
		"없는 계층을 infraLayers 에":     func(c *Config) { c.InfraLayers = []string{"nope"} },
		"없는 계층을 allowInitLayers 에": func(c *Config) { c.AllowInitLayers = []string{"nope"} },
		"없는 계층을 shapeScope 에":      func(c *Config) { c.ShapeScope = []string{"nope"} },
		"빈 shapeScope 를 명시 없이":     func(c *Config) { c.ShapeScope = nil },
		"정규화되지 않은 path":            func(c *Config) { c.Layers["x"] = Layer{Path: "internal//x/"} },
		"절대 path":                  func(c *Config) { c.Layers["x"] = Layer{Path: "/internal/x"} },
		"allow 에 없는 계층":            func(c *Config) { c.Layers["x"] = Layer{Path: "internal/x", Allow: []string{"nope"}} },
		"잘못된 testImportPolicy":     func(c *Config) { c.TestImportPolicy = "sometimes" },
		"잘못된 buildTargets":         func(c *Config) { c.BuildTargets = []string{"windows"} },
		"선택자 열쇠에 점이 없음":            func(c *Config) { c.ForbiddenSelectors["Now"] = "x" },
		"빈 tests.fast":             func(c *Config) { c.Tests.Fast = nil },
		"빈 tests.e2e":              func(c *Config) { c.Tests.E2E = nil },
		"상대 패턴이 아닌 tests":          func(c *Config) { c.Tests.E2E = []string{"test/..."} },
		"testDir 없는 scenarios":     func(c *Config) { c.Scenarios = ScenarioConfig{Doc: "docs/scenarios.md"} },
		"tests.e2e 밖의 testDir": func(c *Config) {
			c.Scenarios = ScenarioConfig{Doc: "docs/scenarios.md", TestDir: "e2e"}
		},
		"근거 없는 excludedPaths":   func(c *Config) { c.ExcludedPaths = []ExcludedPath{{Path: "tools"}} },
		"계층과 겹치는 excludedPaths": func(c *Config) { c.ExcludedPaths = []ExcludedPath{{Path: "internal", Reason: "x"}} },
		"정규화되지 않은 excludedPaths": func(c *Config) {
			c.ExcludedPaths = []ExcludedPath{{Path: "tools/", Reason: "x"}}
		},
	}
	for name, mutate := range cases {
		c := testConfig()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: 오류 없이 통과했다. 이 설정은 게이트를 조용히 끈다", name)
		}
	}
	ok := testConfig()
	ok.ShapeScope = nil
	ok.ShapeRulesDisabled = true
	if err := ok.Validate(); err != nil {
		t.Fatalf("명시적으로 끈 shape 규칙이 거부됐다: %v", err)
	}
	ok = testConfig()
	ok.Scenarios = ScenarioConfig{Doc: "docs/scenarios.md", TestDir: "test/e2e"}
	ok.ExcludedPaths = []ExcludedPath{{Path: "tools", Reason: "게이트 구현"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("정상 설정이 거부됐다: %v", err)
	}
}

// 모듈 안인데 계층이 없는 패키지(excludedPaths)로의 의존은 계층 위반이다. "*" 도 그것을 허용하지 않는다.
func TestConfig_DependingOnUnlayeredPackageIsViolation(t *testing.T) {
	c := testConfig()
	c.ExcludedPaths = []ExcludedPath{{Path: "tools", Reason: "x"}}
	if why := c.importViolation("example.com/app/internal/usecases", "example.com/app/tools/x"); why == "" {
		t.Fatal("게이트 밖 패키지로의 의존이 허용됐다")
	}
	if why := c.importViolation("example.com/app/cmd/app", "example.com/app/tools/x"); why == "" {
		t.Fatal("allow \"*\" 가 게이트 밖 패키지까지 허용했다")
	}
	if why := c.importViolation("example.com/app/internal/usecases", "golang.org/x/text"); why != "" {
		t.Fatalf("모듈 밖 의존이 위반으로 잡혔다: %s", why)
	}
}

func TestConfig_UnknownFieldIsRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "archgate.json")
	if err := os.WriteFile(p, []byte(`{"module":"example.com/app","layers":{"d":{"path":"internal/d"}},"shapeScope":["d"],"tests":{"fast":["./internal/..."],"e2e":["./test/..."]},"forbiddenSelector":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "forbiddenSelector") {
		t.Fatalf("오타 난 필드가 조용히 무시됐다: %v", err)
	}
}

// --- 계층 의존 ---

func TestDeps_TransitiveViolationIsCaught(t *testing.T) {
	c := testConfig()
	// usecases → ports 는 허용, ports → adapters 는 위반. usecases 에서는 전이 위반으로 보인다.
	graph := map[string][]string{
		"example.com/app/internal/usecases":    {"example.com/app/internal/ports", "fmt"},
		"example.com/app/internal/ports":       {"example.com/app/internal/adapters/fs"},
		"example.com/app/internal/adapters/fs": {"os"},
	}
	vs := CheckDepClosure(c, graph)
	if len(vs) != 1 || !strings.Contains(vs[0].Msg, "전이 의존") || vs[0].Pos != "example.com/app/internal/usecases" {
		t.Fatalf("전이 위반 하나만 보고돼야 한다(직접 위반은 파일 검사 몫). got:\n%s", msgs(vs))
	}
}

func TestDeps_NoSiblings(t *testing.T) {
	c := testConfig()
	if why := c.importViolation("example.com/app/internal/adapters/a", "example.com/app/internal/adapters/b"); why == "" {
		t.Fatal("어댑터 묶음 간 의존이 잡히지 않았다")
	}
	if why := c.importViolation("example.com/app/internal/adapters/a", "example.com/app/internal/adapters/a/sub"); why != "" {
		t.Fatalf("같은 묶음 안의 의존이 위반으로 잡혔다: %s", why)
	}
	if why := c.importViolation("example.com/app/internal/usecases/a", "example.com/app/internal/usecases/b"); why != "" {
		t.Fatalf("NoSiblings 가 아닌 계층이 위반으로 잡혔다: %s", why)
	}
}

func TestDeps_WildcardAllow(t *testing.T) {
	c := testConfig()
	for _, dep := range []string{"internal/domain", "internal/adapters/fs", "internal/app"} {
		if why := c.importViolation("example.com/app/cmd/app", "example.com/app/"+dep); why != "" {
			t.Fatalf("allow: [\"*\"] 인 계층이 위반으로 잡혔다: %s", why)
		}
	}
}

// --- 구문 검사 ---

func TestSyntax_ForbiddenImport(t *testing.T) {
	src := "package p\nimport \"math/rand\"\nfunc f() int { return rand.Intn(3) }"
	if !hasRule(syntax(t, FileInput{Src: []byte(src), Layer: "usecases"}), "import") {
		t.Fatal("math/rand 가 잡히지 않았다")
	}
	if hasRule(syntax(t, FileInput{Src: []byte(src), Layer: "adapters"}), "import") {
		t.Fatal("인프라 계층의 import 가 위반으로 잡혔다")
	}
}

func TestSyntax_Init(t *testing.T) {
	src := "package p\nvar m = map[string]int{}\nfunc init() { m[\"a\"] = 1 }"
	if !hasRule(syntax(t, FileInput{Src: []byte(src), Layer: "usecases"}), "init") {
		t.Fatal("init 이 잡히지 않았다")
	}
	if hasRule(syntax(t, FileInput{Src: []byte("package p\ntype T struct{}\nfunc (T) init() {}"), Layer: "usecases"}), "init") {
		t.Fatal("init 이름의 메서드가 위반으로 잡혔다")
	}
	c := testConfig()
	c.AllowInitLayers = []string{"usecases"}
	vs, _ := CheckFileSyntax(c, FileInput{Filename: "f.go", Src: []byte(src), Pkg: "example.com/app/internal/usecases", Layer: "usecases"})
	if hasRule(vs, "init") {
		t.Fatal("allowInitLayers 의 init 이 위반으로 잡혔다")
	}
}

func TestSyntax_ForbiddenLiteral(t *testing.T) {
	if !hasRule(syntax(t, FileInput{Src: []byte("package p\nconst p1 = \"/etc/secret/key\"\n"), Layer: "usecases"}), "lit") {
		t.Fatal("금지 문자열이 잡히지 않았다")
	}
	// 이스케이프로 쪼개도 푼 값으로 본다.
	if !hasRule(syntax(t, FileInput{Src: []byte("package p\nconst p1 = \"/etc\\x2fsecret\"\n"), Layer: "usecases"}), "lit") {
		t.Fatal("이스케이프로 쪼갠 금지 문자열이 잡히지 않았다")
	}
	if hasRule(syntax(t, FileInput{Src: []byte("package p\nconst p1 = \"/etc/passwd\"\n"), Layer: "usecases"}), "lit") {
		t.Fatal("정상 문자열이 위반으로 잡혔다")
	}
}

// 빌드 태그로 배제된 파일은 의존 폐포에 없다. 직접 import 로 계층을 봐야 하고, NoSiblings 도 봐야 한다.
func TestSyntax_ImportLayerOnExcludedFile(t *testing.T) {
	src := "//go:build windows\n\npackage p\nimport \"example.com/app/internal/adapters/fs\"\nvar _ = fs.New\n"
	if !hasRule(syntax(t, FileInput{Src: []byte(src), Layer: "usecases"}), "layer") {
		t.Fatal("배제된 파일의 계층 위반이 잡히지 않았다")
	}
	sib := "//go:build windows\n\npackage a\nimport \"example.com/app/internal/adapters/b\"\nvar _ = b.New\n"
	if !hasRule(syntax(t, FileInput{Src: []byte(sib), Pkg: "example.com/app/internal/adapters/a", Layer: "adapters"}), "layer") {
		t.Fatal("배제된 파일의 NoSiblings 위반이 잡히지 않았다")
	}
}

// 테스트 파일의 계층 import 는 정책으로 선언한다. 묵시적 면제가 아니다.
func TestSyntax_TestImportPolicy(t *testing.T) {
	src := "package p\nimport \"example.com/app/internal/adapters/fs\"\nvar _ = fs.New\n"
	in := FileInput{Filename: "f_test.go", Src: []byte(src), Pkg: "example.com/app/internal/usecases", Layer: "usecases", IsTest: true}
	same := testConfig()
	vs, _ := CheckFileSyntax(same, in)
	if !hasRule(vs, "layer") {
		t.Fatal("testImportPolicy=same 인데 테스트의 계층 위반이 잡히지 않았다")
	}
	anyP := testConfig()
	anyP.TestImportPolicy = "any"
	vs, _ = CheckFileSyntax(anyP, in)
	if hasRule(vs, "layer") {
		t.Fatal("testImportPolicy=any 인데 테스트의 계층 import 가 위반으로 잡혔다")
	}
}

// 타입 정보가 없는 파일의 차선 판정. 경로 끝이 메이저 버전이면 그 앞이 패키지 이름이다.
func TestSyntax_SelectorFallbackHandlesVersionSuffix(t *testing.T) {
	src := "//go:build windows\n\npackage p\nimport \"math/rand/v2\"\nfunc f() int { return rand.N(3) }"
	vs, err := CheckSelectorsSyntactic(testConfig(), FileInput{Filename: "f.go", Src: []byte(src), Layer: "usecases"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasRule(vs, "call") {
		t.Fatal("math/rand/v2.N 이 구문 판정에서 잡히지 않았다")
	}
}

// --- 금지 선택자 (타입 판정) ---

func TestSelectors_Basic(t *testing.T) {
	src := "package usecases\nimport \"time\"\nfunc f() time.Time { return time.Now() }"
	if !hasRule(selectors(t, "usecases", src), "call") {
		t.Fatal("time.Now 가 잡히지 않았다")
	}
	if hasRule(selectors(t, "adapters", strings.Replace(src, "usecases", "adapters", 1)), "call") {
		t.Fatal("인프라 계층의 time.Now 가 위반으로 잡혔다")
	}
}

func TestSelectors_AliasAndDotImportDoNotEvade(t *testing.T) {
	alias := "package usecases\nimport gotime \"time\"\nfunc f() gotime.Time { return gotime.Now() }"
	if !hasRule(selectors(t, "usecases", alias), "call") {
		t.Fatal("별칭 import 로 우회됐다")
	}
	dot := "package usecases\nimport . \"time\"\nfunc f() Time { return Now() }"
	if !hasRule(selectors(t, "usecases", dot), "call") {
		t.Fatal("점 import 로 우회됐다")
	}
	// 점 import 라도 금지 선택자를 안 쓰면 위반이 아니다. 타입 판정이라 가능하다.
	dotOK := "package usecases\nimport . \"time\"\nfunc f() Duration { return Second }"
	if len(selectors(t, "usecases", dotOK)) != 0 {
		t.Fatal("금지 선택자를 안 쓰는 점 import 가 위반으로 잡혔다")
	}
}

// 경로 끝(v2)과 패키지 이름(rand)이 다르다. 타입 정보로 판정하면 문제없다.
func TestSelectors_MajorVersionPath(t *testing.T) {
	src := "package usecases\nimport \"math/rand/v2\"\nfunc f() int { return rand.N(3) }"
	if !hasRule(selectors(t, "usecases", src), "call") {
		t.Fatal("math/rand/v2.N 이 잡히지 않았다")
	}
}

// import 이름과 같은 지역 변수의 메서드는 금지 호출이 아니다. 문자열 비교는 여기서 오탐한다.
func TestSelectors_ShadowedLocalIsNotFalsePositive(t *testing.T) {
	src := "package usecases\nimport \"time\"\nvar _ = time.Second\ntype clock struct{}\nfunc (clock) Now() int { return 0 }\nfunc f(time clock) int { return time.Now() }"
	if vs := selectors(t, "usecases", src); len(vs) != 0 {
		t.Fatalf("지역 변수의 메서드가 금지 호출로 잡혔다:\n%s", msgs(vs))
	}
}

// 테스트 파일에도 금지 선택자가 걸린다. only 로 테스트 파일만 보고한다.
func TestSelectors_TestFilesAreChecked(t *testing.T) {
	p := fixturePkg{path: "example.com/app/internal/usecases", layer: "usecases", files: map[string]string{
		"u.go":      "package usecases\nfunc F() int { return 1 }",
		"u_test.go": "package usecases\nimport \"time\"\nvar _ = time.Now\n",
	}}
	vs := CheckSelectorsTyped(testConfig(), typed(t, p)[0], map[string]bool{"u_test.go": true})
	if !hasRule(vs, "call") {
		t.Fatal("테스트 파일의 time.Now 가 잡히지 않았다")
	}
}

// --- 규칙 A ---

func TestRuleA_TooManyParams(t *testing.T) {
	if !hasRule(shape(t, "package usecases\nfunc f(a int, b string, c bool, d float64, e byte) {}"), "A") {
		t.Fatal("인자 5개가 잡히지 않았다")
	}
}

func TestRuleA_ContextNotCounted(t *testing.T) {
	src := "package usecases\nimport \"context\"\nfunc f(ctx context.Context, a int, b string, c bool, d float64) {}"
	if vs := shape(t, src); len(vs) != 0 {
		t.Fatalf("context 가 개수에 포함됐다:\n%s", msgs(vs))
	}
	// 별칭 import 여도 context.Context 다. 문자열 비교는 여기서 오탐한다.
	aliased := "package usecases\nimport ctx \"context\"\nfunc f(c ctx.Context, a int, b string, d bool, e float64) {}"
	if vs := shape(t, aliased); len(vs) != 0 {
		t.Fatalf("별칭 import 된 context 가 개수에 포함됐다:\n%s", msgs(vs))
	}
}

func TestRuleA_VariadicAndReceiverNotCounted(t *testing.T) {
	if vs := shape(t, "package usecases\nfunc f(a int, b string, c bool, d float64, rest ...byte) {}"); len(vs) != 0 {
		t.Fatalf("가변 인자가 개수에 포함됐다:\n%s", msgs(vs))
	}
	if vs := shape(t, "package usecases\ntype T struct{}\nfunc (t T) f(a int, b string, c bool, d float64) {}"); len(vs) != 0 {
		t.Fatalf("수신자가 인자로 세어졌다:\n%s", msgs(vs))
	}
}

func TestRuleA_SameTypeParams(t *testing.T) {
	if !hasRule(shape(t, "package usecases\nfunc f(ref string, alias string) {}"), "A") {
		t.Fatal("같은 타입 인자 2개가 잡히지 않았다")
	}
	if hasRule(shape(t, "package usecases\nfunc f(ref string, n int) {}"), "A") {
		t.Fatal("서로 다른 타입이 위반으로 잡혔다")
	}
}

// 별칭은 같은 타입이다. 순서를 바꿔도 컴파일된다.
func TestRuleA_AliasIsSameType(t *testing.T) {
	src := "package usecases\ntype Text = string\nfunc f(a string, b Text) {}"
	if !hasRule(shape(t, src), "A") {
		t.Fatal("별칭으로 우회됐다. type Text = string 은 string 이다")
	}
	// 정의 타입은 다른 타입이다. 이것이 규칙 A 가 권하는 해결이다.
	defined := "package usecases\ntype Ref string\ntype Alias string\nfunc f(a Ref, b Alias) {}"
	if vs := shape(t, defined); len(vs) != 0 {
		t.Fatalf("정의 타입이 같은 타입으로 오인됐다:\n%s", msgs(vs))
	}
}

// 문자열 비교가 오탐하던 것들. 오탐하는 게이트는 반드시 약화된다.
func TestRuleA_NoFalsePositivesOnStructurallyDifferentTypes(t *testing.T) {
	cases := map[string]string{
		"배열과 슬라이스":    "package usecases\nfunc f(a [2]int, b []int) {}",
		"함수 타입":       "package usecases\nfunc f(a func(int), b func() error) {}",
		"제네릭 타입 인자":   "package usecases\ntype Set[T any] struct{}\nfunc f(a Set[int], b Set[string]) {}",
		"다른 제네릭 타입":   "package usecases\ntype Box[T any] struct{}\ntype Other[T any] struct{}\nfunc f(a Box[int], b Other[string]) {}",
		"맵 값 타입":      "package usecases\nfunc f(a map[string]int, b map[string]bool) {}",
		"채널 방향":       "package usecases\nfunc f(a <-chan int, b chan<- int) {}",
		"포인터와 값":      "package usecases\ntype T struct{}\nfunc f(a T, b *T) {}",
		"서로 다른 인터페이스": "package usecases\ntype A interface{ A() }\ntype B interface{ B() }\nfunc f(a A, b B) {}",
		"이름 있는 함수 타입": "package usecases\ntype H func()\nfunc f(a H, b func()) {}",
	}
	for name, src := range cases {
		if vs := shape(t, src); len(vs) != 0 {
			t.Errorf("%s: 오탐\n%s", name, msgs(vs))
		}
	}
}

func TestRuleA_GenericSameInstanceIsCaught(t *testing.T) {
	if !hasRule(shape(t, "package usecases\ntype Set[T any] struct{}\nfunc f(a Set[int], b Set[int]) {}"), "A") {
		t.Fatal("같은 제네릭 인스턴스가 잡히지 않았다")
	}
}

// --- 규칙 B ---

const depsSrc = `package usecases

type FS interface{ Read(string) error }
type Meta interface{ List() error }
type Clock interface{ Now() int64 }

type Deps struct {
	F FS
	M Meta
	C Clock
}

func DoA(d Deps) error { return d.F.Read("x") }
func DoB(d Deps) error { return d.M.List() }
`

// (a) 인터페이스 필드를 가진 구조체가 두 함수의 인자면 공용 협력자 묶음이다.
func TestRuleB_SharedCollaboratorBundle(t *testing.T) {
	if !hasRule(params(t, uc(depsSrc)), "B-a") {
		t.Fatal("공용 협력자 묶음이 잡히지 않았다")
	}
}

// (b) 안 읽히는 필드는 시그니처의 거짓말이다.
func TestRuleB_UnusedField(t *testing.T) {
	if !hasRule(params(t, uc(depsSrc)), "B-b") {
		t.Fatal("미사용 필드가 잡히지 않았다")
	}
	clean := `package usecases
type FS interface{ Read(string) error }
type P struct{ F FS; Path string }
func Do(p P) error { return p.F.Read(p.Path) }
`
	if vs := params(t, uc(clean)); len(vs) != 0 {
		t.Fatalf("모든 필드를 읽는 정상 함수가 잡혔다:\n%s", msgs(vs))
	}
}

// (c) 통째 전달을 막지 않으면 (b) 가 무력화된다.
func TestRuleB_WholeForwarding(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type P struct{ F FS }

func helper(p P) error { return p.F.Read("x") }
func Do(p P) error     { return helper(p) }
`
	if !hasRule(params(t, uc(src)), "B-c") {
		t.Fatal("통째 전달이 잡히지 않았다")
	}
}

// 이름 없는 인자와 수신자는 규칙 B 의 우회로였다. `func F(Deps)` 는 Deps 를 받았는데 아무것도 읽지 않은 것이다.
// 문서가 "수신자를 빼면 협력자 묶음이 수신자로 도망간다" 고 강조해 놓고 이름 없는 수신자는 세지 않았다.
func TestRuleB_UnnamedParamAndReceiverDoNotEvade(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type Meta interface{ List() error }

type Deps struct {
	F FS
	M Meta
}

func F(Deps) {}

type R struct {
	F FS
	M Meta
}

func (R) A() {}
func (R) B() {}

type Q struct{ F FS }

func G(_ Q) {}
`
	vs := params(t, uc(src))
	out := msgs(vs)
	for _, want := range []string{
		"F 가 받는 example.com/app/internal/usecases.Deps 의 필드 F, M",
		"usecases.R 는 협력자를 담은 구조체인데 함수 2 개가 받는다(A, B)",
		"A 가 받는 example.com/app/internal/usecases.R 의 필드 F, M",
		"G 가 받는 example.com/app/internal/usecases.Q 의 필드 F",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q 가 보고되지 않았다:\n%s", want, out)
		}
	}
}

// 스칼라만 가진 값 객체는 여러 함수가 받아도 정상이다. 오탐하면 게이트가 약화된다.
func TestRuleB_ValueObjectExempt(t *testing.T) {
	src := `package usecases

type Config struct {
	Root string
	Port int
	Err  error
	Any  any
}

func A(c Config) string { return c.Root }
func B(c Config) int    { return c.Port }
func C(c Config) Config { return c }
`
	vs := params(t, uc(src))
	if hasRule(vs, "B-a") {
		t.Fatal("값 객체가 공유 위반으로 잡혔다")
	}
	if hasRule(vs, "B-c") {
		t.Fatal("값 객체가 통째 전달로 잡혔다")
	}
}

// 포인터로 받는 값 객체는 (b) 면제다. 결과를 담아 돌려주는 구조체나 실행 시점 설정 묶음이 그것이다.
func TestRuleB_PointerValueObjectExemptFromUnusedField(t *testing.T) {
	src := `package usecases

type Result struct {
	Count int
	Names []string
}

func Fill(r *Result) { r.Count = 3 }
`
	if vs := params(t, uc(src)); len(vs) != 0 {
		t.Fatalf("포인터 값 객체가 미사용 필드로 잡혔다:\n%s", msgs(vs))
	}
}

// 협력자 묶음이 포인터가 되는 것만으로 (b) 를 빠져나가면 안 된다.
func TestRuleB_PointerCollaboratorStillChecked(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type Meta interface{ List() error }
type P struct {
	F FS
	M Meta
}

func Do(p *P) error { return p.F.Read("x") }
`
	if !hasRule(params(t, uc(src)), "B-b") {
		t.Fatal("포인터 협력자 묶음의 미사용 필드가 잡히지 않았다")
	}
}

// 다른 패키지에 정의된 구조체는 (b) 대상이 아니다. 값 객체를 오탐하지 않기 위한 경계다.
func TestRuleB_OtherPackageStructExempt(t *testing.T) {
	domain := fixturePkg{path: "example.com/app/internal/domain", layer: "domain", files: map[string]string{
		"d.go": "package domain\n\ntype Config struct {\n\tRoot string\n\tPort int\n}\n",
	}}
	use := uc("package usecases\n\nimport \"example.com/app/internal/domain\"\n\nfunc Do(c domain.Config) string { return c.Root }\n")
	if hasRule(params(t, domain, use), "B-b") {
		t.Fatal("다른 패키지의 값 객체가 미사용 필드로 잡혔다")
	}
}

// 협력자 묶음이 메서드 수신자로 도망가는 것을 (a) 가 잡아야 한다.
func TestRuleB_ReceiverEscapeIsCaught(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type Meta interface{ List() error }
type Deps struct {
	F FS
	M Meta
}

func (d Deps) DoA() error { return d.F.Read("x") }
func (d Deps) DoB() error { return d.M.List() }
`
	if !hasRule(params(t, uc(src)), "B-a") {
		t.Fatal("수신자로 도망간 협력자 묶음이 잡히지 않았다")
	}
}

// 메서드가 하나뿐인 협력자 수신자는 (a) 의 대상이 아니지만 (b) 는 받는다.
// 협력자 하나를 묶은 단일 작업 객체는 정직한 명세일 수 있다. 다만 안 읽는 필드는 여전히 거짓말이다.
func TestRuleB_SingleMethodReceiver(t *testing.T) {
	honest := `package usecases
type Repo interface{ Get() error }
type Svc struct{ R Repo }
func (s Svc) Do() error { return s.R.Get() }
`
	if vs := params(t, uc(honest)); len(vs) != 0 {
		t.Fatalf("모든 필드를 읽는 단일 메서드 수신자가 잡혔다:\n%s", msgs(vs))
	}
	lying := `package usecases
type Repo interface{ Get() error }
type Svc struct{ R Repo; Unused Repo }
func (s Svc) Do() error { return s.R.Get() }
`
	if !hasRule(params(t, uc(lying)), "B-b") {
		t.Fatal("단일 메서드 수신자의 미사용 협력자가 잡히지 않았다")
	}
}

// 한 겹 감싸면 빠져나가던 우회. 협력자 판정은 중첩 구조체 안까지 본다.
func TestRuleB_NestedStructDoesNotEvade(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type Meta interface{ List() error }
type Inner struct{ F FS; M Meta }
type Deps struct{ D Inner }

func DoA(d Deps) error { return d.D.F.Read("x") }
func DoB(d Deps) error { return d.D.M.List() }
func DoC(d Deps) error { return helper(d) }
func helper(d Deps) error { return d.D.F.Read("y") }
`
	vs := params(t, uc(src))
	if !hasRule(vs, "B-a") {
		t.Fatal("중첩 구조체로 감싼 협력자 묶음이 (a) 를 빠져나갔다")
	}
	if !hasRule(vs, "B-c") {
		t.Fatal("중첩 구조체로 감싼 협력자 묶음이 (c) 를 빠져나갔다")
	}
}

// 함수 타입 필드는 인터페이스와 같은 협력자다. `Now func() time.Time` 주입은 흔하다.
func TestRuleB_FuncTypeFieldIsCollaborator(t *testing.T) {
	src := `package usecases

type Deps struct {
	Read func(string) error
	List func() error
}

func DoA(d Deps) error { return d.Read("x") }
func DoB(d Deps) error { return d.List() }
`
	if !hasRule(params(t, uc(src)), "B-a") {
		t.Fatal("함수 타입 필드 묶음이 (a) 를 빠져나갔다")
	}
}

// 협력자가 슬라이스, 맵, 포인터 뒤에 있어도 협력자다.
func TestRuleB_CollaboratorBehindContainerIsFound(t *testing.T) {
	src := `package usecases

type Repo interface{ Get() error }
type Deps struct {
	Hooks []func() error
	Repos map[string]Repo
	Main  *Repo
}

func A(d Deps) { _, _, _ = d.Hooks, d.Repos, d.Main }
func B(d Deps) { _, _, _ = d.Hooks, d.Repos, d.Main }
`
	if !hasRule(params(t, uc(src)), "B-a") {
		t.Fatal("컨테이너 뒤의 협력자 묶음이 (a) 를 빠져나갔다")
	}
}

// 외부 패키지의 인터페이스도 협력자다. 이 파일 안의 선언만 보면 io.Reader 를 놓친다.
func TestRuleB_ExternalInterfaceIsCollaborator(t *testing.T) {
	src := `package usecases

import "io"

type Deps struct {
	R io.Reader
	W io.Writer
}

func A(d Deps) { _, _ = d.R, d.W }
func B(d Deps) { _, _ = d.R, d.W }
`
	if !hasRule(params(t, uc(src)), "B-a") {
		t.Fatal("io.Reader 묶음이 (a) 를 빠져나갔다")
	}
}

// 타입 별칭은 같은 구조체다.
func TestRuleB_TypeAliasDoesNotEvade(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type Meta interface{ List() error }
type Deps struct{ F FS; M Meta }
type Alias = Deps

func A(d Alias) { _, _ = d.F, d.M }
func B(d Alias) { _, _ = d.F, d.M }
`
	if !hasRule(params(t, uc(src)), "B-a") {
		t.Fatal("타입 별칭으로 (a) 를 빠져나갔다")
	}
}

// 지역 변수로 복사해 넘기는 것도 통째 전달이다.
func TestRuleB_LocalCopyIsForwarding(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type P struct{ F FS }

func sink(p P) error { return p.F.Read("x") }
func Do(p P) error {
	if p.F == nil {
		return nil
	}
	q := p
	return sink(q)
}
`
	vs := params(t, uc(src))
	if !hasRule(vs, "B-c") {
		t.Fatal("지역 변수 복사로 (c) 를 빠져나갔다")
	}
}

// nil 비교와 재대입은 통째 사용이 아니다.
func TestRuleB_NilCompareIsNotForwarding(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type P struct{ F FS }

func Do(p *P) error {
	if p == nil {
		return nil
	}
	return p.F.Read("x")
}
`
	if vs := params(t, uc(src)); len(vs) != 0 {
		t.Fatalf("nil 비교가 통째 사용으로 잡혔다:\n%s", msgs(vs))
	}
}

// 내부 블록에서 같은 이름을 다시 선언해 읽은 척하는 것은 읽기가 아니다. 식별자는 객체로 대조한다.
func TestRuleB_ShadowedNameIsNotARead(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type Meta interface{ List() error }
type P struct{ F FS; M Meta }

func Do(p P) error {
	_ = p.F
	func(p P) { _ = p.M }(P{})
	{
		p := P{}
		_ = p.M
	}
	return nil
}
`
	if !hasRule(params(t, uc(src)), "B-b") {
		t.Fatal("가리기로 읽은 척한 필드가 읽힌 것으로 세어졌다")
	}
}

// 쓰기는 읽기가 아니다.
func TestRuleB_WriteIsNotARead(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type Meta interface{ List() error }
type P struct{ F FS; M Meta }

func Do(p *P) {
	p.F = nil
	p.M = nil
}
`
	if !hasRule(params(t, uc(src)), "B-b") {
		t.Fatal("쓰기만 한 필드가 읽힌 것으로 세어졌다")
	}
}

// 임베딩된 협력자를 승격 메서드로 쓰면 그 임베딩 필드를 읽은 것이다.
func TestRuleB_PromotedUseCountsAsRead(t *testing.T) {
	src := `package usecases

type FS interface{ Read(string) error }
type P struct{ FS; Path string }

func Do(p P) error { return p.Read(p.Path) }
`
	if vs := params(t, uc(src)); len(vs) != 0 {
		t.Fatalf("승격 메서드 사용이 읽기로 세어지지 않았다:\n%s", msgs(vs))
	}
}

// 적용 범위 밖 계층은 규칙 A 와 B 를 받지 않는다.
func TestShapeScopeIsRespected(t *testing.T) {
	p := fixturePkg{path: "example.com/app/internal/domain", layer: "domain", files: map[string]string{"d.go": strings.Replace(depsSrc, "package usecases", "package domain", 1)}}
	chs := typed(t, p)
	if vs := CheckParamStructs(testConfig(), chs); len(vs) != 0 {
		t.Fatal("범위 밖 계층이 규칙 B 를 받았다")
	}
	if vs := CheckSignatureShape(testConfig(), chs[0]); len(vs) != 0 {
		t.Fatal("범위 밖 계층이 규칙 A 를 받았다")
	}
}

// --- 시나리오 대응 ---

const scenarioDocSrc = `# 시나리오

## 공통 전제

검증: 이 줄은 시나리오 밖이므로 세지 않는다

## SC-01. 첫 흐름

1. 한다
   검증: 종료 코드 0
2. 또 한다
   검증: 상태가 바뀌었다

## SC-02. 둘째 흐름

1. 한다
   검증: 된다
`

func scenarios(t *testing.T, tests map[string]string) []Violation {
	t.Helper()
	doc, err := ParseScenarioDoc(scenarioDocSrc)
	if err != nil {
		t.Fatal(err)
	}
	st, err := ParseScenarioTests(tests)
	if err != nil {
		t.Fatal(err)
	}
	return CheckScenarios(doc, st, "docs/scenarios.md")
}

func TestScenario_DocParsing(t *testing.T) {
	doc, err := ParseScenarioDoc(scenarioDocSrc)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.order) != 2 || doc.verifys["SC-01"] != 2 || doc.verifys["SC-02"] != 1 {
		t.Fatalf("검증 줄 수가 틀렸다: %+v", doc)
	}
}

func TestScenario_CompleteMappingPasses(t *testing.T) {
	vs := scenarios(t, map[string]string{"test/sc_test.go": `package test
import "testing"
func TestScenario_SC_01(t *testing.T) { verify(t, "V-01"); verify(t, "V-02") }
func TestScenario_SC_02(t *testing.T) { verify(t, "V-01") }
func verify(t *testing.T, id string) {}
`})
	if len(vs) != 0 {
		t.Fatalf("완전한 대응이 위반으로 잡혔다:\n%s", msgs(vs))
	}
}

func TestScenario_MissingOrphanDuplicateAndMarkers(t *testing.T) {
	vs := scenarios(t, map[string]string{"test/sc_test.go": `package test
import "testing"
func TestScenario_SC_01(t *testing.T) { verify(t, "V-01"); verify(t, "V-03") }
func TestScenario_SC_01_again(t *testing.T) {}
func TestScenario_SC_09(t *testing.T) { verify(t, "V-01") }
func verify(t *testing.T, id string) {}
`})
	out := msgs(vs)
	for _, want := range []string{
		"SC-02 의 테스트 TestScenario_SC_02 가 없다", // 누락
		"V-02 표식이 테스트에 없다",                    // 옮겨지지 않은 검증 줄
		"문서에 없는 표식 V-03",                      // 문서보다 많은 표식
		"SC-09 는 문서에 없는 시나리오다",                // 고아
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q 가 보고되지 않았다:\n%s", want, out)
		}
	}
	// 접미사가 붙은 이름은 시나리오 테스트가 아니다. 이름 규약이 곧 대응이다.
	if strings.Contains(out, "테스트가 2 개") {
		t.Errorf("규약 밖 이름이 시나리오 테스트로 세어졌다:\n%s", out)
	}
	dup := scenarios(t, map[string]string{
		"test/a_test.go": "package test\nimport \"testing\"\nfunc TestScenario_SC_01(t *testing.T) { _ = \"V-01\"; _ = \"V-02\" }\nfunc TestScenario_SC_02(t *testing.T) { _ = \"V-01\" }\n",
		"test/b_test.go": "package test\nimport \"testing\"\nfunc TestScenario_SC_02(t *testing.T) { _ = \"V-01\" }\n",
	})
	if !strings.Contains(msgs(dup), "SC-02 의 테스트가 2 개") {
		t.Errorf("중복 테스트가 보고되지 않았다:\n%s", msgs(dup))
	}
}

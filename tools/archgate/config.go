package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

// Config 는 archgate.json 의 내용이다.
//
// 계층 정의를 한 곳에 모으는 것이 이 파일의 목적이다. 계층 이름이 게이트 상수와
// 판별 함수와 허용 목록과 린터 설정에 흩어지면, 계층 하나를 추가할 때 여러 곳을 고쳐야 하고
// 하나를 빠뜨리면 게이트가 조용히 약해진다.
//
// 설정 오류는 전부 로딩 시점에 거부한다. 빈 값이나 오타로 규칙이 조용히 꺼지는 것이
// 가장 나쁜 실패다. 규칙을 끄려면 명시적으로 끈다.
type Config struct {
	Module string           `json:"module"`
	Layers map[string]Layer `json:"layers"`

	// ShapeScope 는 규칙 A 와 B 를 적용할 계층 이름이다.
	// 값 계산이 주인 계층은 빼는 것이 보통이다. 순수 이항 술어를 구조체로 감싸면 읽기만 나빠진다.
	// 비우려면 ShapeRulesDisabled 를 참으로 둔다. 빈 목록만으로는 오류다.
	ShapeScope         []string `json:"shapeScope"`
	ShapeRulesDisabled bool     `json:"shapeRulesDisabled"`

	// MaxParams 는 규칙 A 의 인자 개수 상한이다. 이 값 이상이면 위반이다.
	MaxParams int `json:"maxParams"`

	// ForbiddenSelectors 는 `import경로.식별자` 를 열쇠로 하는 금지 목록이다.
	// 타입 정보로 판정하므로 별칭 import, 점 import, 지역 변수 가리기, `math/rand/v2` 같은
	// 경로 끝과 패키지 이름이 다른 경우에도 그대로 통한다.
	ForbiddenSelectors map[string]string `json:"forbiddenSelectors"`

	// ForbiddenImports 는 import 자체가 금지되는 패키지다.
	ForbiddenImports map[string]string `json:"forbiddenImports"`

	// InfraLayers 는 위 두 금지 목록에서 면제되는 계층이다.
	// 비결정 소스를 실제로 만지는 것이 존재 이유인 계층을 적는다.
	InfraLayers []string `json:"infraLayers"`

	// ForbiddenLiterals 는 소스에 나타나면 안 되는 문자열이다.
	// 이스케이프를 푼 값으로 비교하므로 경로 구분자를 그대로 적는다.
	ForbiddenLiterals []string `json:"forbiddenLiterals"`

	// AllowInitLayers 는 init 함수를 허용하는 계층이다. 보통 비운다.
	AllowInitLayers []string `json:"allowInitLayers"`

	// TestImportPolicy 는 _test.go 파일의 계층 import 규칙이다.
	//   "same"  테스트도 자기 계층의 허용 목록을 따른다 (기본값)
	//   "any"   테스트는 어느 계층이든 import 할 수 있다
	// 묵시적 면제를 두지 않는다. 면제는 정책으로 선언한다.
	TestImportPolicy string `json:"testImportPolicy"`

	// BuildTargets 는 타입 기반 검사를 돌릴 GOOS/GOARCH 조합이다. 비우면 현재 조합만이다.
	// 여기 없는 조합의 파일은 구문 검사(금지 리터럴, 금지 import, 직접 import 계층)만 받는다.
	BuildTargets []string `json:"buildTargets"`

	// BuildTags 는 검사 시 켜 둘 빌드 태그다.
	BuildTags []string `json:"buildTags"`

	// Tests 는 verify.sh 가 도는 테스트 패키지 패턴이다. 계층 경로와 같은 파일에 둔다.
	// 스크립트에 ./internal/... 을 하드코딩하면 계층 정의가 두 곳이 되고, 하나를 고칠 때
	// 다른 하나를 빠뜨려 게이트가 조용히 약해진다. `archgate -print tests.fast` 로 읽는다.
	Tests TestPackages `json:"tests"`

	// Scenarios 는 시나리오 문서와 E2E 테스트의 대응 검사다. 비우면 검사하지 않는다.
	Scenarios ScenarioConfig `json:"scenarios"`

	// ExcludedPaths 는 게이트 밖에 두는 모듈 안 디렉터리다. 게이트 구현(tools/)처럼
	// 어느 계층에도 속하지 않는 것을 여기 적는다. 근거(reason)가 없으면 설정 오류다.
	//
	// 모듈 안의 Go 패키지는 정확히 하나의 계층에 속하거나 여기 있어야 한다.
	// 둘 다 아닌 패키지는 "검사 대상 아님" 이 아니라 설정 오류(종료 코드 2)다.
	// 이전 버전은 미선언 패키지를 조용히 건너뛰었고, 그래서 internal/rogue/ 하나를 만들면
	// 그 안의 금지 호출도, 그것을 향한 계층 의존도 게이트 밖이었다. 실제로 뚫린 구멍이다.
	// 계층에 속한 패키지가 여기 있는 패키지를 import 하는 것은 계층 위반이다.
	// 그렇지 않으면 코드를 tools/ 로 옮기는 것이 곧 우회가 된다.
	ExcludedPaths []ExcludedPath `json:"excludedPaths"`
}

// ExcludedPath 는 게이트 밖 디렉터리 하나다.
type ExcludedPath struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// TestPackages 는 등급별 테스트 패키지 패턴이다.
type TestPackages struct {
	// Fast 는 훅이 도는 빠른 등급이다. L1, L2.
	Fast []string `json:"fast"`
	// E2E 는 시나리오 테스트가 있는 곳이다. full 과 deep 에서 돈다.
	E2E []string `json:"e2e"`
}

// ScenarioConfig 는 시나리오 문서와 테스트 디렉터리의 위치다. 모듈 루트 기준 상대 경로다.
type ScenarioConfig struct {
	// Doc 은 시나리오 문서다. `## SC-01.` 형식의 제목에서 ID 를, `검증:` 줄에서 검증 지점 수를 뽑는다.
	Doc string `json:"doc"`
	// TestDir 은 E2E 테스트가 있는 디렉터리다. `func TestScenario_SC_01(` 을 찾는다.
	TestDir string `json:"testDir"`
}

// Layer 는 계층 하나의 정의다.
type Layer struct {
	// Path 는 모듈 경로 다음에 오는 디렉터리다. 이 접두사로 패키지가 어느 계층인지 정한다.
	Path string `json:"path"`

	// Allow 는 의존할 수 있는 계층 이름이다. 자기 계층은 항상 허용된다.
	// "*" 하나만 넣으면 전부 허용한다. 테스트 계층처럼 조립을 맡는 곳에 쓴다.
	Allow []string `json:"allow"`

	// NoSiblings 가 참이면 같은 계층 안의 다른 최상위 묶음끼리 참조를 금지한다.
	// 어댑터처럼 서로 몰라야 하는 것들을 담는 계층에 쓴다.
	NoSiblings bool `json:"noSiblings"`
}

const unknownLayer = ""

// LoadConfig 는 설정을 읽고 정합성을 검사한다.
func LoadConfig(p string) (*Config, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // 오타로 규칙이 조용히 꺼지는 것을 막는다
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s 를 읽을 수 없다: %w", p, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &c, nil
}

// Validate 는 설정의 정합성을 검사한다. 파일시스템이 필요 없으므로 픽스처로 검증할 수 있다.
func (c *Config) Validate() error {
	if c.Module == "" {
		return fmt.Errorf("module 이 없다")
	}
	if c.Module != path.Clean(c.Module) || strings.HasPrefix(c.Module, "/") {
		return fmt.Errorf("module %q 이 정규화된 경로가 아니다", c.Module)
	}
	if len(c.Layers) == 0 {
		return fmt.Errorf("layers 가 비어 있다. 계층이 없으면 게이트는 아무것도 검사하지 않는다")
	}
	if c.MaxParams <= 0 {
		c.MaxParams = 5
	}
	byPath := map[string]string{}
	for _, name := range c.LayerNames() {
		l := c.Layers[name]
		if l.Path == "" {
			return fmt.Errorf("계층 %q 에 path 가 없다", name)
		}
		if l.Path != path.Clean(l.Path) || strings.HasPrefix(l.Path, "/") || l.Path == "." || strings.HasPrefix(l.Path, "..") {
			return fmt.Errorf("계층 %q 의 path %q 가 정규화된 상대 경로가 아니다", name, l.Path)
		}
		if other, dup := byPath[l.Path]; dup {
			return fmt.Errorf("계층 %q 와 %q 의 path 가 같다(%s). 어느 계층인지 판정이 비결정적이 된다", other, name, l.Path)
		}
		byPath[l.Path] = name
		for _, a := range l.Allow {
			if a == "*" {
				continue
			}
			if _, ok := c.Layers[a]; !ok {
				return fmt.Errorf("계층 %q 의 allow 에 없는 계층 %q 가 있다", name, a)
			}
		}
	}
	check := func(field string, names []string) error {
		for _, s := range names {
			if _, ok := c.Layers[s]; !ok {
				return fmt.Errorf("%s 에 없는 계층 %q 가 있다", field, s)
			}
		}
		return nil
	}
	if err := check("shapeScope", c.ShapeScope); err != nil {
		return err
	}
	if err := check("infraLayers", c.InfraLayers); err != nil {
		return err
	}
	if err := check("allowInitLayers", c.AllowInitLayers); err != nil {
		return err
	}
	if len(c.ShapeScope) == 0 && !c.ShapeRulesDisabled {
		return fmt.Errorf("shapeScope 가 비어 있다. 규칙 A/B 를 끄려면 shapeRulesDisabled 를 true 로 둔다")
	}
	if len(c.ShapeScope) > 0 && c.ShapeRulesDisabled {
		return fmt.Errorf("shapeScope 와 shapeRulesDisabled 가 동시에 설정됐다")
	}
	switch c.TestImportPolicy {
	case "":
		c.TestImportPolicy = "same"
	case "same", "any":
	default:
		return fmt.Errorf("testImportPolicy %q 는 same 또는 any 여야 한다", c.TestImportPolicy)
	}
	for _, t := range c.BuildTargets {
		os, arch, ok := strings.Cut(t, "/")
		if !ok || os == "" || arch == "" {
			return fmt.Errorf("buildTargets 항목 %q 는 GOOS/GOARCH 형식이어야 한다", t)
		}
	}
	for key := range c.ForbiddenSelectors {
		if i := strings.LastIndex(key, "."); i <= 0 || i == len(key)-1 {
			return fmt.Errorf("forbiddenSelectors 열쇠 %q 는 import경로.식별자 형식이어야 한다", key)
		}
	}
	if len(c.Tests.Fast) == 0 {
		return fmt.Errorf("tests.fast 가 비어 있다. 훅이 아무 테스트도 돌리지 않게 된다")
	}
	if len(c.Tests.E2E) == 0 {
		return fmt.Errorf("tests.e2e 가 비어 있다. 완료 판정의 근거인 E2E 가 어디에도 없다")
	}
	for _, p := range append(append([]string{}, c.Tests.Fast...), c.Tests.E2E...) {
		if !strings.HasPrefix(p, "./") {
			return fmt.Errorf("tests 의 패키지 패턴 %q 는 ./ 로 시작하는 상대 패턴이어야 한다", p)
		}
	}
	if (c.Scenarios.Doc == "") != (c.Scenarios.TestDir == "") {
		return fmt.Errorf("scenarios 는 doc 과 testDir 을 함께 적는다")
	}
	if c.Scenarios.TestDir != "" {
		// 시나리오 대응 검사는 testDir 을 읽고, E2E 실행은 tests.e2e 를 돈다. 둘이 어긋나면
		// 문서에 대응된 테스트가 실제로는 한 번도 실행되지 않은 채 완료 판정이 난다.
		// 실행 시점의 대조(-print scenarios → testreport -scenarios)가 최종 보루이고,
		// 이것은 그 앞에서 설정 오류를 일찍 잡는 것이다.
		if !patternsCover(c.Tests.E2E, c.Scenarios.TestDir) {
			return fmt.Errorf("scenarios.testDir %q 가 tests.e2e %v 에 포함되지 않는다. 시나리오 테스트가 E2E 에서 돌지 않는다", c.Scenarios.TestDir, c.Tests.E2E)
		}
	}
	seenEx := map[string]bool{}
	for _, e := range c.ExcludedPaths {
		if e.Path == "" || e.Path != path.Clean(e.Path) || strings.HasPrefix(e.Path, "/") || e.Path == "." || strings.HasPrefix(e.Path, "..") {
			return fmt.Errorf("excludedPaths 의 path %q 가 정규화된 상대 경로가 아니다", e.Path)
		}
		if strings.TrimSpace(e.Reason) == "" {
			return fmt.Errorf("excludedPaths %q 에 reason 이 없다. 근거 없는 예외는 범위를 넓히는 근거가 된다", e.Path)
		}
		if seenEx[e.Path] {
			return fmt.Errorf("excludedPaths 에 %q 가 두 번 있다", e.Path)
		}
		seenEx[e.Path] = true
		for _, name := range c.LayerNames() {
			lp := c.Layers[name].Path
			if lp == e.Path || strings.HasPrefix(lp, e.Path+"/") || strings.HasPrefix(e.Path, lp+"/") {
				return fmt.Errorf("excludedPaths %q 가 계층 %q 의 path %q 와 겹친다. 계층이면서 게이트 밖일 수는 없다", e.Path, name, lp)
			}
		}
	}
	return nil
}

// patternsCover 는 ./dir 또는 ./dir/... 형식의 go 패키지 패턴 중 하나가 dir 을 포함하는지 본다.
func patternsCover(patterns []string, dir string) bool {
	for _, p := range patterns {
		p = strings.TrimPrefix(p, "./")
		if base, ok := strings.CutSuffix(p, "/..."); ok {
			if base == "" || base == "." || base == dir || strings.HasPrefix(dir, base+"/") {
				return true
			}
			continue
		}
		if p == dir {
			return true
		}
	}
	return false
}

// IsExcluded 는 패키지가 excludedPaths 아래에 있는지 본다.
func (c *Config) IsExcluded(pkg string) bool {
	rel, ok := strings.CutPrefix(pkg, c.Module+"/")
	if !ok {
		return false
	}
	for _, e := range c.ExcludedPaths {
		if rel == e.Path || strings.HasPrefix(rel, e.Path+"/") {
			return true
		}
	}
	return false
}

// LayerOf 는 패키지 경로가 속한 계층 이름을 돌려준다.
// 모듈 밖이면 빈 문자열이다. 접두사가 겹치면 더 긴 쪽이 이긴다.
//
// 표준 라이브러리인지 서드파티인지는 여기서 묻지 않는다. 모듈 안의 계층이 아니면
// 전부 바깥 의존이다. 경로 모양으로 표준 라이브러리를 추정하면 `module app` 같은
// 점 없는 모듈 경로가 통째로 검사 밖이 된다.
func (c *Config) LayerOf(pkg string) string {
	rel, ok := strings.CutPrefix(pkg, c.Module+"/")
	if !ok {
		if pkg == c.Module {
			rel = ""
		} else {
			return unknownLayer
		}
	}
	best, bestLen := unknownLayer, -1
	for _, name := range c.LayerNames() {
		l := c.Layers[name]
		if rel == l.Path || strings.HasPrefix(rel, l.Path+"/") {
			if len(l.Path) > bestLen {
				best, bestLen = name, len(l.Path)
			}
		}
	}
	return best
}

// InModule 은 패키지 경로가 이 모듈 안인지 본다.
func (c *Config) InModule(pkg string) bool {
	return pkg == c.Module || strings.HasPrefix(pkg, c.Module+"/")
}

// Allows 는 from 계층이 to 계층에 의존할 수 있는지 판정한다.
func (c *Config) Allows(from, to string) bool {
	if from == to {
		return true
	}
	l, ok := c.Layers[from]
	if !ok {
		return false
	}
	for _, a := range l.Allow {
		if a == "*" || a == to {
			return true
		}
	}
	return false
}

// InScope 는 규칙 A 와 B 의 적용 대상 계층인지 본다.
func (c *Config) InScope(layer string) bool {
	return contains(c.ShapeScope, layer)
}

// IsInfra 는 금지 목록에서 면제되는 계층인지 본다.
func (c *Config) IsInfra(layer string) bool {
	return contains(c.InfraLayers, layer)
}

// AllowsInit 는 init 함수가 허용되는 계층인지 본다.
func (c *Config) AllowsInit(layer string) bool {
	return contains(c.AllowInitLayers, layer)
}

// siblingOf 는 계층 경로 다음의 첫 구간을 돌려준다. NoSiblings 판정에 쓴다.
func (c *Config) siblingOf(pkg string) string {
	layer := c.LayerOf(pkg)
	l, ok := c.Layers[layer]
	if !ok {
		return ""
	}
	rel, ok := strings.CutPrefix(pkg, c.Module+"/"+l.Path+"/")
	if !ok {
		return ""
	}
	first, _, _ := strings.Cut(rel, "/")
	return first
}

// importViolation 은 from 패키지가 to 패키지를 (직접 또는 전이적으로) 의존할 때
// 계층 규칙에 어긋나면 그 이유를 돌려준다. 어긋나지 않으면 빈 문자열이다.
// 직접 import 검사와 폐포 검사가 같은 판정을 쓴다.
func (c *Config) importViolation(from, to string) string {
	if from == to || !c.InModule(to) {
		return ""
	}
	self := c.LayerOf(from)
	dep := c.LayerOf(to)
	if self == unknownLayer {
		return ""
	}
	if dep == unknownLayer {
		// 모듈 안인데 계층이 없는 패키지는 excludedPaths 거나(게이트 밖) 미선언(설정 오류로 이미 2)이다.
		// 어느 쪽이든 계층 코드가 거기 의존하면 게이트 밖으로 의존이 새는 것이다.
		return fmt.Sprintf("%s 계층이 계층 밖 패키지 %s 에 의존한다. 게이트 밖(excludedPaths)으로 의존을 내보내지 않는다", self, to)
	}
	if !c.Allows(self, dep) {
		return fmt.Sprintf("%s 계층이 %s 계층 %s 에 의존한다", self, dep, to)
	}
	if self == dep && c.Layers[self].NoSiblings {
		a, b := c.siblingOf(from), c.siblingOf(to)
		if a != "" && b != "" && a != b {
			return fmt.Sprintf("%s 안의 %s 가 다른 묶음 %s 에 의존한다", self, a, b)
		}
	}
	return ""
}

// LayerNames 는 보고를 안정시키기 위한 정렬된 계층 이름이다.
func (c *Config) LayerNames() []string {
	out := make([]string, 0, len(c.Layers))
	for k := range c.Layers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

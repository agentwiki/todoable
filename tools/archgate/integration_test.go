package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 통합 자기검증. 순수 판정 함수가 아니라 파일 발견, export 데이터, go.mod 대조, 종료 코드까지
// 실제 임시 모듈 위에서 돈다.
//
// 이전 버전에서 심각한 결함이 발견된 곳은 전부 순수 함수 테스트의 범위 밖이었다.
// "발견된 파일을 검사할 수 있다" 와 "파일을 빠짐없이 발견한다" 는 다른 주장이고,
// 후자는 디스크 위에서만 증명된다. 아래 각 사례는 실제로 뚫렸던 것이다.

const modName = "example.com/app"

// module 은 임시 디렉터리에 Go 모듈 하나를 만든다. files 의 열쇠는 루트 기준 상대 경로다.
func module(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	base := map[string]string{
		"go.mod":        "module " + modName + "\n\ngo 1.22\n",
		"archgate.json": defaultJSON,
	}
	// 계층 path 는 디렉터리로 존재해야 한다. 비어 있어도 된다.
	for _, dir := range []string{"internal/domain", "internal/ports", "internal/usecases", "internal/adapters", "cmd", "test"} {
		base[dir+"/.keep"] = ""
	}
	for k, v := range files {
		base[k] = v
	}
	for name, src := range base {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const defaultJSON = `{
  "module": "example.com/app",
  "layers": {
    "domain":   {"path": "internal/domain",   "allow": []},
    "ports":    {"path": "internal/ports",    "allow": ["domain"]},
    "usecases": {"path": "internal/usecases", "allow": ["domain", "ports"]},
    "adapters": {"path": "internal/adapters", "allow": ["domain", "ports"], "noSiblings": true},
    "main":     {"path": "cmd",               "allow": ["*"]},
    "test":     {"path": "test",              "allow": ["*"]}
  },
  "shapeScope": ["usecases"],
  "maxParams": 5,
  "infraLayers": ["adapters"],
  "forbiddenSelectors": {"time.Now": "Clock 포트를 쓴다"},
  "forbiddenImports": {"math/rand": "Random 포트를 쓴다"},
  "forbiddenLiterals": ["/etc/secret"],
  "tests": {"fast": ["./internal/...", "./cmd/..."], "e2e": ["./test/..."]}
}
`

// gate 는 모듈 루트에서 archgate 를 돌리고 종료 코드와 stderr 를 돌려준다.
func gate(t *testing.T, root string, args ...string) (int, string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	var stdout, stderr bytes.Buffer
	code := run(filepath.Join(root, "archgate.json"), root, false, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 명령이 없다")
	}
}

func TestIntegration_CleanModulePasses(t *testing.T) {
	requireGo(t)
	root := module(t, map[string]string{
		"internal/domain/d.go":   "package domain\n\ntype ID string\n",
		"internal/usecases/u.go": "package usecases\n\nimport \"example.com/app/internal/domain\"\n\nfunc Use(id domain.ID) domain.ID { return id }\n",
	})
	code, out := gate(t, root)
	if code != 0 {
		t.Fatalf("정상 모듈이 통과하지 못했다(code=%d):\n%s", code, out)
	}
}

// go list ./... 는 현재 GOOS 에서 활성 파일이 없는 패키지를 보고하지 않는다.
// 그 패키지의 금지 호출과 계층 위반은 파일시스템 순회로만 발견된다.
func TestIntegration_TagExcludedOnlyPackageIsDiscovered(t *testing.T) {
	requireGo(t)
	root := module(t, map[string]string{
		"internal/usecases/w.go":    "//go:build plan9\n\npackage usecases\n\nimport (\n\t\"time\"\n\t\"example.com/app/internal/adapters/fs\"\n)\n\nvar _ = fs.New\nvar _ = time.Now\n",
		"internal/adapters/fs/f.go": "package fs\n\nfunc New() {}\n",
	})
	code, out := gate(t, root)
	if code != 1 {
		t.Fatalf("배제 파일만 있는 패키지가 검사되지 않았다(code=%d):\n%s", code, out)
	}
	for _, want := range []string{"layer", "call"} {
		if !strings.Contains(out, want) {
			t.Errorf("규칙 %s 위반이 보고되지 않았다:\n%s", want, out)
		}
	}
}

// cgo 파일은 GoFiles 가 아니라 CgoFiles 에 들어간다. 읽지 않으면 그 파일이 통째로 게이트 밖이다.
func TestIntegration_CgoFileIsChecked(t *testing.T) {
	requireGo(t)
	root := module(t, map[string]string{
		"internal/usecases/c.go": "package usecases\n\n/*\n#include <stdlib.h>\n*/\nimport \"C\"\nimport \"time\"\n\nfunc Now() time.Time { return time.Now() }\n",
	})
	code, out := gate(t, root)
	if code != 1 || !strings.Contains(out, "time.Now") {
		t.Fatalf("cgo 파일의 time.Now 가 잡히지 않았다(code=%d):\n%s", code, out)
	}
}

// 테스트 전용 import 는 go list 의 Deps 에 없다. 정책이 same 이면 파일 단위로 잡아야 한다.
func TestIntegration_TestOnlyImportFollowsPolicy(t *testing.T) {
	requireGo(t)
	files := map[string]string{
		"internal/usecases/u.go":      "package usecases\n\nfunc F() int { return 1 }\n",
		"internal/usecases/u_test.go": "package usecases\n\nimport (\n\t\"testing\"\n\t\"example.com/app/internal/adapters/fs\"\n)\n\nfunc TestF(t *testing.T) { fs.New(); _ = F() }\n",
		"internal/adapters/fs/f.go":   "package fs\n\nfunc New() {}\n",
	}
	code, out := gate(t, module(t, files))
	if code != 1 || !strings.Contains(out, "layer") {
		t.Fatalf("testImportPolicy=same 인데 테스트의 어댑터 import 가 통과했다(code=%d):\n%s", code, out)
	}
	files["archgate.json"] = strings.Replace(defaultJSON, `"maxParams": 5,`, `"maxParams": 5, "testImportPolicy": "any",`, 1)
	code, out = gate(t, module(t, files))
	if code != 0 {
		t.Fatalf("testImportPolicy=any 인데 실패했다(code=%d):\n%s", code, out)
	}
}

// 점 없는 모듈 경로도 유효하다. 표준 라이브러리 추정으로 모듈 안 패키지가 빠지면 안 된다.
func TestIntegration_DotlessModulePath(t *testing.T) {
	requireGo(t)
	root := module(t, map[string]string{
		"go.mod":                    "module app\n\ngo 1.22\n",
		"archgate.json":             strings.Replace(defaultJSON, `"module": "example.com/app"`, `"module": "app"`, 1),
		"internal/usecases/u.go":    "package usecases\n\nimport \"app/internal/adapters/fs\"\n\nvar _ = fs.New\n",
		"internal/adapters/fs/f.go": "package fs\n\nfunc New() {}\n",
	})
	code, out := gate(t, root)
	if code != 1 || !strings.Contains(out, "layer") {
		t.Fatalf("점 없는 모듈의 계층 위반이 잡히지 않았다(code=%d):\n%s", code, out)
	}
}

// 전이 의존은 폐포로 잡는다. usecases → ports 는 허용, ports 가 몰래 adapters 를 끌면 usecases 에서 보인다.
func TestIntegration_TransitiveViolation(t *testing.T) {
	requireGo(t)
	root := module(t, map[string]string{
		"internal/ports/p.go":       "package ports\n\nimport \"example.com/app/internal/adapters/fs\"\n\nvar _ = fs.New\n\ntype P interface{ Do() }\n",
		"internal/usecases/u.go":    "package usecases\n\nimport \"example.com/app/internal/ports\"\n\nfunc Use(p ports.P) { p.Do() }\n",
		"internal/adapters/fs/f.go": "package fs\n\nfunc New() {}\n",
	})
	code, out := gate(t, root)
	if code != 1 || !strings.Contains(out, "전이 의존") {
		t.Fatalf("전이 의존 위반이 보고되지 않았다(code=%d):\n%s", code, out)
	}
}

// 설정 오류는 위반(1)이 아니라 판정 불능(2)이다. 게이트가 눈을 감은 것을 통과로 보고하지 않는다.
func TestIntegration_ConfigErrorsExitTwo(t *testing.T) {
	requireGo(t)
	cases := map[string]string{
		"빈 layers":   strings.Replace(defaultJSON, `"shapeScope": ["usecases"]`, `"shapeScope": []`, 1),
		"module 불일치": strings.Replace(defaultJSON, `"module": "example.com/app"`, `"module": "example.com/other"`, 1),
		"모르는 키(오타)":  strings.Replace(defaultJSON, `"forbiddenLiterals"`, `"forbiddenLiteral"`, 1),
		"없는 계층 디렉터리": strings.Replace(defaultJSON, `"path": "internal/domain"`, `"path": "internal/nowhere"`, 1),
	}
	for name, cfg := range cases {
		root := module(t, map[string]string{
			"archgate.json":          cfg,
			"internal/domain/d.go":   "package domain\n",
			"internal/usecases/u.go": "package usecases\n",
		})
		if code, out := gate(t, root); code != 2 {
			t.Errorf("%s: 종료 코드 2 여야 한다. got=%d\n%s", name, code, out)
		}
	}
}

// 규칙 B 가 모듈 안 다른 패키지의 인터페이스를 export 데이터로 본다.
// importer 가 모듈 패키지를 못 풀면 타입 검사가 실패하고, 그것은 조용한 통과가 아니라 2 여야 한다.
func TestIntegration_CrossPackageInterfaceIsResolved(t *testing.T) {
	requireGo(t)
	root := module(t, map[string]string{
		"internal/ports/p.go":    "package ports\n\ntype FS interface{ Read(string) error }\ntype Meta interface{ List() error }\n",
		"internal/usecases/u.go": "package usecases\n\nimport \"example.com/app/internal/ports\"\n\ntype Deps struct {\n\tF ports.FS\n\tM ports.Meta\n}\n\nfunc A(d Deps) error { return d.F.Read(\"x\") }\nfunc B(d Deps) error { return d.M.List() }\n",
	})
	code, out := gate(t, root)
	if code != 1 || !strings.Contains(out, "B-a") {
		t.Fatalf("다른 패키지 인터페이스로 된 협력자 묶음이 잡히지 않았다(code=%d):\n%s", code, out)
	}
}

// 시나리오 대응 검사가 실제 파일에서 돈다.
func TestIntegration_ScenarioManifest(t *testing.T) {
	requireGo(t)
	cfg := strings.Replace(defaultJSON, `"tests":`, `"scenarios": {"doc": "docs/scenarios.md", "testDir": "test"},
  "tests":`, 1)
	root := module(t, map[string]string{
		"archgate.json":        cfg,
		"docs/scenarios.md":    "# 시나리오\n\n## SC-01. 첫 흐름\n\n1. 한다\n   검증: 된다\n2. 또 한다\n   검증: 또 된다\n\n## SC-02. 둘째 흐름\n\n1. 한다\n   검증: 된다\n",
		"internal/domain/d.go": "package domain\n",
		"test/sc_test.go":      "package test\n\nimport \"testing\"\n\nfunc TestScenario_SC_01(t *testing.T) { _ = \"V-01\" }\n",
	})
	code, out := gate(t, root)
	if code != 1 {
		t.Fatalf("시나리오 불일치가 위반으로 보고되지 않았다(code=%d):\n%s", code, out)
	}
	for _, want := range []string{"SC-02", "V-02"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s 누락이 보고되지 않았다:\n%s", want, out)
		}
	}
}

// 선언되지 않은 패키지는 "검사 대상 아님" 이 아니라 설정 오류다.
// 이전 버전은 조용히 건너뛰었고, internal/rogue/ 하나로 그 안의 금지 호출도 그것을 향한 의존도 게이트 밖이었다.
func TestIntegration_UnlayeredPackageIsConfigError(t *testing.T) {
	requireGo(t)
	root := module(t, map[string]string{
		"internal/rogue/r.go":    "package rogue\n\nimport \"time\"\n\nfunc Now() time.Time { return time.Now() }\n",
		"internal/usecases/u.go": "package usecases\n\nimport \"example.com/app/internal/rogue\"\n\nvar _ = rogue.Now\n",
	})
	code, out := gate(t, root)
	if code != 2 || !strings.Contains(out, "internal/rogue") {
		t.Fatalf("미선언 패키지가 설정 오류(2)로 거부되지 않았다(code=%d):\n%s", code, out)
	}
}

// excludedPaths 는 그 디렉터리를 게이트 밖에 두지만, 계층 코드가 거기 의존하는 것은 계층 위반이다.
// 그렇지 않으면 코드를 tools/ 로 옮기는 것이 곧 우회다.
func TestIntegration_ExcludedPathIsOutsideButNotImportable(t *testing.T) {
	requireGo(t)
	cfg := strings.Replace(defaultJSON, `"tests":`, `"excludedPaths": [{"path": "tools", "reason": "게이트 구현"}],
  "tests":`, 1)
	files := map[string]string{
		"archgate.json":     cfg,
		"tools/helper/h.go": "package helper\n\nimport \"time\"\n\nfunc H() time.Time { return time.Now() }\n",
	}
	if code, out := gate(t, module(t, files)); code != 0 {
		t.Fatalf("excludedPaths 의 금지 호출이 검사됐다(code=%d):\n%s", code, out)
	}
	files["internal/usecases/u.go"] = "package usecases\n\nimport \"example.com/app/tools/helper\"\n\nvar _ = helper.H\n"
	if code, out := gate(t, module(t, files)); code != 1 || !strings.Contains(out, "계층 밖 패키지") {
		t.Fatalf("게이트 밖 패키지로의 의존이 위반으로 잡히지 않았다(code=%d):\n%s", code, out)
	}
}

// 빌드 제약으로 배제된 시나리오 테스트는 존재하지만 실행되지 않는다. 대응으로 인정하면 안 된다.
func TestIntegration_BuildTagExcludedScenarioTestIsNotMapping(t *testing.T) {
	requireGo(t)
	cfg := strings.Replace(defaultJSON, `"tests":`, `"scenarios": {"doc": "docs/scenarios.md", "testDir": "test"},
  "tests":`, 1)
	root := module(t, map[string]string{
		"archgate.json":     cfg,
		"docs/scenarios.md": "## SC-01. 첫 흐름\n\n1. 한다\n   검증: 된다\n\n## SC-02. 둘째 흐름\n\n1. 한다\n   검증: 된다\n",
		"test/a_test.go":    "package test\n\nimport \"testing\"\n\nfunc TestScenario_SC_01(t *testing.T) { _ = \"V-01\" }\n",
		"test/b_test.go":    "//go:build plan9\n\npackage test\n\nimport \"testing\"\n\nfunc TestScenario_SC_02(t *testing.T) { _ = \"V-01\" }\n",
	})
	code, out := gate(t, root)
	if code != 1 || !strings.Contains(out, "빌드 제약") || !strings.Contains(out, "TestScenario_SC_02 가 없다") {
		t.Fatalf("배제된 시나리오 테스트가 대응으로 인정됐다(code=%d):\n%s", code, out)
	}
}

// buildTags 는 export 데이터(go list)에도 전달돼야 한다. 아니면 태그 뒤의 선언을 참조하는 패키지가
// 판정 불능(2)이 된다. 잘못된 빨간색도 게이트를 약화시킨다.
func TestIntegration_BuildTagsReachExportData(t *testing.T) {
	requireGo(t)
	cfg := strings.Replace(defaultJSON, `"maxParams": 5,`, `"maxParams": 5, "buildTags": ["special"],`, 1)
	root := module(t, map[string]string{
		"archgate.json":           cfg,
		"internal/ports/fs_sp.go": "//go:build special\n\npackage ports\n\ntype FS interface{ Read() }\n",
		"internal/usecases/u.go":  "package usecases\n\nimport \"example.com/app/internal/ports\"\n\ntype Deps struct{ FS ports.FS }\n\nfunc F(d Deps) { d.FS.Read() }\n",
	})
	code, out := gate(t, root)
	if code != 0 {
		t.Fatalf("buildTags 가 export 데이터에 전달되지 않았다(code=%d):\n%s", code, out)
	}
}

// -print scenarios 는 문서의 시나리오와 검증 줄 수를 testreport 가 읽는 형식으로 낸다.
func TestIntegration_PrintScenarios(t *testing.T) {
	requireGo(t)
	cfg := strings.Replace(defaultJSON, `"tests":`, `"scenarios": {"doc": "docs/scenarios.md", "testDir": "test"},
  "tests":`, 1)
	root := module(t, map[string]string{
		"archgate.json":     cfg,
		"docs/scenarios.md": "## SC-01. 첫\n\n1. a\n   검증: x\n2. b\n   검증: y\n\n## SC-02. 둘\n\n1. a\n   검증: x\n",
	})
	var stdout, stderr bytes.Buffer
	if code := printValue(filepath.Join(root, "archgate.json"), root, "scenarios", &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d\n%s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "SC-01=2 SC-02=1" {
		t.Fatalf("scenarios 출력이 틀렸다: %q", got)
	}
}

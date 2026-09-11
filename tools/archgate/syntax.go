package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// Violation 은 규칙 위반 한 건이다.
type Violation struct {
	Rule string
	Pos  string
	Msg  string
}

func (v Violation) String() string { return fmt.Sprintf("%-6s %s  %s", v.Rule, v.Pos, v.Msg) }

// FileInput 은 파일 하나에 대한 구문 검사 입력이다.
type FileInput struct {
	Filename string
	Src      []byte
	Pkg      string // import 경로
	Layer    string
	IsTest   bool
}

// CheckFileSyntax 는 빌드 컨텍스트와 무관하게 파일 하나에서 판정할 수 있는 것을 본다.
//
//   - 금지 import
//   - init 부수효과
//   - 금지 문자열 리터럴
//   - 직접 import 의 계층 위반 (허용 목록과 NoSiblings 둘 다)
//
// 빌드 태그로 배제된 파일, cgo 파일, 테스트 파일에도 똑같이 적용된다.
// 배제된 파일은 의존 폐포에 나타나지 않으므로 이 검사가 그 파일의 유일한 계층 검사다.
func CheckFileSyntax(c *Config, in FileInput) ([]Violation, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, in.Filename, in.Src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	pos := func(p token.Pos) string {
		return fmt.Sprintf("%s:%d", in.Filename, fset.Position(p).Line)
	}
	var out []Violation
	infra := c.IsInfra(in.Layer)
	layerChecked := in.Layer != unknownLayer && (!in.IsTest || c.TestImportPolicy == "same")

	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if !infra {
			if why, bad := c.ForbiddenImports[path]; bad {
				out = append(out, Violation{"import", pos(imp.Pos()), path + " 를 import 한다. " + why})
			}
		}
		if layerChecked {
			if why := c.importViolation(in.Pkg, path); why != "" {
				out = append(out, Violation{"layer", pos(imp.Pos()), why})
			}
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			if node.Recv == nil && node.Name.Name == "init" && !c.AllowsInit(in.Layer) {
				out = append(out, Violation{"init", pos(node.Pos()),
					"init 함수를 정의한다. 조립은 합성 루트에서 명시적으로 한다"})
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING || len(c.ForbiddenLiterals) == 0 {
				return true
			}
			// 이스케이프를 푼 값으로 본다. 원문 그대로 보면 백슬래시 이중화를 놓친다.
			text, err := strconv.Unquote(node.Value)
			if err != nil {
				text = node.Value
			}
			for _, bad := range c.ForbiddenLiterals {
				if strings.Contains(text, bad) {
					out = append(out, Violation{"lit", pos(node.Pos()),
						"금지된 문자열 " + bad + " 이 리터럴에 있다"})
				}
			}
		}
		return true
	})
	return out, nil
}

var versionSuffix = regexp.MustCompile(`^v[0-9]+$`)

// localImportNames 는 지역 이름에서 import 경로로 가는 표를 만든다.
//
// 타입 정보가 없는 파일(빌드 컨텍스트 밖의 파일)에만 쓴다. 기본 지역 이름은
// 경로의 마지막 구간이되, `math/rand/v2` 처럼 마지막 구간이 메이저 버전이면 그 앞 구간이다.
// 패키지 선언이 경로와 다른 경우는 소스만으로 알 수 없다. 그 한계 때문에 활성 파일은
// 타입 정보로 판정한다. 두 번째 반환값은 점 import 된 경로다.
func localImportNames(f *ast.File) (map[string]string, []string) {
	byLocal := map[string]string{}
	var dotted []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		switch {
		case imp.Name == nil:
			parts := strings.Split(path, "/")
			last := parts[len(parts)-1]
			if versionSuffix.MatchString(last) && len(parts) > 1 {
				last = parts[len(parts)-2]
			}
			byLocal[last] = path
		case imp.Name.Name == ".":
			dotted = append(dotted, path)
		case imp.Name.Name == "_":
		default:
			byLocal[imp.Name.Name] = path
		}
	}
	return byLocal, dotted
}

// CheckSelectorsSyntactic 은 타입 정보 없이 금지 선택자를 본다.
//
// 활성 파일은 CheckSelectorsTyped 가 맡는다. 이 함수는 현재 빌드 컨텍스트에서 컴파일되지
// 않아 타입을 붙일 수 없는 파일의 차선책이다. 지역 변수가 import 이름을 가리면 오탐하고,
// 패키지 선언 이름이 경로와 다르면 미탐한다. 그 한계를 없애려면 buildTargets 에 그 조합을
// 넣어 타입 검사를 받게 한다.
func CheckSelectorsSyntactic(c *Config, in FileInput) ([]Violation, error) {
	if c.IsInfra(in.Layer) {
		return nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, in.Filename, in.Src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	pos := func(p token.Pos) string {
		return fmt.Sprintf("%s:%d", in.Filename, fset.Position(p).Line)
	}
	var out []Violation
	byLocal, dotted := localImportNames(f)
	for _, path := range dotted {
		for key, why := range c.ForbiddenSelectors {
			if strings.HasPrefix(key, path+".") {
				out = append(out, Violation{"import", pos(f.Pos()),
					path + " 를 점 import 한다. 타입 정보 없이는 선택자를 판정할 수 없다. " + why})
				break
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		path, ok := byLocal[id.Name]
		if !ok {
			return true
		}
		if why, bad := c.ForbiddenSelectors[path+"."+sel.Sel.Name]; bad {
			out = append(out, Violation{"call", pos(sel.Pos()), path + "." + sel.Sel.Name + " 를 쓴다. " + why})
		}
		return true
	})
	return out, nil
}

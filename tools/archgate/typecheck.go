package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
)

// Checked 는 타입 검사를 마친 패키지 한 벌이다. 규칙 A, B 와 금지 선택자 판정의 입력이다.
type Checked struct {
	Path   string
	Layer  string
	Fset   *token.FileSet
	Files  []*ast.File
	Pkg    *types.Package
	Info   *types.Info
	Errors []error // lenient 모드에서 모아 둔 타입 오류
}

// ParsedFile 은 파일 이름과 소스다. 파일 이름은 보고에 쓰이는 상대 경로다.
type ParsedFile struct {
	Name string
	Src  []byte
}

// TypeCheck 는 파일들을 하나의 패키지로 파싱하고 타입을 붙인다.
//
// 판정을 문자열이 아니라 타입으로 하는 이유: 별칭(type X = Y), 제네릭 인스턴스, 배열과 슬라이스,
// 외부 패키지 인터페이스, 가리기(shadowing)를 문자열 비교는 전부 틀린다.
// 게이트가 오탐하면 약화되고, 미탐하면 리뷰어가 "기계 판정은 끝났다" 고 믿은 채 지나친다.
//
// strict 가 참이면 타입 오류 하나도 허용하지 않는다. 빌드가 통과한 파일이라면 오류가 없어야 하고,
// 있다면 게이트가 판정 능력을 잃은 것이므로 조용히 통과시키지 않고 실패로 올린다.
// 테스트 변형은 export_test 패턴 때문에 완전한 타입 검사가 어려울 수 있어 lenient 로 돈다.
func TypeCheck(path string, files []ParsedFile, imp types.Importer, strict bool) (*Checked, error) {
	fset := token.NewFileSet()
	var asts []*ast.File
	for _, f := range files {
		a, err := parser.ParseFile(fset, f.Name, f.Src, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		asts = append(asts, a)
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	ch := &Checked{Path: path, Fset: fset, Files: asts, Info: info}
	conf := types.Config{
		Importer:    imp,
		FakeImportC: true, // cgo 파일도 검사 대상이다. C 심볼은 실제 타입이 필요 없다
		Error: func(err error) {
			ch.Errors = append(ch.Errors, err)
		},
	}
	pkg, _ := conf.Check(path, fset, asts, info)
	ch.Pkg = pkg
	if strict && len(ch.Errors) > 0 {
		return nil, fmt.Errorf("%s 타입 검사 실패(%d 건). 첫 오류: %v", path, len(ch.Errors), ch.Errors[0])
	}
	return ch, nil
}

// position 은 보고용 위치 문자열이다.
func (ch *Checked) position(p token.Pos) string {
	pos := ch.Fset.Position(p)
	return fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
}

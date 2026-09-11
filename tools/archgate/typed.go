package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// CheckSelectorsTyped 는 타입 정보로 금지 선택자를 본다.
//
// 식별자가 실제로 어느 패키지의 어느 객체를 가리키는지 types.Info.Uses 로 확인한다.
// 별칭 import, 점 import, 지역 변수 가리기, 경로 끝과 패키지 이름이 다른 모듈이 전부 같은 판정을 받는다.
// 타입이 붙지 않은 식별자(lenient 모드의 테스트 파일)는 import 표로 차선 판정한다.
//
// only 가 비어 있지 않으면 그 파일 이름만 본다. 테스트 변형을 검사할 때 비테스트 파일을
// 두 번 보고하지 않기 위한 것이다.
func CheckSelectorsTyped(c *Config, ch *Checked, only map[string]bool) []Violation {
	if c.IsInfra(ch.Layer) {
		return nil
	}
	var out []Violation
	for _, f := range ch.Files {
		name := ch.Fset.Position(f.Pos()).Filename
		if len(only) > 0 && !only[name] {
			continue
		}
		byLocal, _ := localImportNames(f)
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				obj := ch.Info.Uses[x]
				if obj == nil || obj.Pkg() == nil || obj.Parent() != obj.Pkg().Scope() {
					return true
				}
				key := obj.Pkg().Path() + "." + obj.Name()
				if why, bad := c.ForbiddenSelectors[key]; bad {
					out = append(out, Violation{"call", ch.position(x.Pos()), key + " 를 쓴다. " + why})
				}
			case *ast.SelectorExpr:
				// 타입이 붙지 않은 선택자만 차선 판정한다. 붙은 것은 위 Ident 분기가 처리한다.
				id, ok := x.X.(*ast.Ident)
				if !ok || ch.Info.Uses[id] != nil || ch.Info.Uses[x.Sel] != nil {
					return true
				}
				if path, ok := byLocal[id.Name]; ok {
					key := path + "." + x.Sel.Name
					if why, bad := c.ForbiddenSelectors[key]; bad {
						out = append(out, Violation{"call", ch.position(x.Pos()), key + " 를 쓴다(구문 판정). " + why})
					}
				}
			}
			return true
		})
	}
	return out
}

// CheckSignatureShape 는 규칙 A 를 담당한다.
//
//   - 인자가 MaxParams 개 이상이면 위반. 옵션 구조체를 쓰라는 뜻이다.
//   - 같은 타입 인자가 2개 이상이면 위반. 순서를 바꿔도 컴파일되는 자리를 없앤다.
//
// "같은 타입" 은 types.Identical 로 판정한다. `type Text = string` 은 string 과 같고,
// `[2]int` 와 `[]int` 는 다르며, `Box[int]` 와 `Other[string]` 은 다르다.
// 첫 인자가 context.Context 면 개수에서 뺀다. 관용이고 구조체에 넣으면 오히려 나쁘다.
// 가변 인자는 개수에서 뺀다. 메서드의 수신자는 인자가 아니므로 세지 않는다.
func CheckSignatureShape(c *Config, ch *Checked) []Violation {
	if !c.InScope(ch.Layer) {
		return nil
	}
	var out []Violation
	qual := types.RelativeTo(ch.Pkg)
	for _, f := range ch.Files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			obj, _ := ch.Info.Defs[fn.Name].(*types.Func)
			if obj == nil {
				continue
			}
			sig := obj.Type().(*types.Signature)
			var params []*types.Var
			for i := 0; i < sig.Params().Len(); i++ {
				p := sig.Params().At(i)
				if i == 0 && isContext(p.Type()) {
					continue
				}
				if sig.Variadic() && i == sig.Params().Len()-1 {
					continue
				}
				params = append(params, p)
			}
			if len(params) >= c.MaxParams {
				out = append(out, Violation{"A", ch.position(fn.Pos()),
					fmt.Sprintf("%s 의 인자가 %d 개다. %d 개 이상이면 옵션 구조체를 쓴다",
						fn.Name.Name, len(params), c.MaxParams)})
			}
			// 같은 타입끼리 묶는다. 대표 하나를 두고 Identical 로 비교한다.
			type group struct {
				t types.Type
				n int
			}
			var groups []*group
		next:
			for _, p := range params {
				for _, g := range groups {
					if types.Identical(g.t, p.Type()) {
						g.n++
						continue next
					}
				}
				groups = append(groups, &group{t: p.Type(), n: 1})
			}
			var dup []string
			for _, g := range groups {
				if g.n > 1 {
					dup = append(dup, fmt.Sprintf("%s(%d개)", types.TypeString(g.t, qual), g.n))
				}
			}
			sort.Strings(dup)
			for _, d := range dup {
				out = append(out, Violation{"A", ch.position(fn.Pos()),
					fmt.Sprintf("%s 가 같은 타입 %s 인자를 받는다. 순서를 바꿔도 컴파일된다", fn.Name.Name, d)})
			}
		}
	}
	return out
}

func isContext(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return false
	}
	return n.Obj().Pkg().Path() == "context" && n.Obj().Name() == "Context"
}

// paramUse 는 어떤 함수가 어떤 구조체를 인자나 수신자로 받았는지의 기록이다.
type paramUse struct {
	fn         string
	pkg        string
	pos        string
	structKey  string
	byValue    bool
	isRecv     bool
	readFields map[string]bool
	forwards   bool // 인자를 통째로 다른 곳에 썼는가 (전달, 복사, 반환)
}

type structInfo struct {
	pkg      string
	name     string
	fields   []string
	hasIface bool
}

// CheckParamStructs 는 규칙 B 를 담당한다. 셋을 함께 건다.
//
//	(a) 인터페이스 필드를 가진 구조체는 한 함수의 인자로만 쓴다. 수신자도 함께 센다
//	(b) 같은 패키지에 정의된 인자 구조체는 모든 필드가 본문에서 읽혀야 한다
//	(c) 인자 구조체를 통째로 다른 곳에 넘기지 않는다
//
// 하나라도 빠지면 우회된다. (c) 가 없으면 필요 없는 필드를 그대로 다음 함수에 흘려보내며
// (b) 를 만족시킬 수 있고, (a) 가 없으면 이름만 바꾼 공용 묶음이 다시 생긴다.
//
// 적용 대상을 가르는 기준이 핵심이다. (a) 와 (c) 는 "협력자를 담은 구조체" 에만 적용한다.
// 협력자란 인터페이스나 함수 타입 필드다. 중첩 구조체 안에 있어도, 포인터나 슬라이스 뒤에 있어도,
// 외부 패키지에서 온 인터페이스여도 같다. 타입 정보로 판정하므로 이름을 보지 않는다.
// 스칼라만 가진 값 객체는 여러 함수가 받아도 정상이다.
//
// (b) 는 값으로 받는 인자에 적용한다. 포인터로 받는 것은 부르는 쪽과 공유하는 가변 객체이므로
// 모든 필드를 읽으라고 요구할 수 없다. 다만 협력자 구조체는 포인터여도, 수신자여도 적용한다.
// 그렇게 하지 않으면 협력자 묶음이 포인터나 수신자가 되는 것만으로 (b) 를 빠져나간다.
//
// 읽기와 쓰기를 구분한다. `p.F = x` 는 F 를 읽은 것이 아니다.
// 식별자는 이름이 아니라 객체로 대조한다. 내부 블록에서 같은 이름을 다시 선언해도 섞이지 않는다.
func CheckParamStructs(c *Config, pkgs []*Checked) []Violation {
	var uses []paramUse
	structs := map[string]structInfo{}

	for _, ch := range pkgs {
		if !c.InScope(ch.Layer) {
			continue
		}
		for _, f := range ch.Files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				pos := ch.position(fn.Pos())
				record := func(v *types.Var, typ types.Type, isRecv bool) {
					named, st, byValue, ok := structOf(typ)
					if !ok || !c.InModule(named.Obj().Pkg().Path()) {
						return
					}
					key := named.Obj().Pkg().Path() + "." + named.Obj().Name()
					if _, seen := structs[key]; !seen {
						structs[key] = describeStruct(named, st)
					}
					read, forwards := map[string]bool{}, false
					if v != nil {
						read, forwards = analyzeParamUse(ch.Info, fn.Body, v, st)
					}
					uses = append(uses, paramUse{
						fn: fn.Name.Name, pkg: ch.Path, pos: pos, structKey: key,
						byValue: byValue, isRecv: isRecv, readFields: read, forwards: forwards,
					})
				}
				// 이름 없는 인자와 수신자(`func F(Deps)`, `func (Deps) M()`)와 `_` 도 인자를 받은 것이다.
				// 본문에서 아무것도 읽을 수 없으므로 "받았는데 하나도 읽지 않았다" 로 기록한다.
				// 이름을 지우면 규칙 B 를 빠져나가던 우회로다. 이름을 붙이지 않으면 위반이 없어지는 것이 아니라
				// 필드 전부가 미사용이 된다.
				collect := func(field *ast.Field, isRecv bool) {
					if len(field.Names) == 0 {
						if tv, ok := ch.Info.Types[field.Type]; ok {
							record(nil, tv.Type, isRecv)
						}
						return
					}
					for _, name := range field.Names {
						v, _ := ch.Info.Defs[name].(*types.Var)
						if v == nil {
							if tv, ok := ch.Info.Types[field.Type]; ok {
								record(nil, tv.Type, isRecv) // `_` 는 Defs 에 객체가 없다
							}
							continue
						}
						record(v, v.Type(), isRecv)
					}
				}
				if fn.Recv != nil {
					for _, r := range fn.Recv.List {
						collect(r, true)
					}
				}
				if fn.Type.Params != nil {
					for _, p := range fn.Type.Params.List {
						collect(p, false)
					}
				}
			}
		}
	}

	var out []Violation
	// (a) 공유 금지. 수신자도 함께 센다. 수신자를 빼면 협력자 묶음이 수신자로 도망간다.
	holders := map[string][]paramUse{}
	for _, u := range uses {
		holders[u.structKey] = append(holders[u.structKey], u)
	}
	for _, key := range sortedKeys(holders) {
		us := holders[key]
		info := structs[key]
		if !info.hasIface {
			continue
		}
		names := map[string]bool{}
		for _, u := range us {
			names[u.pos+" "+u.fn] = true
		}
		if len(names) < 2 {
			continue
		}
		var fns []string
		for _, u := range us {
			fns = append(fns, u.fn)
		}
		sort.Strings(fns)
		out = append(out, Violation{"B-a", us[0].pos,
			fmt.Sprintf("%s 는 협력자를 담은 구조체인데 함수 %d 개가 받는다(%s). 공용 협력자 묶음이다. 함수마다 자기 인자 구조체를 갖는다",
				key, len(names), strings.Join(fns, ", "))})
	}

	for _, u := range uses {
		info := structs[u.structKey]
		// (b) 미사용 필드 금지.
		applyB := (u.byValue || info.hasIface) && (!u.isRecv || info.hasIface) && info.pkg == u.pkg
		if applyB {
			var missing []string
			for _, f := range info.fields {
				if !u.readFields[f] {
					missing = append(missing, f)
				}
			}
			if len(missing) > 0 {
				out = append(out, Violation{"B-b", u.pos,
					fmt.Sprintf("%s 가 받는 %s 의 필드 %s 를 본문에서 읽지 않는다. 인자 구조체는 그 작업의 명세다",
						u.fn, u.structKey, strings.Join(missing, ", "))})
			}
		}
		// (c) 통째 전달 금지.
		if info.hasIface && u.forwards {
			out = append(out, Violation{"B-c", u.pos,
				fmt.Sprintf("%s 가 받는 %s 를 통째로 쓴다(전달, 복사, 반환). 넘겨받는 쪽도 필요한 것만 받는다",
					u.fn, u.structKey)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Pos < out[j].Pos
	})
	return out
}

// structOf 는 타입이 (포인터일 수 있는) 이름 있는 구조체면 그 정보를 돌려준다.
func structOf(t types.Type) (*types.Named, *types.Struct, bool, bool) {
	byValue := true
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		byValue = false
		t = types.Unalias(p.Elem())
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return nil, nil, false, false
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil, nil, false, false
	}
	return named, st, byValue, true
}

func describeStruct(named *types.Named, st *types.Struct) structInfo {
	info := structInfo{pkg: named.Obj().Pkg().Path(), name: named.Obj().Name()}
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Name() == "_" {
			continue
		}
		info.fields = append(info.fields, f.Name())
	}
	info.hasIface = hasCollaborator(st, map[*types.Named]bool{})
	return info
}

// hasCollaborator 는 타입 안 어딘가에 인터페이스나 함수 타입이 있는지 본다.
//
// 중첩 구조체, 포인터, 슬라이스, 맵, 채널 뒤에 숨어도 찾는다. 한 겹 감싸는 것으로 규칙을
// 빠져나가게 두지 않기 위해서다. 빈 인터페이스(any)와 error 는 협력자로 보지 않는다.
// 값 객체가 흔히 갖는 필드이고, 그것을 협력자로 세면 오탐이 늘어 게이트가 약화된다.
func hasCollaborator(t types.Type, seen map[*types.Named]bool) bool {
	t = types.Unalias(t)
	switch x := t.(type) {
	case *types.Named:
		if seen[x] {
			return false
		}
		seen[x] = true
		if x == types.Universe.Lookup("error").Type() {
			return false
		}
		return hasCollaborator(x.Underlying(), seen)
	case *types.Interface:
		return !x.Empty()
	case *types.Signature:
		return true
	case *types.Struct:
		for i := 0; i < x.NumFields(); i++ {
			if hasCollaborator(x.Field(i).Type(), seen) {
				return true
			}
		}
	case *types.Pointer:
		return hasCollaborator(x.Elem(), seen)
	case *types.Slice:
		return hasCollaborator(x.Elem(), seen)
	case *types.Array:
		return hasCollaborator(x.Elem(), seen)
	case *types.Map:
		return hasCollaborator(x.Elem(), seen)
	case *types.Chan:
		return hasCollaborator(x.Elem(), seen)
	}
	return false
}

// analyzeParamUse 는 인자 객체 v 가 본문에서 어떻게 쓰이는지 본다.
//
// 돌려주는 것: 읽힌 최상위 필드 이름의 집합, 그리고 통째로 쓰였는지.
// "통째로 쓰였다" 는 필드 선택이 아닌 모든 사용이다. 호출 인자, 복사(q := p), 반환,
// 복합 리터럴, 주소 취득. 예외는 nil 비교와 인자 자신에 대한 재대입뿐이다.
// 복사를 통째 사용으로 보는 이유: `q := p; sink(q)` 로 (c) 를 우회하는 것을 막는다.
func analyzeParamUse(info *types.Info, body *ast.BlockStmt, v *types.Var, st *types.Struct) (map[string]bool, bool) {
	read := map[string]bool{}
	forwards := false
	if body == nil {
		return read, false
	}
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		defer func() { stack = append(stack, n) }()
		id, ok := n.(*ast.Ident)
		if !ok || info.Uses[id] != v {
			return true
		}
		parent := ast.Node(nil)
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		grand := ast.Node(nil)
		if len(stack) > 1 {
			grand = stack[len(stack)-2]
		}
		switch p := parent.(type) {
		case *ast.SelectorExpr:
			if p.X != id {
				forwards = true
				return true
			}
			sel := info.Selections[p]
			if sel == nil {
				return true // 패키지 한정자처럼 보이는 경우는 없다. 타입 오류로 정보가 빠진 것이다
			}
			idx := sel.Index()
			if sel.Kind() == types.FieldVal || len(idx) > 1 {
				if isAssignTarget(grand, p) {
					return true // 쓰기는 읽기가 아니다
				}
				read[st.Field(idx[0]).Name()] = true
			}
			return true
		case *ast.BinaryExpr:
			if (p.Op == token.EQL || p.Op == token.NEQ) && (isNil(p.X, info) || isNil(p.Y, info)) {
				return true
			}
		case *ast.AssignStmt:
			for _, l := range p.Lhs {
				if l == id {
					return true // 인자 자신의 재대입
				}
			}
		}
		forwards = true
		return true
	})
	return read, forwards
}

// isAssignTarget 는 sel 이 단순 대입문의 좌변인지 본다. `p.F = x` 만 쓰기다.
// `p.F += x` 나 `p.F++` 는 읽기를 포함하므로 읽기로 친다.
func isAssignTarget(stmt ast.Node, sel *ast.SelectorExpr) bool {
	as, ok := stmt.(*ast.AssignStmt)
	if !ok || (as.Tok != token.ASSIGN && as.Tok != token.DEFINE) {
		return false
	}
	for _, l := range as.Lhs {
		if l == sel {
			return true
		}
	}
	return false
}

func isNil(e ast.Expr, info *types.Info) bool {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	_, isNil := info.Uses[id].(*types.Nil)
	return isNil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

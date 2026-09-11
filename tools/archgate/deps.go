package main

import (
	"fmt"
	"sort"
)

// CheckDepClosure 는 모듈 안 패키지 그래프의 전이 의존을 계층 규칙에 비추어 본다.
//
// graph 는 패키지 → 직접 import 목록이다. 모듈 밖 의존은 계층이 없으므로 무시된다.
// 직접 import 위반은 파일 검사가 줄 번호와 함께 보고하므로 여기서는 전이 의존만 보고한다.
// 전이 의존을 보는 이유: A→B 가 허용되고 B→C 가 허용돼도 A→C 가 금지일 수 있다.
// 그 경로가 헬퍼 뒤에 숨는 것을 시그니처가 아니라 그래프에서 잡는다.
func CheckDepClosure(c *Config, graph map[string][]string) []Violation {
	var out []Violation
	pkgs := make([]string, 0, len(graph))
	for p := range graph {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, from := range pkgs {
		if c.LayerOf(from) == unknownLayer {
			continue
		}
		direct := map[string]bool{}
		for _, d := range graph[from] {
			direct[d] = true
		}
		seen := map[string]bool{from: true}
		stack := append([]string{}, graph[from]...)
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[cur] || !c.InModule(cur) {
				continue
			}
			seen[cur] = true
			stack = append(stack, graph[cur]...)
		}
		var closure []string
		for p := range seen {
			if p != from && !direct[p] {
				closure = append(closure, p)
			}
		}
		sort.Strings(closure)
		for _, dep := range closure {
			if why := c.importViolation(from, dep); why != "" {
				out = append(out, Violation{"layer", from, fmt.Sprintf("전이 의존: %s", why)})
			}
		}
	}
	return out
}

// Command todoable은 로컬 작업 실행 도구의 합성 루트다.
// 현재는 프로젝트 골격이며 제품 CLI 계약은 구현되지 않았다.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, `{"protocol_version":1,"error":"not_implemented","message":"todoable 제품 기능은 아직 구현되지 않았습니다"}`)
	os.Exit(1)
}

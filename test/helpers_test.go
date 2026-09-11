package test

import "testing"

// todo는 미구현을 통과로 보고하지 않도록 testreport의 표식을 남긴다.
func todo(t *testing.T, sc string) {
	t.Helper()
	t.Skipf("SCENARIO_TODO %s 미구현", sc)
}

// verify는 문서의 검증 줄을 실행 결과에서 관찰할 수 있는 하위 테스트로 만든다.
func verify(t *testing.T, id string, check func(t *testing.T)) {
	t.Helper()
	t.Run(id, check)
}

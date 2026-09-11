package test

import (
	"fmt"
	"strings"
	"testing"
)

// TestScenario_SC_01: 동일 입력을 동시에 여러 번 접수
func TestScenario_SC_01(t *testing.T) {
	f := newIntake(t)
	var first map[string]any
	verify(t, "V-01", func(t *testing.T) {
		results := concurrentIntake(t, f)
		fresh := 0
		for _, result := range results {
			if result == nil {
				t.Fatal("missing CLI result")
			}
			if first == nil {
				first = result
			}
			if result["submission_id"] != first["submission_id"] || result["run_id"] != first["run_id"] || result["state"] != "waiting" || result["task_version"] != float64(1) {
				t.Fatalf("responses differ: %v / %v", first, result)
			}
			if result["deduplicated"] == false {
				fresh++
			}
		}
		if fresh != 1 {
			t.Fatalf("new submissions: %d", fresh)
		}
		f.stored(t, first)
	})
	verify(t, "V-02", func(t *testing.T) {
		if first == nil {
			t.Fatal("initial submission failed")
		}
		result := f.call(t, "run", "submit", f.input)
		if result["submission_id"] != first["submission_id"] || result["run_id"] != first["run_id"] || result["deduplicated"] != true {
			t.Fatalf("retransmission: %v / %v", first, result)
		}
		f.stored(t, first)
	})
}

// TestScenario_SC_02: 접수 커밋 뒤 응답 전에 강제 종료
func TestScenario_SC_02(t *testing.T) {
	f := newIntake(t)
	var original map[string]any
	verify(t, "V-01", func(t *testing.T) {
		original = crashAfterCommit(t, f)
		sameSubmission(t, original, f.call(t, "run", "submit", f.input))
		f.stored(t, original)
	})
	verify(t, "V-02", func(t *testing.T) {
		sameSubmission(t, original, f.call(t, "run", "submit", f.input))
		f.stored(t, original)
	})
}

// TestScenario_SC_03: 큐 포화·Task 비활성 중 중복 재전송
func TestScenario_SC_03(t *testing.T) {
	f := newIntake(t)
	first := f.call(t, "run", "submit", f.input)
	original := inputJSON(`{"b":[true,"1"],"a":1}`)
	verify(t, "V-01", func(t *testing.T) {
		for i := 0; i < 99; i++ {
			f.submitRaw(t, inputJSON(fmt.Sprintf(`{"generation":%d}`, i)))
		}
		sameSubmission(t, first, f.submitRaw(t, original))
		f.call(t, "task", "disable", "concurrent")
		sameSubmission(t, first, f.submitRaw(t, original))
		f.counts(t, 100)
	})
	verify(t, "V-02", func(t *testing.T) {
		writeTest(t, f.input, []byte(inputJSON(`{"new":true}`)), 0600)
		f.rejected(t, 6, "task_disabled", "run", "submit", f.input)
		f.call(t, "task", "enable", "concurrent")
		f.rejected(t, 5, "queue_full", "run", "submit", f.input)
		f.counts(t, 100)
		var total, remaining, calls int
		var input string
		e := f.database(t).QueryRow("SELECT b.total,b.remaining,r.calls_used,s.input FROM submissions s JOIN repeat_budgets b ON b.submission_id=s.id JOIN runs r ON r.submission_id=s.id WHERE s.id=?", first["submission_id"]).Scan(&total, &remaining, &calls, &input)
		if e != nil || total != 2 || remaining != 2 || calls != 0 || input != `{"a":1,"b":[true,"1"]}` {
			t.Fatalf("original altered: %d %d %d %s %v", total, remaining, calls, input, e)
		}
	})
}

// TestScenario_SC_04: 같은 키의 A 실행 중 B·C 접수
func TestScenario_SC_04(t *testing.T) {
	todo(t, "SC-04")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-04")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-04")
	})
}

// TestScenario_SC_05: A → B 뒤 과거 A 재전송
func TestScenario_SC_05(t *testing.T) {
	todo(t, "SC-05")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-05")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-05")
	})
}

// TestScenario_SC_06: Task 갱신 중 접수 경합
func TestScenario_SC_06(t *testing.T) {
	todo(t, "SC-06")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-06")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-06")
	})
}

// TestScenario_SC_07: Task 갱신 뒤 버전 생략 재전송
func TestScenario_SC_07(t *testing.T) {
	f := newIntake(t)
	first := f.call(t, "run", "submit", f.input)
	verify(t, "V-01", func(t *testing.T) {
		updateTask(t, f)
		sameSubmission(t, first, f.call(t, "run", "submit", f.input))
		f.stored(t, first)
	})
	verify(t, "V-02", func(t *testing.T) {
		raw := inputJSON(`{"a":1,"b":[true,"1"]}`)
		writeTest(t, f.input, []byte(strings.Replace(raw, `"task_id"`, `"task_version":2,"task_id"`, 1)), 0600)
		f.rejected(t, 3, "metadata_conflict", "run", "submit", f.input)
		sameSubmission(t, first, f.submitRaw(t, strings.Replace(raw, `"task_id"`, `"task_version":1,"task_id"`, 1)))
		f.stored(t, first)
		db := f.database(t)
		if _, e := db.Exec("UPDATE repeat_budgets SET remaining=1"); e != nil {
			t.Fatal(e)
		}
		if _, e := db.Exec("UPDATE runs SET calls_used=1"); e != nil {
			t.Fatal(e)
		}
		sameSubmission(t, first, f.call(t, "run", "submit", f.input))
		var remaining, calls int
		if e := db.QueryRow("SELECT b.remaining,r.calls_used FROM repeat_budgets b JOIN runs r ON r.submission_id=b.submission_id").Scan(&remaining, &calls); e != nil || remaining != 1 || calls != 1 {
			t.Fatalf("spent budget reset: %d %d %v", remaining, calls, e)
		}

	})
}

// TestScenario_SC_08: 서로 다른 Task의 같은 충돌 키
func TestScenario_SC_08(t *testing.T) {
	todo(t, "SC-08")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-08")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-08")
	})
}

// TestScenario_SC_09: 무관한 충돌 키 여러 개
func TestScenario_SC_09(t *testing.T) {
	todo(t, "SC-09")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-09")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-09")
	})
}

// TestScenario_SC_10: Run 반복과 다른 입력의 자원 경쟁
func TestScenario_SC_10(t *testing.T) {
	todo(t, "SC-10")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-10")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-10")
	})
}

// TestScenario_SC_11: 최초 종료검사 참
func TestScenario_SC_11(t *testing.T) {
	todo(t, "SC-11")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-11")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-11")
	})
}

// TestScenario_SC_12: 마지막 허용 agent 호출 뒤 검사 참
func TestScenario_SC_12(t *testing.T) {
	todo(t, "SC-12")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-12")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-12")
	})
}

// TestScenario_SC_13: agent 정상 비영 코드, 종료검사 참
func TestScenario_SC_13(t *testing.T) {
	todo(t, "SC-13")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-13")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-13")
	})
}

// TestScenario_SC_14: after 정상 실패
func TestScenario_SC_14(t *testing.T) {
	todo(t, "SC-14")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-14")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-14")
	})
}

// TestScenario_SC_15: 조건이 계속 거짓
func TestScenario_SC_15(t *testing.T) {
	todo(t, "SC-15")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-15")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-15")
	})
}

// TestScenario_SC_16: 긴 정기 작업 중 여러 시각 도래
func TestScenario_SC_16(t *testing.T) {
	todo(t, "SC-16")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-16")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-16")
	})
}

// TestScenario_SC_17: 일정 커밋·재시작 경계에서 중단
func TestScenario_SC_17(t *testing.T) {
	todo(t, "SC-17")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-17")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-17")
	})
}

// TestScenario_SC_18: 빠진 보고 기간 수동 접수
func TestScenario_SC_18(t *testing.T) {
	todo(t, "SC-18")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-18")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-18")
	})
}

// TestScenario_SC_19: DST 시각 전환과 시계 후퇴
func TestScenario_SC_19(t *testing.T) {
	todo(t, "SC-19")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-19")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-19")
	})
}

// TestScenario_SC_20: 슬롯 예약·명령 결과 기록 사이 강제 종료
func TestScenario_SC_20(t *testing.T) {
	todo(t, "SC-20")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-20")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-20")
	})
}

// TestScenario_SC_21: after 외부 효과 직후 결과 저장 전 종료
func TestScenario_SC_21(t *testing.T) {
	todo(t, "SC-21")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-21")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-21")
	})
}

// TestScenario_SC_22: 옛 프로세스가 남거나 PID 확인 실패
func TestScenario_SC_22(t *testing.T) {
	todo(t, "SC-22")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-22")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-22")
	})
}

// TestScenario_SC_23: 차단 해소·재실행
func TestScenario_SC_23(t *testing.T) {
	todo(t, "SC-23")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-23")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-23")
	})
}

// TestScenario_SC_24: 취소와 완료 경합
func TestScenario_SC_24(t *testing.T) {
	todo(t, "SC-24")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-24")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-24")
	})
}

// TestScenario_SC_25: DB·디스크 쓰기 실패
func TestScenario_SC_25(t *testing.T) {
	todo(t, "SC-25")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-25")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-25")
	})
}

// TestScenario_SC_26: 출력 폭주·멈춘 검사
func TestScenario_SC_26(t *testing.T) {
	todo(t, "SC-26")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-26")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-26")
	})
}

// TestScenario_SC_27: 완료 로그 정리
func TestScenario_SC_27(t *testing.T) {
	todo(t, "SC-27")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-27")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-27")
	})
}

// TestScenario_SC_28: 이슈 수정 실사용
func TestScenario_SC_28(t *testing.T) {
	todo(t, "SC-28")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-28")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-28")
	})
}

// TestScenario_SC_29: 독립 자료 여러 개 변환 실사용
func TestScenario_SC_29(t *testing.T) {
	todo(t, "SC-29")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-29")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-29")
	})
}

// TestScenario_SC_30: 기간별 리포트와 외부 게시 실사용
func TestScenario_SC_30(t *testing.T) {
	todo(t, "SC-30")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-30")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-30")
	})
}

// TestScenario_SC_31: Task 등록·갱신·활성 상태
func TestScenario_SC_31(t *testing.T) {
	todo(t, "SC-31")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-31")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-31")
	})
}

// TestScenario_SC_32: JSON 정규화와 중복 메타데이터
func TestScenario_SC_32(t *testing.T) {
	f := newIntake(t)
	first := f.call(t, "run", "submit", f.input)
	verify(t, "V-01", func(t *testing.T) {
		sameSubmission(t, first, f.submitRaw(t, inputJSON(`{ "a":1.0, "b": [true,"1"] }`)))
		for _, input := range []string{`{"a":1,"b":["1",true]}`, `{"a":"1","b":[true,"1"]}`, `{"a":1,"b":[true,"1"],"generation":2}`} {
			got := f.submitRaw(t, inputJSON(input))
			if got["deduplicated"] != false || got["submission_id"] == first["submission_id"] {
				t.Fatalf("new input merged: %v", got)
			}
		}
		raw := inputJSON(`{"a":1,"b":[true,"1"]}`)
		for _, bad := range []string{strings.Replace(raw, "resource", "other", 1), strings.Replace(raw, `"task_id"`, `"task_version":2,"task_id"`, 1)} {
			writeTest(t, f.input, []byte(bad), 0600)
			f.rejected(t, 3, "metadata_conflict", "run", "submit", f.input)
		}
		db := f.database(t)
		if _, e := db.Exec("UPDATE submissions SET hash=? WHERE id=?", hashForCollision(), first["submission_id"]); e != nil {
			t.Fatal(e)
		}
		writeTest(t, f.input, []byte(inputJSON(`{"collision":true}`)), 0600)
		f.rejected(t, 3, "hash_collision", "run", "submit", f.input)
		f.counts(t, 4)
	})
	verify(t, "V-02", func(t *testing.T) {
		before := f.durableAdmission(t)
		for _, bad := range []string{inputJSON("{\"x\":\"" + string([]byte{255}) + "\"}"), inputJSON(`{"x":"\ud800"}`), inputJSON(`{"x":1,"x":2}`), inputJSON(`{"x":1e400}`), inputJSON(`{"x":9007199254740992}`), strings.Replace(inputJSON(`{}`), "item", " item", 1), strings.Replace(inputJSON(`{}`), "resource", "", 1)} {
			writeTest(t, f.input, []byte(bad), 0600)
			f.rejected(t, 2, "validation_error", "run", "submit", f.input)
		}
		f.counts(t, 4)
		if after := f.durableAdmission(t); after != before {
			t.Fatal("rejected request mutated durable admission")
		}
	})
}

// TestScenario_SC_33: Task·입력·설정 파싱과 상한
func TestScenario_SC_33(t *testing.T) {
	todo(t, "SC-33")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-33")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-33")
	})
}

// TestScenario_SC_34: 환경·컨텍스트·명령 실행 계약
func TestScenario_SC_34(t *testing.T) {
	todo(t, "SC-34")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-34")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-34")
	})
}

// TestScenario_SC_35: 시작 재확인과 단계별 실패
func TestScenario_SC_35(t *testing.T) {
	todo(t, "SC-35")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-35")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-35")
	})
}

// TestScenario_SC_36: 일정 표현·활성화·버전 이력
func TestScenario_SC_36(t *testing.T) {
	todo(t, "SC-36")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-36")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-36")
	})
}

// TestScenario_SC_37: 일정 포화·오류와 수동 기간 경계
func TestScenario_SC_37(t *testing.T) {
	todo(t, "SC-37")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-37")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-37")
	})
}

// TestScenario_SC_38: 시간 예산과 읽기 전용 검사 복구
func TestScenario_SC_38(t *testing.T) {
	todo(t, "SC-38")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-38")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-38")
	})
}

// TestScenario_SC_39: 결과 불명 접수의 취소 해소
func TestScenario_SC_39(t *testing.T) {
	todo(t, "SC-39")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-39")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-39")
	})
}

// TestScenario_SC_40: 설정 변경과 기존 접수 보호
func TestScenario_SC_40(t *testing.T) {
	todo(t, "SC-40")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-40")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-40")
	})
}

// TestScenario_SC_41: CLI 응답·관측·데몬 경계
func TestScenario_SC_41(t *testing.T) {
	todo(t, "SC-41")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-41")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-41")
	})
}

// TestScenario_SC_42: 종료 신호와 일관된 저장 복구
func TestScenario_SC_42(t *testing.T) {
	todo(t, "SC-42")
	verify(t, "V-01", func(t *testing.T) {
		todo(t, "SC-42")
	})
	verify(t, "V-02", func(t *testing.T) {
		todo(t, "SC-42")
	})
}

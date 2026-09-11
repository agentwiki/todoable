package local

import (
	"encoding/json"
	"github.com/agentwiki/todoable/internal/domain"
	"testing"
)

func TestSubmissionIdentityAndRollback(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	raw, _ := json.Marshal(map[string]any{"version": 1, "id": "task", "workdir": t.TempDir(), "agent": []string{"echo"}, "prompt": "work", "finish": map[string]any{"check": []string{"true"}}, "repeat": 2})
	task, e := ParseTask(raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Register(task); e != nil {
		t.Fatal(e)
	}
	parse := func(raw string) domain.SubmissionInput {
		t.Helper()
		in, e := domain.ParseSubmission([]byte(raw))
		if e != nil {
			t.Fatal(e)
		}
		return in
	}
	a := parse(`{"task_id":"task","input_key":"key","concurrency_key":"resource","input":{"a":1,"b":2}}`)
	first, e := s.Submit(a)
	if e != nil {
		t.Fatal(e)
	}
	normalized := parse(`{"task_id":"task","input_key":"key","concurrency_key":"resource","input":{"b":2.0,"a":1.0}}`)
	same, e := s.Submit(normalized)
	if e != nil || !same.Deduplicated || same.SubmissionID != first.SubmissionID {
		t.Fatalf("canonical duplicate: %+v %v", same, e)
	}
	a.ConcurrencyKey = "other"
	if _, e = s.Submit(a); e == nil {
		t.Fatal("metadata conflict accepted")
	}
	a.ConcurrencyKey = "resource"
	a.Input = json.RawMessage(`{"a":2,"b":2}`)
	second, e := s.Submit(a)
	if e != nil || second.SubmissionID == first.SubmissionID {
		t.Fatalf("different input: %+v %v", second, e)
	}
	// A failure after the submission insert must roll back all three records.
	if _, e = s.db.Exec(`CREATE TRIGGER fail_run BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT,'injected run failure'); END`); e != nil {
		t.Fatal(e)
	}
	a.Input = json.RawMessage(`{"a":3}`)
	if _, e = s.Submit(a); e == nil {
		t.Fatal("expected injected failure")
	}
	for _, table := range []string{"submissions", "runs", "repeat_budgets"} {
		var n int
		if e = s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil || n != 2 {
			t.Fatalf("atomic %s: %d %v", table, n, e)
		}
	}
}
func FuzzTask(f *testing.F) {
	f.Add([]byte("version: 1\nid: task\n"))
	f.Add([]byte("a: &a [*a]"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 10000 {
			t.Skip()
		}
		_, _ = ParseTask(b)
	})
}

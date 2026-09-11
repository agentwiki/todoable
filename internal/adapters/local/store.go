package local

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentwiki/todoable/internal/domain"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct{ db *sql.DB }

func Open(dir string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	if _, e := os.Stat(filepath.Join(dir, "config.yaml")); e == nil {
		return nil, &domain.Fault{Code: 6, Kind: "not_implemented", Message: "config.yaml support is not implemented"}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	path, e := filepath.Abs(filepath.Join(dir, "todoable.db"))
	if e != nil {
		return nil, e
	}
	u := url.URL{Scheme: "file", Path: path}
	db, e := sql.Open("sqlite3", u.String()+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate")
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY, version INTEGER NOT NULL, definition TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS submissions(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,task_id TEXT NOT NULL REFERENCES tasks(id),task_version INTEGER NOT NULL,input_key TEXT NOT NULL,input TEXT NOT NULL,hash TEXT NOT NULL UNIQUE,concurrency_key TEXT NOT NULL,snapshot TEXT NOT NULL,state TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS repeat_budgets(submission_id TEXT PRIMARY KEY REFERENCES submissions(id),total INTEGER NOT NULL,remaining INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS runs(id TEXT PRIMARY KEY,submission_id TEXT NOT NULL REFERENCES submissions(id),run_seq INTEGER NOT NULL,state TEXT NOT NULL,calls_used INTEGER NOT NULL DEFAULT 0,UNIQUE(submission_id,run_seq));`)
	if e != nil {
		_ = db.Close()
		return nil, e
	}
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Register(task domain.Task) (int, error) {
	raw, e := json.Marshal(task)
	if e != nil {
		return 0, e
	}
	definition, e := domain.Canonical(raw)
	if e != nil {
		return 0, e
	}
	tx, e := s.db.Begin()
	if e != nil {
		return 0, e
	}
	defer func() { _ = tx.Rollback() }()
	var existing string
	var version int
	e = tx.QueryRow("SELECT version,definition FROM tasks WHERE id=?", task.ID).Scan(&version, &existing)
	if e == nil {
		if existing != string(definition) {
			return 0, &domain.Fault{Code: 3, Kind: "definition_conflict", Message: "Task already has a different definition"}
		}
		return version, tx.Commit()
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return 0, e
	}
	if _, e = tx.Exec("INSERT INTO tasks VALUES(?,1,?)", task.ID, string(definition)); e != nil {
		return 0, e
	}
	return 1, tx.Commit()
}
func (s *Store) Submit(in domain.SubmissionInput) (domain.Result, error) {
	out := domain.Result{ProtocolVersion: 1}
	tx, e := s.db.Begin()
	if e != nil {
		return out, e
	}
	defer func() { _ = tx.Rollback() }()
	var definition string
	var version int
	e = tx.QueryRow("SELECT version,definition FROM tasks WHERE id=?", in.TaskID).Scan(&version, &definition)
	if errors.Is(e, sql.ErrNoRows) {
		return out, &domain.Fault{Code: 4, Kind: "not_found", Message: "Task not found"}
	}
	if e != nil {
		return out, e
	}
	var oldTask, oldKey, oldInput, oldConcurrency string
	e = tx.QueryRow(`SELECT s.id,s.task_version,r.state,r.id,s.task_id,s.input_key,s.input,s.concurrency_key FROM submissions s JOIN runs r ON r.submission_id=s.id WHERE s.hash=? ORDER BY r.run_seq DESC LIMIT 1`, in.Hash()).Scan(&out.SubmissionID, &out.TaskVersion, &out.State, &out.RunID, &oldTask, &oldKey, &oldInput, &oldConcurrency)
	if e == nil {
		if oldTask != in.TaskID || oldKey != in.InputKey || oldInput != string(in.Input) {
			return out, &domain.Fault{Code: 3, Kind: "hash_collision", Message: "canonical input hash collision"}
		}
		if oldConcurrency != in.ConcurrencyKey || (in.TaskVersion != nil && *in.TaskVersion != out.TaskVersion) {
			return out, &domain.Fault{Code: 3, Kind: "metadata_conflict", Message: "duplicate metadata differs"}
		}
		out.Deduplicated = true
		return out, tx.Commit()
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if in.TaskVersion != nil && *in.TaskVersion != version {
		return out, &domain.Fault{Code: 4, Kind: "not_found", Message: "Task version not found"}
	}
	var pending, keyPending int
	if e = tx.QueryRow("SELECT count(*),coalesce(sum(CASE WHEN task_id=? AND input_key=? THEN 1 ELSE 0 END),0) FROM submissions WHERE state='active'", in.TaskID, in.InputKey).Scan(&pending, &keyPending); e != nil {
		return out, e
	}
	if pending >= 1000 || keyPending >= 100 {
		return out, &domain.Fault{Code: 5, Kind: "queue_full", Message: "pending submission limit reached"}
	}
	var task domain.Task
	if e = json.Unmarshal([]byte(definition), &task); e != nil {
		return out, e
	}
	snap, e := snapshot(task)
	if e != nil {
		return out, e
	}
	out.SubmissionID, e = uuid()
	if e != nil {
		return out, e
	}
	out.RunID, e = uuid()
	if e != nil {
		return out, e
	}
	out.TaskVersion = version
	out.State = "waiting"
	if _, e = tx.Exec("INSERT INTO submissions(id,task_id,task_version,input_key,input,hash,concurrency_key,snapshot,state) VALUES(?,?,?,?,?,?,?,?,?)", out.SubmissionID, in.TaskID, version, in.InputKey, string(in.Input), in.Hash(), in.ConcurrencyKey, string(snap), "active"); e != nil {
		return out, e
	}
	if _, e = tx.Exec("INSERT INTO repeat_budgets VALUES(?,?,?)", out.SubmissionID, task.Repeat, task.Repeat); e != nil {
		return out, e
	}
	if _, e = tx.Exec("INSERT INTO runs(id,submission_id,run_seq,state) VALUES(?,?,1,?)", out.RunID, out.SubmissionID, out.State); e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func uuid() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func ErrorResult(err error) (int, map[string]any) {
	f := &domain.Fault{Code: 1, Kind: "storage_error", Message: err.Error()}
	var known *domain.Fault
	if errors.As(err, &known) {
		f = known
	}
	if strings.Contains(err.Error(), "database is locked") || strings.Contains(err.Error(), "database table is locked") {
		f = &domain.Fault{Code: 5, Kind: "database_busy", Message: "database is locked"}
	}
	return f.Code, map[string]any{"protocol_version": 1, "error": f.Kind, "message": f.Message}
}
func (s *Store) Run(id string) (map[string]any, error) {
	var task, inputKey, input, submission, state, concurrency string
	var version, seq, calls, total, remaining int
	e := s.db.QueryRow(`SELECT s.task_id,s.task_version,s.input_key,s.input,s.id,r.state,r.run_seq,r.calls_used,s.concurrency_key,b.total,b.remaining FROM runs r JOIN submissions s ON s.id=r.submission_id JOIN repeat_budgets b ON b.submission_id=s.id WHERE r.id=?`, id).Scan(&task, &version, &inputKey, &input, &submission, &state, &seq, &calls, &concurrency, &total, &remaining)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, &domain.Fault{Code: 4, Kind: "not_found", Message: "Run not found"}
	}
	if e != nil {
		return nil, e
	}
	return map[string]any{"protocol_version": 1, "task_id": task, "task_version": version, "input_key": inputKey, "input": json.RawMessage(input), "submission_id": submission, "run_id": id, "run_seq": seq, "state": state, "stage": "waiting", "calls_used": calls, "repeat": total, "repeat_remaining": remaining, "concurrency_key": concurrency, "predecessor": nil, "next_check_at": nil, "scheduled_at": nil, "last_check": nil, "time_remaining": nil, "cancel_requested": false, "blocked_reason": nil, "steps": []any{}}, nil
}

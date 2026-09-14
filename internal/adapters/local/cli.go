package local

import (
	"encoding/json"
	"fmt"
	"github.com/agentwiki/todoable/internal/domain"
	"io"
	"os"
	"path/filepath"
)

func DefaultDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "todoable")
	}
	return filepath.Join(os.Getenv("HOME"), ".local/share/todoable")
}
func ReadFile(path string) ([]byte, error) {
	return ReadFileLimit(path, 1048576+16384)
}
func ReadFileLimit(path string, limit int) ([]byte, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, e := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if e != nil {
		return nil, e
	}
	if len(b) > limit {
		return nil, domain.Invalid("file too large")
	}
	return b, nil
}
func Write(value any) error { return json.NewEncoder(os.Stdout).Encode(value) }
func Fail(err error) int {
	code, value := ErrorResult(err)
	_ = json.NewEncoder(os.Stderr).Encode(value)
	return code
}

func WriteSchedule(value map[string]any) error {
	_, e := fmt.Fprintf(os.Stdout, "Task: %v (version %v)\nEnabled: %v\nAnchor: %v\nObserved: %v\nLatest unaccepted: %v\nLast accepted: %v\nSkipped: %v\nDiscarded: %v (%v)\nLast error: %v\n", value["task_id"], value["task_version"], value["enabled"], value["anchor"], value["observed_at"], value["latest_unaccepted_at"], value["last_accepted_at"], value["skipped"], value["discarded"], value["discard_reason"], value["last_error"])
	return e
}

func WriteObservation(kind string, value map[string]any) error {
	if kind == "status" {
		daemon := value["daemon"].(map[string]any)
		if _, err := fmt.Fprintf(os.Stdout, "Last recorded daemon state: %v\nObserved: %v\nStale: %v (recorded state does not prove current liveness)\n", daemon["state"], daemon["observed_at"], daemon["stale"]); err != nil {
			return err
		}
		for _, raw := range value["runs"].([]any) {
			if err := WriteObservation("run", raw.(map[string]any)); err != nil {
				return err
			}
		}
		return nil
	}
	if kind == "task" {
		definition, err := json.MarshalIndent(value["definition"], "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(os.Stdout, "Task: %v (version %v, current %v)\nEnabled: %v\nDefinition:\n%s\n", value["task_id"], value["task_version"], value["current_version"], value["enabled"], definition)
		return err
	}
	for _, field := range []string{"task_id", "task_version", "input_key", "input", "submission_id", "run_id", "run_seq", "state", "stage", "calls_used", "repeat", "repeat_remaining", "concurrency_key", "predecessor", "next_check_at", "scheduled_at", "last_check", "time_remaining", "cancel_requested", "blocked_reason", "steps", "summary", "resolutions"} {
		raw, err := json.Marshal(value[field])
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(os.Stdout, "%s: %s\n", field, raw); err != nil {
			return err
		}
	}
	return nil
}

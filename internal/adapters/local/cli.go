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
	var r io.Reader = os.Stdin
	if path != "-" {
		f, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, e := io.ReadAll(io.LimitReader(r, 1048576+16385))
	if e != nil {
		return nil, e
	}
	if len(b) > 1048576+16384 {
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

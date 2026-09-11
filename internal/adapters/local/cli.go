package local

import (
	"encoding/json"
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

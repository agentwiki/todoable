package local

import (
	"context"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agentwiki/todoable/internal/domain"
)

// WriteLogs follows stored byte streams, with independent offsets for every Step.
// It never infers a Run result from the presence or absence of a log file.
func (s *Store) WriteLogs(runID, stepID string, follow bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	offsets := map[string]int64{}
	for {
		view, err := s.Run(runID)
		if err != nil {
			return err
		}
		found := stepID == ""
		for _, raw := range view["steps"].([]any) {
			step := raw.(map[string]any)
			id := step["step_id"].(string)
			if stepID != "" && id != stepID {
				continue
			}
			found = true
			for _, name := range []string{"stdout", "stderr"} {
				path := filepath.Join(s.dir, "runs", runID, "steps", id, name)
				file, e := os.Open(path)
				if os.IsNotExist(e) {
					continue
				}
				if e != nil {
					return e
				}
				_, e = file.Seek(offsets[path], io.SeekStart)
				if e != nil {
					_ = file.Close()
					return e
				}
				n, e := io.Copy(os.Stdout, file)
				offsets[path] += n
				_ = file.Close()
				if e != nil {
					return e
				}
			}
		}
		if !found {
			return &domain.Fault{Code: 4, Kind: "not_found", Message: "Step not found in Run"}
		}
		terminal := view["state"] != "waiting" && view["state"] != "running" && view["state"] != "blocked"
		if !follow || terminal {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

package local

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/agentwiki/todoable/internal/domain"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Config struct {
	Version               int               `json:"version"`
	MaxRunningRuns        int               `json:"max_running_runs"`
	MaxCheckProcesses     int               `json:"max_check_processes"`
	MaxPendingSubmissions int               `json:"max_pending_submissions"`
	MaxPendingPerInputKey int               `json:"max_pending_per_input_key"`
	MaxInputBytes         int               `json:"max_input_bytes"`
	MaxManifestBytes      int               `json:"max_manifest_bytes"`
	MaxRepeat             int               `json:"max_repeat"`
	MaxCallsPerRun        int               `json:"max_calls_per_run"`
	StepLogBytes          int64             `json:"step_log_bytes"`
	CompletedLogRetention string            `json:"completed_log_retention"`
	CompletedLogBytes     int64             `json:"completed_log_bytes"`
	Caps                  map[string]string `json:"caps"`
}

func DefaultConfig() Config {
	return Config{1, 2, 4, 1000, 100, 1048576, 1048576, 1000, 100, 10485760, "720h", 1073741824, map[string]string{"repeat_delay": "24h", "start_poll_every": "24h", "start_wait_timeout": "720h", "start_check_timeout": "5m", "finish_check_timeout": "5m", "before_timeout": "1h", "agent_timeout": "4h", "after_timeout": "1h", "run_timeout": "24h"}}
}
func ReadConfig(dir string) (Config, error) {
	c := DefaultConfig()
	raw, e := ReadFileLimit(filepath.Join(dir, "config.yaml"), 1048576)
	if errors.Is(e, os.ErrNotExist) {
		return c, nil
	}
	if e != nil {
		return c, e
	}
	raw, e = manifestJSON(raw, 1048576)
	if e != nil {
		return c, e
	}
	if e = domain.Decode(raw, &c); e != nil {
		return c, e
	}
	if c.Version != 1 || c.MaxRunningRuns <= 0 || c.MaxCheckProcesses <= 0 || c.MaxPendingSubmissions <= 0 || c.MaxPendingPerInputKey <= 0 || c.MaxPendingPerInputKey > c.MaxPendingSubmissions || c.MaxInputBytes <= 0 || c.MaxInputBytes > int(^uint(0)>>1)-16385 || c.MaxManifestBytes <= 0 || c.MaxRepeat < 0 || c.MaxCallsPerRun <= 0 || c.StepLogBytes <= 0 || c.CompletedLogBytes <= 0 || !duration(c.CompletedLogRetention, time.Nanosecond, time.Duration(1<<63-1)) {
		return c, domain.Invalid("invalid config limits")
	}
	defaults := DefaultConfig().Caps
	for k, v := range c.Caps {
		if _, ok := defaults[k]; !ok || !duration(v, time.Nanosecond, time.Duration(1<<63-1)) {
			return c, domain.Invalid("invalid config cap: " + k)
		}
	}
	if c.Caps == nil {
		return c, domain.Invalid("caps must be an object")
	}
	for k, v := range defaults {
		if _, ok := c.Caps[k]; !ok {
			c.Caps[k] = v
		}
	}
	return c, nil
}
func (s *Store) InputLimit() int                           { return s.config.MaxInputBytes }
func (s *Store) ParseTask(raw []byte) (domain.Task, error) { return parseTask(raw, s.config) }
func (s *Store) ReadManifest(path string) ([]byte, error) {
	return ReadFileLimit(path, s.config.MaxManifestBytes)
}
func (s *Store) ReadSubmission(path string) ([]byte, error) {
	return ReadFileLimit(path, s.config.MaxInputBytes+16384)
}

func (s *Store) ConfigHash() string {
	raw, _ := json.Marshal(s.config)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CheckCLIConfig compares against the configuration held by the live lock owner.
// Stale lock contents have no authority after the daemon exits.
func (s *Store) CheckCLIConfig() error {
	lock, e := os.OpenFile(filepath.Join(s.dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer func() { _ = lock.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		e = syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if e == nil {
			return nil
		}
		if !errors.Is(e, syscall.EWOULDBLOCK) {
			return e
		}
		raw := make([]byte, 65)
		n, readErr := lock.ReadAt(raw, 0)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if string(raw[:n]) == s.ConfigHash() {
			return nil
		}
		if !time.Now().Before(deadline) {
			return &domain.Fault{Code: 6, Kind: "config_mismatch", Message: "configuration differs from running daemon; restart the daemon"}
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func (s *Store) WorkerCount() int { return s.config.MaxRunningRuns + s.config.MaxCheckProcesses }
func validateTaskCaps(task domain.Task, config Config) error {
	if task.Repeat > config.MaxRepeat || task.Finish.MaxCalls > config.MaxCallsPerRun {
		return domain.Invalid("selected Task version exceeds current installation caps")
	}
	if !duration(task.RepeatDelay, 0, capDuration(config, "repeat_delay")) {
		return domain.Invalid("repeat_delay exceeds current installation cap")
	}
	if task.Start != nil && (!duration(task.Start.PollEvery, time.Second, capDuration(config, "start_poll_every")) || !duration(task.Start.WaitTimeout, 0, capDuration(config, "start_wait_timeout"))) {
		return domain.Invalid("start duration exceeds current installation cap")
	}
	for k, v := range task.Limits {
		if !duration(v, time.Nanosecond, capDuration(config, k)) {
			return domain.Invalid("Task limit exceeds current installation cap: " + k)
		}
	}
	return nil
}

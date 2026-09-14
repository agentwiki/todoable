package domain

import "encoding/json"

type CheckFeedback struct {
	Kind      string `json:"kind"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
}
type Execution struct {
	StepID, RunID, SubmissionID, TaskID, InputKey, Stage string
	TaskVersion, RunSeq, CallIndex                       int
	Input                                                json.RawMessage
	Task                                                 Task
	LastCheck                                            *CheckFeedback
	TimeoutNS                                            int64
	Phase                                                string
}
type ProcessIdentity struct {
	StepID, BootID, ProcessStart string
	PID, PGID                    int
}
type Outcome struct {
	Kind                 string `json:"kind"`
	ExitCode             int    `json:"exit_code"`
	Stdout               string `json:"stdout"`
	Stderr               string `json:"stderr"`
	Truncated            bool   `json:"truncated"`
	StdoutPath           string `json:"stdout_path"`
	StderrPath           string `json:"stderr_path"`
	PID, PGID            int
	BootID, ProcessStart string
	Untracked            bool `json:"untracked_processes,omitempty"`
	ElapsedNS            int64
}

// NextStage distinguishes an observed nonzero exit from an unobserved effect.
func NextStage(e Execution, o Outcome) string {
	if o.Kind == "budget_exhausted" {
		return "failed:run_timeout"
	}
	if o.Kind == "process_unknown" {
		return "blocked:process_unknown"
	}
	check := e.Stage == "start_check" || e.Stage == "finish_check"
	if o.Kind != "exited" && (o.Kind != "start_failed" || check) {
		if check {
			return "blocked:check_error"
		}
		return "blocked:outcome_unknown"
	}
	switch e.Stage {
	case "start_check":
		if o.ExitCode != 0 {
			return "waiting"
		}
		if e.Phase == "preflight" {
			return "ready"
		}
		if len(e.Task.Before) > 0 {
			return "before"
		}
		return "finish_check"
	case "before":
		if o.Kind == "start_failed" || o.ExitCode != 0 {
			return "failed:before"
		}
		return "finish_check"
	case "agent":
		return "finish_check"
	case "finish_check":
		if o.ExitCode == 0 {
			if len(e.Task.After) > 0 {
				return "after"
			}
			return "succeeded"
		}
		if e.CallIndex >= e.Task.Finish.MaxCalls {
			return "failed:max_calls"
		}
		return "agent"
	case "after":
		if o.Kind == "start_failed" || o.ExitCode != 0 {
			return "failed:after"
		}
		return "succeeded"
	}
	return "blocked:outcome_unknown"
}

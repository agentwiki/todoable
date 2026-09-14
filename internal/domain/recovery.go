package domain

import "strings"

type ResumeRequest struct {
	RunID            string
	StepID           string
	Action           string
	Reason           string
	ExitCode         *int
	ProcessesStopped bool
}

func ValidateResume(r ResumeRequest) error {
	if r.RunID == "" || r.StepID == "" || strings.TrimSpace(r.Reason) == "" {
		return Invalid("resume requires a Run, Step and nonempty reason")
	}
	switch r.Action {
	case "retry", "confirm-success":
		if r.ExitCode != nil {
			return Invalid("exit code is only valid for confirm-failure")
		}
	case "confirm-failure":
		if r.ExitCode == nil || *r.ExitCode < 1 || *r.ExitCode > 255 {
			return Invalid("confirm-failure requires exit code 1..255")
		}
	default:
		return Invalid("unknown resume action")
	}
	return nil
}

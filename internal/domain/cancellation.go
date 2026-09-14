package domain

import "strings"

type CancelRequest struct {
	SubmissionID       string
	Reason             string
	AcknowledgeEffects bool
	ProcessesStopped   bool
}

func ValidateCancel(r CancelRequest) error {
	if r.SubmissionID == "" || strings.TrimSpace(r.Reason) == "" {
		return Invalid("cancellation requires a submission and nonempty reason")
	}
	return nil
}

type TaskCancelRequest struct {
	TaskID string
	Reason string
}

func ValidateTaskCancel(request TaskCancelRequest) error {
	if !ValidID(request.TaskID) || strings.TrimSpace(request.Reason) == "" {
		return Invalid("Task cancellation requires an ID and nonempty reason")
	}
	return nil
}

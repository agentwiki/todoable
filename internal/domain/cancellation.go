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

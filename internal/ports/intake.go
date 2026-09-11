package ports

import "github.com/agentwiki/todoable/internal/domain"

type IntakeStore interface {
	Register(domain.Task) (int, error)
	Submit(domain.SubmissionInput) (domain.Result, error)
}

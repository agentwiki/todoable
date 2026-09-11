package ports

import "github.com/agentwiki/todoable/internal/domain"

type IntakeStore interface {
	Register(domain.Task) (int, error)
	Update(domain.Task, int) (int, error)
	SetEnabled(string, bool) error
	Submit(domain.SubmissionInput) (domain.Result, error)
}

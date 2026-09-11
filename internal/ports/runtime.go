package ports

import (
	"context"
	"github.com/agentwiki/todoable/internal/domain"
)

type RuntimeStore interface {
	Reserve() (*domain.Execution, error)
	Complete(domain.Execution, domain.Outcome, string) error
}
type Executor interface {
	Execute(context.Context, domain.Execution) (domain.Outcome, error)
}

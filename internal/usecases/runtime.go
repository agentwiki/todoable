package usecases

import (
	"context"
	"github.com/agentwiki/todoable/internal/domain"
	"github.com/agentwiki/todoable/internal/ports"
)

// Advance reserves durable intent before issuing any external command.
func Advance(ctx context.Context, store ports.RuntimeStore, runner ports.Executor) (bool, error) {
	execution, err := store.Reserve()
	if err != nil || execution == nil {
		return false, err
	}
	outcome, err := runner.Execute(ctx, *execution)
	if err != nil {
		return true, err
	}
	return true, store.Complete(*execution, outcome, domain.NextStage(*execution, outcome))
}

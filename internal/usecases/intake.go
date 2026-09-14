package usecases

import (
	"github.com/agentwiki/todoable/internal/domain"
	"github.com/agentwiki/todoable/internal/ports"
)

func Register(store ports.IntakeStore, task domain.Task) (int, error) { return store.Register(task) }
func Submit(store ports.IntakeStore, raw []byte) (domain.Result, error) {
	in, err := domain.ParseSubmissionLimit(raw, store.InputLimit())
	if err != nil {
		return domain.Result{}, err
	}
	return store.Submit(in)
}

func Update(store ports.IntakeStore, task domain.Task, expected int) (int, error) {
	return store.Update(task, expected)
}
func SetEnabled(store ports.IntakeStore, id string, enabled bool) error {
	return store.SetEnabled(id, enabled)
}

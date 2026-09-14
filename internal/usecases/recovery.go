package usecases

import (
	"github.com/agentwiki/todoable/internal/domain"
	"github.com/agentwiki/todoable/internal/ports"
)

func Resume(store ports.RecoveryStore, request domain.ResumeRequest) (map[string]any, error) {
	if err := domain.ValidateResume(request); err != nil {
		return nil, err
	}
	return store.Resume(request)
}

func Cancel(store ports.RecoveryStore, request domain.CancelRequest) (map[string]any, error) {
	if err := domain.ValidateCancel(request); err != nil {
		return nil, err
	}
	return store.Cancel(request)
}

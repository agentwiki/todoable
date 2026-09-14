package ports

import "github.com/agentwiki/todoable/internal/domain"

type RecoveryStore interface {
	Resume(domain.ResumeRequest) (map[string]any, error)
}

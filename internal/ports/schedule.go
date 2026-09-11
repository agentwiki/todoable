package ports

import (
	"github.com/agentwiki/todoable/internal/domain"
	"time"
)

type ScheduleStore interface {
	SetSchedule(string, bool) error
	Schedule(string) (map[string]any, error)
	SubmitSchedule(string, time.Time, int) (domain.Result, error)
	PublishSchedules() error
}

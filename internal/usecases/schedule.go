package usecases

import (
	"github.com/agentwiki/todoable/internal/domain"
	"github.com/agentwiki/todoable/internal/ports"
	"time"
)

func SetSchedule(store ports.ScheduleStore, id string, enabled bool) error {
	return store.SetSchedule(id, enabled)
}
func Schedule(store ports.ScheduleStore, id string) (map[string]any, error) {
	return store.Schedule(id)
}
func SubmitSchedule(store ports.ScheduleStore, id string, at time.Time, version int) (domain.Result, error) {
	return store.SubmitSchedule(id, at, version)
}
func PublishSchedules(store ports.ScheduleStore) error { return store.PublishSchedules() }

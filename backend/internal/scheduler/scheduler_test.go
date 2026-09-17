package scheduler

import (
	"testing"
	"time"

	"irrigation/internal/models"
)

func TestTriggerIdempotencyKey(t *testing.T) {
	now := time.Date(2026, 9, 17, 6, 0, 0, 0, time.Local)

	timed := models.IrrigationSchedule{ID: 7, Type: models.ScheduleTypeTimed, StartTime: "06:00"}
	timedWant := "timed:7:2026-09-17:06:00"
	if got := triggerIdempotencyKey(timed, now); got != timedWant {
		t.Errorf("timed key = %q, want %q", got, timedWant)
	}

	cond := models.IrrigationSchedule{ID: 8, Type: models.ScheduleTypeConditional}
	condWant := "cond:8:2026-09-17"
	if got := triggerIdempotencyKey(cond, now); got != condWant {
		t.Errorf("conditional key = %q, want %q", got, condWant)
	}

	// 同一计划同一天重复触发（含重启后重试）必须生成相同的键
	if again := triggerIdempotencyKey(timed, now.Add(30*time.Second)); again != timedWant {
		t.Errorf("same-day retrigger produced different key: %q, want %q", again, timedWant)
	}

	// 跨天后键应变化，允许新的一天重新预留
	if tomorrow := triggerIdempotencyKey(timed, now.AddDate(0, 0, 1)); tomorrow == timedWant {
		t.Errorf("key should differ across days, got %q", tomorrow)
	}
}

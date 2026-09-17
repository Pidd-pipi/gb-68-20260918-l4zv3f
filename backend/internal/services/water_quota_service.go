package services

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

var (
	ErrInsufficientWaterQuota   = errors.New("insufficient water quota")
	ErrInvalidWaterQuota        = errors.New("daily water quota cannot be negative")
	ErrIrrigationAlreadyHandled = errors.New("irrigation already handled")
)

type QuotaStatus struct {
	ZoneID              uint      `json:"zone_id"`
	Quota               *float64  `json:"quota"`
	Used                float64   `json:"used"`
	Reserved            float64   `json:"reserved"`
	Available           *float64  `json:"available"`
	QuotaDate           time.Time `json:"quota_date"`
	NextResetAt         time.Time `json:"next_reset_at"`
	EarliestAvailableAt time.Time `json:"earliest_available_at"`
}

type InsufficientQuotaError struct {
	Required            float64   `json:"required"`
	Used                float64   `json:"used"`
	Reserved            float64   `json:"reserved"`
	Quota               *float64  `json:"quota"`
	Available           *float64  `json:"available"`
	NextResetAt         time.Time `json:"next_reset_at"`
	EarliestAvailableAt time.Time `json:"earliest_available_at"`
}

func (e *InsufficientQuotaError) Error() string { return ErrInsufficientWaterQuota.Error() }

func (e *InsufficientQuotaError) Is(target error) bool { return target == ErrInsufficientWaterQuota }

type AutoIrrigationResult struct {
	Started      bool                         `json:"started"`
	Postponed    bool                         `json:"postponed"`
	Skipped      bool                         `json:"skipped"`
	Log          *models.IrrigationLog        `json:"log,omitempty"`
	Postponement *models.SchedulePostponement `json:"postponement,omitempty"`
}

type QuotaService struct{}

func NewQuotaService() *QuotaService { return &QuotaService{} }

func QuotaDay(now time.Time) time.Time {
	now = now.Local()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

func nextQuotaReset(now time.Time) time.Time {
	return QuotaDay(now).Add(24 * time.Hour)
}

func (s *QuotaService) GetQuotaStatus(zoneID uint, now time.Time) (*QuotaStatus, error) {
	dayStart := QuotaDay(now)
	dayEnd := dayStart.Add(24 * time.Hour)

	var zone models.IrrigationZone
	if err := database.DB.First(&zone, zoneID).Error; err != nil {
		return nil, err
	}

	used, reserved, err := zoneQuotaTotals(database.DB, zoneID, dayStart, dayEnd)
	if err != nil {
		return nil, err
	}

	status := &QuotaStatus{
		ZoneID:              zoneID,
		Quota:               zone.DailyWaterQuota,
		Used:                used,
		Reserved:            reserved,
		QuotaDate:           dayStart,
		NextResetAt:         dayEnd,
		EarliestAvailableAt: dayEnd,
	}
	if zone.DailyWaterQuota != nil {
		available := *zone.DailyWaterQuota - used - reserved
		status.Available = &available
	}
	return status, nil
}

func (s *QuotaService) SetDailyQuota(zoneID uint, quota *float64) error {
	if quota != nil && *quota < 0 {
		return ErrInvalidWaterQuota
	}
	now := time.Now()
	return database.DB.Transaction(func(tx *gorm.DB) error {
		var posts []models.SchedulePostponement
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("zone_id = ? AND status = ?", zoneID, models.SchedulePostponementPending).
			Find(&posts).Error; err != nil {
			return err
		}

		zone, err := lockZone(tx, zoneID)
		if err != nil {
			return err
		}
		result := tx.Model(zone).Update("daily_water_quota", quota)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return recalculateLockedPostponements(tx, posts, *zone, now)
	})
}

func (s *QuotaService) ListPostponements(zoneID, scheduleID *uint, status *string, startTime, endTime time.Time, limit int) ([]models.SchedulePostponement, error) {
	var postponements []models.SchedulePostponement
	query := database.DB.Model(&models.SchedulePostponement{})
	if zoneID != nil {
		query = query.Where("zone_id = ?", *zoneID)
	}
	if scheduleID != nil {
		query = query.Where("schedule_id = ?", *scheduleID)
	}
	if status != nil {
		query = query.Where("status = ?", *status)
	}
	if !startTime.IsZero() {
		query = query.Where("quota_date >= ?", QuotaDay(startTime))
	}
	if !endTime.IsZero() {
		query = query.Where("quota_date <= ?", QuotaDay(endTime))
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	err := query.Order("earliest_execute_at DESC, id DESC").Limit(limit).Find(&postponements).Error
	return postponements, err
}

func (s *QuotaService) ListDuePostponements(now time.Time) ([]models.SchedulePostponement, error) {
	var postponements []models.SchedulePostponement
	err := database.DB.Where("status = ? AND earliest_execute_at <= ?",
		models.SchedulePostponementPending, now).
		Order("earliest_execute_at ASC, id ASC").
		Find(&postponements).Error
	return postponements, err
}

func zoneQuotaTotals(tx *gorm.DB, zoneID uint, dayStart, dayEnd time.Time) (float64, float64, error) {
	var used, reserved float64
	if err := tx.Model(&models.IrrigationLog{}).
		Where("zone_id = ? AND status = ? AND start_time >= ? AND start_time < ?",
			zoneID, models.ExecutionStatusSuccess, dayStart, dayEnd).
		Select("COALESCE(SUM(water_usage), 0)").Scan(&used).Error; err != nil {
		return 0, 0, err
	}
	if err := tx.Model(&models.WaterReservation{}).
		Where("zone_id = ? AND quota_date = ? AND status = ?",
			zoneID, dayStart, models.WaterReservationActive).
		Select("COALESCE(SUM(requested), 0)").Scan(&reserved).Error; err != nil {
		return 0, 0, err
	}
	return used, reserved, nil
}

func insufficientError(zone models.IrrigationZone, required, used, reserved float64, resetAt time.Time) *InsufficientQuotaError {
	err := &InsufficientQuotaError{
		Required:            required,
		Used:                used,
		Reserved:            reserved,
		Quota:               zone.DailyWaterQuota,
		NextResetAt:         resetAt,
		EarliestAvailableAt: resetAt,
	}
	if zone.DailyWaterQuota != nil {
		available := *zone.DailyWaterQuota - used - reserved
		err.Available = &available
	}
	return err
}

func lockZone(tx *gorm.DB, zoneID uint) (*models.IrrigationZone, error) {
	var zone models.IrrigationZone
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&zone, zoneID).Error; err != nil {
		return nil, err
	}
	return &zone, nil
}

func scheduleTriggerHandled(tx *gorm.DB, scheduleID uint, trigger models.TriggerType, dayStart, dayEnd time.Time) (bool, error) {
	var activeCount int64
	if err := tx.Model(&models.WaterReservation{}).
		Where("schedule_id = ? AND trigger_type = ? AND quota_date = ? AND status = ?",
			scheduleID, trigger, dayStart, models.WaterReservationActive).
		Count(&activeCount).Error; err != nil {
		return false, err
	}
	if activeCount > 0 {
		return true, nil
	}

	var successCount int64
	if err := tx.Model(&models.IrrigationLog{}).
		Where("schedule_id = ? AND trigger_type = ? AND status = ? AND start_time >= ? AND start_time < ?",
			scheduleID, trigger, models.ExecutionStatusSuccess, dayStart, dayEnd).
		Count(&successCount).Error; err != nil {
		return false, err
	}
	if successCount > 0 {
		return true, nil
	}

	var postCount int64
	if err := tx.Model(&models.SchedulePostponement{}).
		Where("schedule_id = ? AND trigger_type = ? AND quota_date = ? AND status IN ?",
			scheduleID, trigger, dayStart, []models.SchedulePostponementStatus{
				models.SchedulePostponementPending,
				models.SchedulePostponementExecuting,
				models.SchedulePostponementCompleted,
			}).
		Count(&postCount).Error; err != nil {
		return false, err
	}
	return postCount > 0, nil
}

func createLogAndReservation(tx *gorm.DB, scheduleID, zoneID *uint, trigger models.TriggerType, requested float64, now, dayStart time.Time) (*models.IrrigationLog, *models.WaterReservation, error) {
	log := &models.IrrigationLog{
		ScheduleID:  scheduleID,
		ZoneID:      zoneID,
		TriggerType: trigger,
		StartTime:   now,
		Status:      models.ExecutionStatusInProgress,
	}
	if err := tx.Create(log).Error; err != nil {
		return nil, nil, err
	}

	reservation := &models.WaterReservation{
		ScheduleID:  scheduleID,
		ZoneID:      zoneID,
		LogID:       &log.ID,
		TriggerType: trigger,
		QuotaDate:   dayStart,
		Requested:   requested,
		Status:      models.WaterReservationActive,
	}
	if err := tx.Create(reservation).Error; err != nil {
		return nil, nil, err
	}
	return log, reservation, nil
}

// StartScheduledIrrigation reserves water for an automatic trigger. If quota is
// unavailable it records a postponement instead of creating an in-progress log.
func (s *QuotaService) StartScheduledIrrigation(schedule models.IrrigationSchedule, trigger models.TriggerType, required float64) (*AutoIrrigationResult, error) {
	if required < 0 {
		return nil, errors.New("required water amount cannot be negative")
	}
	if schedule.ZoneID == nil {
		return nil, errors.New("schedule zone is required for water quota control")
	}

	now := time.Now()
	dayStart := QuotaDay(now)
	dayEnd := dayStart.Add(24 * time.Hour)
	zoneID := *schedule.ZoneID
	result := &AutoIrrigationResult{}

	err := database.DB.Transaction(func(tx *gorm.DB) error {
		zone, err := lockZone(tx, zoneID)
		if err != nil {
			return err
		}

		var current models.IrrigationSchedule
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&current, schedule.ID).Error; err != nil {
			return err
		}
		if current.Status != models.ScheduleStatusActive {
			result.Skipped = true
			return nil
		}

		handled, err := scheduleTriggerHandled(tx, schedule.ID, trigger, dayStart, dayEnd)
		if err != nil {
			return err
		}
		if handled {
			result.Skipped = true
			return nil
		}

		used, reserved, err := zoneQuotaTotals(tx, zoneID, dayStart, dayEnd)
		if err != nil {
			return err
		}
		if zone.DailyWaterQuota != nil && used+reserved+required > *zone.DailyWaterQuota {
			postponement := &models.SchedulePostponement{
				ScheduleID:        schedule.ID,
				ZoneID:            &zoneID,
				TriggerType:       trigger,
				QuotaDate:         dayStart,
				RequiredAmount:    required,
				EarliestExecuteAt: dayEnd,
				Status:            models.SchedulePostponementPending,
				Reason:            "区域日供水额度不足",
			}
			if err := tx.Create(postponement).Error; err != nil {
				return err
			}
			result.Postponed = true
			result.Postponement = postponement
			return nil
		}

		log, _, err := createLogAndReservation(tx, &schedule.ID, &zoneID, trigger, required, now, dayStart)
		if err != nil {
			return err
		}
		result.Started = true
		result.Log = log
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *QuotaService) StartManualIrrigation(zoneID uint, requested float64) (*models.IrrigationLog, *QuotaStatus, error) {
	if requested < 0 {
		return nil, nil, errors.New("requested water amount cannot be negative")
	}

	now := time.Now()
	dayStart := QuotaDay(now)
	dayEnd := dayStart.Add(24 * time.Hour)
	var log *models.IrrigationLog
	var status *QuotaStatus

	err := database.DB.Transaction(func(tx *gorm.DB) error {
		zone, err := lockZone(tx, zoneID)
		if err != nil {
			return err
		}
		used, reserved, err := zoneQuotaTotals(tx, zoneID, dayStart, dayEnd)
		if err != nil {
			return err
		}
		if zone.DailyWaterQuota != nil && used+reserved+requested > *zone.DailyWaterQuota {
			return insufficientError(*zone, requested, used, reserved, dayEnd)
		}
		log, _, err = createLogAndReservation(tx, nil, &zoneID, models.TriggerTypeManual, requested, now, dayStart)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrInsufficientWaterQuota) {
			status, _ = s.GetQuotaStatus(zoneID, now)
		}
		return nil, status, err
	}
	status, _ = s.GetQuotaStatus(zoneID, now)
	return log, status, nil
}

// ExecuteDuePostponement starts one postponed automatic irrigation when quota
// has recovered. Compensation is charged to the quota day on which it runs.
func (s *QuotaService) ExecuteDuePostponement(postponementID uint) (*AutoIrrigationResult, error) {
	now := time.Now()
	result := &AutoIrrigationResult{}

	err := database.DB.Transaction(func(tx *gorm.DB) error {
		var post models.SchedulePostponement
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&post, postponementID).Error; err != nil {
			return err
		}
		if post.Status != models.SchedulePostponementPending || post.EarliestExecuteAt.After(now) {
			result.Skipped = true
			return nil
		}
		if post.ZoneID == nil {
			return errors.New("postponement has no zone")
		}

		zone, err := lockZone(tx, *post.ZoneID)
		if err != nil {
			return err
		}
		var schedule models.IrrigationSchedule
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&schedule, post.ScheduleID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				markPostponementCanceled(tx, &post, "灌溉计划已移除")
				result.Skipped = true
				return nil
			}
			return err
		}
		if schedule.Status != models.ScheduleStatusActive {
			markPostponementCanceled(tx, &post, "灌溉计划已停用")
			result.Skipped = true
			return nil
		}

		reserveDay := QuotaDay(now)
		reserveDayEnd := reserveDay.Add(24 * time.Hour)
		handled, err := scheduleTriggerHandled(tx, schedule.ID, post.TriggerType, reserveDay, reserveDayEnd)
		if err != nil {
			return err
		}
		if handled {
			completedAt := now
			post.Status = models.SchedulePostponementCompleted
			post.ExecutedAt = &completedAt
			if err := tx.Model(&post).Updates(map[string]interface{}{
				"status":      post.Status,
				"executed_at": post.ExecutedAt,
				"reason":      "检测到同一计划当天已补偿或执行",
			}).Error; err != nil {
				return err
			}
			result.Skipped = true
			return nil
		}

		used, reserved, err := zoneQuotaTotals(tx, *post.ZoneID, reserveDay, reserveDayEnd)
		if err != nil {
			return err
		}
		if zone.DailyWaterQuota != nil && used+reserved+post.RequiredAmount > *zone.DailyWaterQuota {
			post.EarliestExecuteAt = reserveDayEnd
			if err := tx.Model(&post).Update("earliest_execute_at", reserveDayEnd).Error; err != nil {
				return err
			}
			result.Postponement = &post
			return nil
		}

		log, reservation, err := createLogAndReservation(tx, &schedule.ID, post.ZoneID, post.TriggerType, post.RequiredAmount, now, reserveDay)
		if err != nil {
			return err
		}
		post.Status = models.SchedulePostponementExecuting
		post.ReservationID = &reservation.ID
		post.ExecutedAt = &now
		if err := tx.Model(&post).Updates(map[string]interface{}{
			"status":         post.Status,
			"reservation_id": post.ReservationID,
			"executed_at":    post.ExecutedAt,
		}).Error; err != nil {
			return err
		}

		result.Started = true
		result.Log = log
		result.Postponement = &post
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// recalculateLockedPostponements moves pending records to the current quota
// day's schedule when released quota has made enough room. Callers must not hold
// the zone row lock when invoking it; this function locks posts before the zone
// to keep the same lock order as ExecuteDuePostponement.
func recalculateZonePostponements(tx *gorm.DB, zoneID uint, now time.Time) error {
	var posts []models.SchedulePostponement
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("zone_id = ? AND status = ?", zoneID, models.SchedulePostponementPending).
		Order("earliest_execute_at ASC, id ASC").
		Find(&posts).Error; err != nil {
		return err
	}
	zone, err := lockZone(tx, zoneID)
	if err != nil {
		return err
	}
	return recalculateLockedPostponements(tx, posts, *zone, now)
}

func recalculateLockedPostponements(tx *gorm.DB, posts []models.SchedulePostponement, zone models.IrrigationZone, now time.Time) error {
	dayStart := QuotaDay(now)
	dayEnd := dayStart.Add(24 * time.Hour)
	used, reserved, err := zoneQuotaTotals(tx, zone.ID, dayStart, dayEnd)
	if err != nil {
		return err
	}

	tentative := 0.0
	for _, post := range posts {
		earliest := dayEnd
		if zone.DailyWaterQuota == nil || used+reserved+tentative+post.RequiredAmount <= *zone.DailyWaterQuota {
			earliest = now
			tentative += post.RequiredAmount
		}
		if !post.EarliestExecuteAt.Equal(earliest) {
			if err := tx.Model(&models.SchedulePostponement{}).
				Where("id = ? AND status = ?", post.ID, models.SchedulePostponementPending).
				Update("earliest_execute_at", earliest).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func markPostponementCanceled(tx *gorm.DB, post *models.SchedulePostponement, reason string) {
	now := time.Now()
	post.Status = models.SchedulePostponementCanceled
	post.CanceledAt = &now
	post.Reason = reason
	tx.Model(post).Updates(map[string]interface{}{
		"status":      post.Status,
		"canceled_at": post.CanceledAt,
		"reason":      reason,
	})
}

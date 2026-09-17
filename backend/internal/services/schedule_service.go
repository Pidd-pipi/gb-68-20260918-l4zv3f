package services

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

type ScheduleService struct{}

func NewScheduleService() *ScheduleService {
	return &ScheduleService{}
}

func (s *ScheduleService) CreateSchedule(schedule *models.IrrigationSchedule) error {
	return database.DB.Create(schedule).Error
}

func (s *ScheduleService) GetScheduleByID(id uint) (*models.IrrigationSchedule, error) {
	var schedule models.IrrigationSchedule
	if err := database.DB.First(&schedule, id).Error; err != nil {
		return nil, err
	}
	return &schedule, nil
}

func (s *ScheduleService) ListSchedules(zoneID *uint, status *string) ([]models.IrrigationSchedule, error) {
	var schedules []models.IrrigationSchedule
	query := database.DB

	if zoneID != nil {
		query = query.Where("zone_id = ?", *zoneID)
	}
	if status != nil {
		query = query.Where("status = ?", *status)
	}

	if err := query.Find(&schedules).Error; err != nil {
		return nil, err
	}
	return schedules, nil
}

func (s *ScheduleService) ListActiveSchedules() ([]models.IrrigationSchedule, error) {
	var schedules []models.IrrigationSchedule
	err := database.DB.Where("status = ?", models.ScheduleStatusActive).Find(&schedules).Error
	return schedules, err
}

func (s *ScheduleService) UpdateSchedule(id uint, updates map[string]interface{}) error {
	return database.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.IrrigationSchedule{}).Where("id = ?", id).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("schedule not found")
		}
		if status, ok := updates["status"]; ok && status == models.ScheduleStatusInactive {
			return releaseScheduleQuotasTx(tx, id, "灌溉计划已停用")
		}
		return nil
	})
}

func (s *ScheduleService) SetScheduleStatus(id uint, status models.ScheduleStatus) error {
	return database.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.IrrigationSchedule{}).
			Where("id = ?", id).
			Update("status", status)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("schedule not found")
		}
		if status == models.ScheduleStatusInactive {
			return releaseScheduleQuotasTx(tx, id, "灌溉计划已停用")
		}
		return nil
	})
}

func (s *ScheduleService) DeleteSchedule(id uint) error {
	return database.DB.Transaction(func(tx *gorm.DB) error {
		var schedule models.IrrigationSchedule
		if err := tx.First(&schedule, id).Error; err != nil {
			return errors.New("schedule not found")
		}
		if err := releaseScheduleQuotasTx(tx, id, "灌溉计划已移除"); err != nil {
			return err
		}
		return tx.Delete(&models.IrrigationSchedule{}, id).Error
	})
}

func releaseScheduleQuotasTx(tx *gorm.DB, id uint, reason string) error {
	now := time.Now()
	if err := tx.Model(&models.IrrigationLog{}).
		Where("schedule_id = ? AND status = ?", id, models.ExecutionStatusInProgress).
		Updates(map[string]interface{}{
			"status":        models.ExecutionStatusFailed,
			"end_time":      now,
			"error_message": reason,
		}).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.WaterReservation{}).
		Where("schedule_id = ? AND status = ?", id, models.WaterReservationActive).
		Updates(map[string]interface{}{
			"status":      models.WaterReservationReleased,
			"released_at": now,
		}).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.SchedulePostponement{}).
		Where("schedule_id = ? AND status IN ?", id, []models.SchedulePostponementStatus{
			models.SchedulePostponementPending,
			models.SchedulePostponementExecuting,
		}).
		Updates(map[string]interface{}{
			"status":      models.SchedulePostponementCanceled,
			"canceled_at": now,
			"reason":      reason,
		}).Error; err != nil {
		return err
	}
	return nil
}

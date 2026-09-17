package services

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// CompleteIrrigation finalizes an execution and converts the water reservation
// into actual usage, or releases it when execution failed.
func (s *QuotaService) CompleteIrrigation(logID uint, success bool, waterUsage *float64, errorMsg *string) error {
	_, err := s.completeIrrigation(logID, success, waterUsage, errorMsg, 0)
	return err
}

// CompletePostponedIrrigation atomically finalizes an execution started from a
// postponement and marks that postponement completed or failed.
func (s *QuotaService) CompletePostponedIrrigation(logID, postponementID uint, success bool, waterUsage *float64, errorMsg *string) error {
	_, err := s.completeIrrigation(logID, success, waterUsage, errorMsg, postponementID)
	return err
}

func (s *QuotaService) completeIrrigation(logID uint, success bool, waterUsage *float64, errorMsg *string, postponementID uint) (*uint, error) {
	now := time.Now()
	var zoneID *uint

	err := database.DB.Transaction(func(tx *gorm.DB) error {
		var log models.IrrigationLog
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&log, logID).Error; err != nil {
			return err
		}
		if log.Status != models.ExecutionStatusInProgress {
			return ErrIrrigationAlreadyHandled
		}
		zoneID = log.ZoneID

		if postponementID > 0 {
			var post models.SchedulePostponement
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				First(&post, postponementID).Error; err != nil {
				return err
			}
			if post.Status != models.SchedulePostponementExecuting {
				return ErrIrrigationAlreadyHandled
			}
		}

		var reservation models.WaterReservation
		findErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("log_id = ? AND status = ?", logID, models.WaterReservationActive).
			First(&reservation).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		hasReservation := findErr == nil

		updates := map[string]interface{}{"end_time": now}
		if success {
			updates["status"] = models.ExecutionStatusSuccess
		} else {
			updates["status"] = models.ExecutionStatusFailed
			if errorMsg != nil {
				updates["error_message"] = *errorMsg
			}
		}
		if waterUsage != nil {
			updates["water_usage"] = *waterUsage
		}
		if err := tx.Model(&log).Updates(updates).Error; err != nil {
			return err
		}

		if hasReservation {
			if success {
				actual := reservation.Requested
				if waterUsage != nil {
					actual = *waterUsage
				}
				if err := tx.Model(&reservation).Updates(map[string]interface{}{
					"status":       models.WaterReservationConsumed,
					"actual_usage": actual,
					"consumed_at":  now,
				}).Error; err != nil {
					return err
				}
			} else {
				if err := tx.Model(&reservation).Updates(map[string]interface{}{
					"status":      models.WaterReservationReleased,
					"released_at": now,
				}).Error; err != nil {
					return err
				}
			}
		}

		if postponementID > 0 {
			postUpdates := map[string]interface{}{}
			if success {
				postUpdates["status"] = models.SchedulePostponementCompleted
				postUpdates["executed_at"] = now
			} else {
				postUpdates["status"] = models.SchedulePostponementFailed
				postUpdates["failed_at"] = now
				if errorMsg != nil {
					postUpdates["reason"] = *errorMsg
				}
			}
			if err := tx.Model(&models.SchedulePostponement{}).
				Where("id = ? AND status = ?", postponementID, models.SchedulePostponementExecuting).
				Updates(postUpdates).Error; err != nil {
				return err
			}
		}

		// Re-evaluate waiting compensation after reserved water becomes actual
		// usage or is released. Lower actual usage can also free quota early.
		if zoneID != nil {
			return recalculateZonePostponements(tx, *zoneID, now)
		}
		return nil
	})
	return zoneID, err
}

// FailStartedIrrigation releases a reservation when the process cannot begin
// running an irrigation after quota reservation succeeded.
func (s *QuotaService) FailStartedIrrigation(logID uint, errorMsg string) error {
	usage := (*float64)(nil)
	msg := errorMsg
	return s.CompleteIrrigation(logID, false, usage, &msg)
}

// RecoverInterruptedQuotas is called at startup. Active in-progress executions
// could not have completed during an unexpected shutdown, so reservations are
// released and executing postponements return to pending for one retry.
func (s *QuotaService) RecoverInterruptedQuotas() error {
	now := time.Now()
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.IrrigationLog{}).
			Where("status = ?", models.ExecutionStatusInProgress).
			Updates(map[string]interface{}{
				"status":        models.ExecutionStatusFailed,
				"end_time":      now,
				"error_message": "服务重启，释放进行中预留水量",
			}).Error; err != nil {
			return err
		}

		if err := tx.Model(&models.WaterReservation{}).
			Where("status = ?", models.WaterReservationActive).
			Updates(map[string]interface{}{
				"status":      models.WaterReservationReleased,
				"released_at": now,
			}).Error; err != nil {
			return err
		}

		return tx.Model(&models.SchedulePostponement{}).
			Where("status = ?", models.SchedulePostponementExecuting).
			Updates(map[string]interface{}{
				"status":         models.SchedulePostponementPending,
				"reservation_id": nil,
				"executed_at":    nil,
				"reason":         "服务重启后等待额度恢复补偿",
			}).Error
	}); err != nil {
		return err
	}

	var zoneIDs []uint
	if err := database.DB.Model(&models.SchedulePostponement{}).
		Where("status = ?", models.SchedulePostponementPending).
		Distinct().Pluck("zone_id", &zoneIDs).Error; err != nil {
		return err
	}
	for _, zoneID := range zoneIDs {
		if err := database.DB.Transaction(func(tx *gorm.DB) error {
			return recalculateZonePostponements(tx, zoneID, now)
		}); err != nil {
			return err
		}
	}
	return nil
}

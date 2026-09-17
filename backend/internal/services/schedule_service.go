package services

import (
	"errors"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

type ScheduleService struct {
	quotaService *WaterQuotaService
}

func NewScheduleService() *ScheduleService {
	return &ScheduleService{
		quotaService: NewWaterQuotaService(),
	}
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
	result := database.DB.Model(&models.IrrigationSchedule{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("schedule not found")
	}
	return nil
}

func (s *ScheduleService) SetScheduleStatus(id uint, status models.ScheduleStatus) error {
	if err := s.UpdateSchedule(id, map[string]interface{}{"status": status}); err != nil {
		return err
	}
	if status == models.ScheduleStatusInactive {
		// 计划停用时释放其在途预留额度，并取消待补偿的顺延记录
		if err := s.quotaService.ReleaseReservationsBySchedule(id); err != nil {
			return err
		}
		return s.quotaService.CancelDeferralsBySchedule(id)
	}
	return nil
}

func (s *ScheduleService) DeleteSchedule(id uint) error {
	result := database.DB.Delete(&models.IrrigationSchedule{}, id)
	if result.RowsAffected == 0 {
		return errors.New("schedule not found")
	}
	if result.Error != nil {
		return result.Error
	}
	// 计划移除时释放其在途预留额度，并取消待补偿的顺延记录
	if err := s.quotaService.ReleaseReservationsBySchedule(id); err != nil {
		return err
	}
	return s.quotaService.CancelDeferralsBySchedule(id)
}

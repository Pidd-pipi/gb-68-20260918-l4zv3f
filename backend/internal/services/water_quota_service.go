package services

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// 水量估算系数，与调度器中的用水量估算口径保持一致（升/秒）
const WaterEstimatePerSecond = 0.1

// EstimateWaterAmount 按灌溉时长估算所需水量
func EstimateWaterAmount(durationSeconds int) float64 {
	if durationSeconds <= 0 {
		return 0
	}
	return float64(durationSeconds) * WaterEstimatePerSecond
}

// ErrInsufficientQuota 区域当日供水额度不足
var ErrInsufficientQuota = errors.New("insufficient daily water quota")

type WaterQuotaService struct{}

func NewWaterQuotaService() *WaterQuotaService {
	return &WaterQuotaService{}
}

// SetZoneQuota 设置或更新区域日供水额度
func (s *WaterQuotaService) SetZoneQuota(zoneID uint, dailyQuota float64) (*models.ZoneWaterQuota, error) {
	if dailyQuota < 0 {
		return nil, errors.New("daily_quota must not be negative")
	}

	var quota models.ZoneWaterQuota
	err := database.DB.Where("zone_id = ?", zoneID).First(&quota).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		quota = models.ZoneWaterQuota{ZoneID: zoneID, DailyQuota: dailyQuota}
		if err := database.DB.Create(&quota).Error; err != nil {
			return nil, err
		}
		return &quota, nil
	}

	if err := database.DB.Model(&quota).Update("daily_quota", dailyQuota).Error; err != nil {
		return nil, err
	}
	return &quota, nil
}

// GetZoneQuota 获取区域日供水额度，未设置时返回 nil
func (s *WaterQuotaService) GetZoneQuota(zoneID uint) (*models.ZoneWaterQuota, error) {
	var quota models.ZoneWaterQuota
	err := database.DB.Where("zone_id = ?", zoneID).First(&quota).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &quota, nil
}

// QuotaUsage 区域当日额度使用情况
type QuotaUsage struct {
	ZoneID     uint    `json:"zone_id"`
	Date       string  `json:"date"`
	Configured bool    `json:"configured"`
	DailyQuota float64 `json:"daily_quota"`
	Used       float64 `json:"used"`
	Reserved   float64 `json:"reserved"`
	Available  float64 `json:"available"`
}

// GetQuotaUsage 查询区域当日已用、在途与可用额度
func (s *WaterQuotaService) GetQuotaUsage(zoneID uint, day time.Time) (*QuotaUsage, error) {
	usage := &QuotaUsage{ZoneID: zoneID, Date: day.Format("2006-01-02")}

	quota, err := s.GetZoneQuota(zoneID)
	if err != nil {
		return nil, err
	}
	if quota == nil {
		return usage, nil
	}
	usage.Configured = true
	usage.DailyQuota = quota.DailyQuota

	used, reserved, err := s.sumUsedAndReserved(zoneID, day)
	if err != nil {
		return nil, err
	}
	usage.Used = used
	usage.Reserved = reserved
	usage.Available = quota.DailyQuota - used - reserved
	if usage.Available < 0 {
		usage.Available = 0
	}
	return usage, nil
}

// sumUsedAndReserved 统计当日已成功执行用水量与在途预留水量
func (s *WaterQuotaService) sumUsedAndReserved(zoneID uint, day time.Time) (float64, float64, error) {
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)

	var used float64
	err := database.DB.Model(&models.IrrigationLog{}).
		Select("COALESCE(SUM(water_usage), 0)").
		Where("zone_id = ? AND status = ? AND start_time >= ? AND start_time < ?",
			zoneID, models.ExecutionStatusSuccess, dayStart, dayEnd).
		Scan(&used).Error
	if err != nil {
		return 0, 0, err
	}

	var reserved float64
	err = database.DB.Model(&models.WaterReservation{}).
		Select("COALESCE(SUM(amount), 0)").
		Where("zone_id = ? AND reserved_date = ? AND status = ?",
			zoneID, dayStart, models.ReservationStatusReserved).
		Scan(&reserved).Error
	if err != nil {
		return 0, 0, err
	}

	return used, reserved, nil
}

// hasAvailable 校验在当日已用与在途基础上是否还能容纳 amount
func (s *WaterQuotaService) hasAvailable(zoneID uint, dailyQuota, amount float64, day time.Time) (bool, error) {
	used, reserved, err := s.sumUsedAndReserved(zoneID, day)
	if err != nil {
		return false, err
	}
	return used+reserved+amount <= dailyQuota, nil
}

// ReserveOutcome 预留结果
type ReserveOutcome int

const (
	// ReserveCreated 新建预留成功
	ReserveCreated ReserveOutcome = iota
	// ReserveExisting 幂等命中，复用已有预留，未重复扣减
	ReserveExisting
	// ReserveInsufficient 额度不足
	ReserveInsufficient
)

// TryReserve 尝试为区域预留当日水量。
// idempotencyKey 非空时启用幂等：同一键重复触发或重启后重试不会重复扣减。
// 区域未设置额度时不做限制，直接返回 ReserveCreated 且 reservation 为 nil。
func (s *WaterQuotaService) TryReserve(zoneID uint, scheduleID *uint, amount float64, idempotencyKey string, now time.Time) (*models.WaterReservation, ReserveOutcome, error) {
	quota, err := s.GetZoneQuota(zoneID)
	if err != nil {
		return nil, ReserveCreated, err
	}
	if quota == nil {
		return nil, ReserveCreated, nil
	}

	date := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	if idempotencyKey != "" {
		var existing models.WaterReservation
		err := database.DB.Where("idempotency_key = ?", idempotencyKey).First(&existing).Error
		if err == nil {
			if existing.Status != models.ReservationStatusReleased {
				return &existing, ReserveExisting, nil
			}
			// 已释放的预留（如执行失败）允许重新占用，净扣减不重复，但仍需校验当前额度
			ok, err := s.hasAvailable(zoneID, quota.DailyQuota, amount, now)
			if err != nil {
				return nil, ReserveCreated, err
			}
			if !ok {
				return nil, ReserveInsufficient, nil
			}
			updates := map[string]interface{}{
				"status":        models.ReservationStatusReserved,
				"reserved_date": date,
				"log_id":        nil,
				"amount":        amount,
			}
			if err := database.DB.Model(&existing).Updates(updates).Error; err != nil {
				return nil, ReserveCreated, err
			}
			existing.Status = models.ReservationStatusReserved
			existing.Amount = amount
			return &existing, ReserveCreated, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ReserveCreated, err
		}
	}

	ok, err := s.hasAvailable(zoneID, quota.DailyQuota, amount, now)
	if err != nil {
		return nil, ReserveCreated, err
	}
	if !ok {
		return nil, ReserveInsufficient, nil
	}

	reservation := &models.WaterReservation{
		ZoneID:       zoneID,
		ScheduleID:   scheduleID,
		Amount:       amount,
		ReservedDate: date,
		Status:       models.ReservationStatusReserved,
	}
	if idempotencyKey != "" {
		reservation.IdempotencyKey = &idempotencyKey
	}
	if err := database.DB.Create(reservation).Error; err != nil {
		return nil, ReserveCreated, err
	}
	return reservation, ReserveCreated, nil
}

// BindLog 将预留与灌溉执行记录关联
func (s *WaterQuotaService) BindLog(reservationID uint, logID uint) error {
	return database.DB.Model(&models.WaterReservation{}).
		Where("id = ?", reservationID).
		Update("log_id", logID).Error
}

// ConsumeReservationByLog 执行成功，预留转为已消费（已用部分由执行记录统计，口径不变）
func (s *WaterQuotaService) ConsumeReservationByLog(logID uint) error {
	return database.DB.Model(&models.WaterReservation{}).
		Where("log_id = ? AND status = ?", logID, models.ReservationStatusReserved).
		Update("status", models.ReservationStatusConsumed).Error
}

// ReleaseReservationByLog 执行失败，释放预留额度
func (s *WaterQuotaService) ReleaseReservationByLog(logID uint) error {
	return database.DB.Model(&models.WaterReservation{}).
		Where("log_id = ? AND status = ?", logID, models.ReservationStatusReserved).
		Update("status", models.ReservationStatusReleased).Error
}

// ReleaseReservationsBySchedule 计划停用或移除时，释放其在途预留额度
func (s *WaterQuotaService) ReleaseReservationsBySchedule(scheduleID uint) error {
	return database.DB.Model(&models.WaterReservation{}).
		Where("schedule_id = ? AND status = ?", scheduleID, models.ReservationStatusReserved).
		Update("status", models.ReservationStatusReleased).Error
}

// DeferSchedule 额度不足时登记计划顺延：记录所需水量与最早可执行时间。
// 同一计划同时只保留一条待补偿记录，重复顺延仅更新水量与时间。
func (s *WaterQuotaService) DeferSchedule(scheduleID uint, zoneID *uint, triggerType models.TriggerType, requiredWater float64, now time.Time) (*models.IrrigationDeferral, error) {
	// 额度按自然日重置，最早可执行时间为次日零点
	earliest := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())

	var deferral models.IrrigationDeferral
	err := database.DB.Where("schedule_id = ? AND status = ?", scheduleID, models.DeferralStatusPending).
		First(&deferral).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		deferral = models.IrrigationDeferral{
			ScheduleID:       scheduleID,
			ZoneID:           zoneID,
			TriggerType:      triggerType,
			RequiredWater:    requiredWater,
			EarliestExecTime: earliest,
			Status:           models.DeferralStatusPending,
		}
		if err := database.DB.Create(&deferral).Error; err != nil {
			return nil, err
		}
		return &deferral, nil
	}

	updates := map[string]interface{}{
		"required_water":     requiredWater,
		"earliest_exec_time": earliest,
	}
	if err := database.DB.Model(&deferral).Updates(updates).Error; err != nil {
		return nil, err
	}
	deferral.RequiredWater = requiredWater
	deferral.EarliestExecTime = earliest
	return &deferral, nil
}

// ListDeferrals 查询顺延记录，支持按计划与状态筛选
func (s *WaterQuotaService) ListDeferrals(scheduleID *uint, status *string) ([]models.IrrigationDeferral, error) {
	var deferrals []models.IrrigationDeferral
	query := database.DB

	if scheduleID != nil {
		query = query.Where("schedule_id = ?", *scheduleID)
	}
	if status != nil {
		query = query.Where("status = ?", *status)
	}

	if err := query.Order("created_at DESC").Find(&deferrals).Error; err != nil {
		return nil, err
	}
	return deferrals, nil
}

// DueDeferrals 查询到达最早可执行时间的待补偿顺延记录
func (s *WaterQuotaService) DueDeferrals(now time.Time) ([]models.IrrigationDeferral, error) {
	var deferrals []models.IrrigationDeferral
	err := database.DB.Where("status = ? AND earliest_exec_time <= ?", models.DeferralStatusPending, now).
		Find(&deferrals).Error
	return deferrals, err
}

// MarkDeferralCompensated 标记顺延已补偿，同一顺延记录只补偿一次
func (s *WaterQuotaService) MarkDeferralCompensated(id uint) error {
	now := time.Now()
	result := database.DB.Model(&models.IrrigationDeferral{}).
		Where("id = ? AND status = ?", id, models.DeferralStatusPending).
		Updates(map[string]interface{}{
			"status":         models.DeferralStatusCompensated,
			"compensated_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("deferral %d is not pending", id)
	}
	return nil
}

// CancelDeferralsBySchedule 计划停用或移除时，取消待补偿的顺延记录
func (s *WaterQuotaService) CancelDeferralsBySchedule(scheduleID uint) error {
	return database.DB.Model(&models.IrrigationDeferral{}).
		Where("schedule_id = ? AND status = ?", scheduleID, models.DeferralStatusPending).
		Update("status", models.DeferralStatusCancelled).Error
}

package services

import (
	"time"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

type IrrigationService struct {
	quotaService *WaterQuotaService
}

func NewIrrigationService() *IrrigationService {
	return &IrrigationService{
		quotaService: NewWaterQuotaService(),
	}
}

// StartResult 启动灌溉的结果：要么立即开始执行，要么因额度不足顺延
type StartResult struct {
	Log        *models.IrrigationLog      `json:"log,omitempty"`
	Deferral   *models.IrrigationDeferral `json:"deferral,omitempty"`
	Deferred   bool                       `json:"deferred"`
	Duplicated bool                       `json:"duplicated"`
}

// RequestStart 统一的灌溉启动入口，手动/定时/条件触发共用当日已用与在途额度。
// estimatedWater 为本次所需水量估算；idempotencyKey 非空时保证重复触发或重启不重复扣减。
// 额度不足时不生成进行中记录：有计划则登记顺延，无计划（手动）返回 ErrInsufficientQuota。
func (s *IrrigationService) RequestStart(scheduleID *uint, zoneID *uint, triggerType models.TriggerType, estimatedWater float64, idempotencyKey string) (*StartResult, error) {
	if zoneID == nil {
		log, err := s.StartIrrigation(scheduleID, zoneID, triggerType)
		if err != nil {
			return nil, err
		}
		return &StartResult{Log: log}, nil
	}

	now := time.Now()
	reservation, outcome, err := s.quotaService.TryReserve(*zoneID, scheduleID, estimatedWater, idempotencyKey, now)
	if err != nil {
		return nil, err
	}

	switch outcome {
	case ReserveExisting:
		// 幂等命中：本次触发已预留过，不重复扣减也不重复执行
		return &StartResult{Duplicated: true}, nil
	case ReserveInsufficient:
		if scheduleID == nil {
			return nil, ErrInsufficientQuota
		}
		deferral, err := s.quotaService.DeferSchedule(*scheduleID, zoneID, triggerType, estimatedWater, now)
		if err != nil {
			return nil, err
		}
		return &StartResult{Deferral: deferral, Deferred: true}, nil
	}

	log, err := s.StartIrrigation(scheduleID, zoneID, triggerType)
	if err != nil {
		if reservation != nil {
			// 执行记录创建失败，释放刚预留的额度
			_ = s.releaseReservationByID(reservation.ID)
		}
		return nil, err
	}

	if reservation != nil {
		if err := s.quotaService.BindLog(reservation.ID, log.ID); err != nil {
			return nil, err
		}
	}
	return &StartResult{Log: log}, nil
}

func (s *IrrigationService) releaseReservationByID(reservationID uint) error {
	return database.DB.Model(&models.WaterReservation{}).
		Where("id = ? AND status = ?", reservationID, models.ReservationStatusReserved).
		Update("status", models.ReservationStatusReleased).Error
}

func (s *IrrigationService) StartIrrigation(scheduleID *uint, zoneID *uint, triggerType models.TriggerType) (*models.IrrigationLog, error) {
	log := &models.IrrigationLog{
		ScheduleID:  scheduleID,
		ZoneID:      zoneID,
		TriggerType: triggerType,
		StartTime:   time.Now(),
		Status:      models.ExecutionStatusInProgress,
	}

	if err := database.DB.Create(log).Error; err != nil {
		return nil, err
	}

	return log, nil
}

func (s *IrrigationService) CompleteIrrigation(logID uint, success bool, waterUsage *float64, errorMsg *string) error {
	now := time.Now()
	updates := map[string]interface{}{
		"end_time": now,
	}

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

	err := database.DB.Model(&models.IrrigationLog{}).
		Where("id = ?", logID).
		Updates(updates).Error
	if err != nil {
		return err
	}

	// 成功：预留转为已消费；失败：释放预留额度
	if success {
		return s.quotaService.ConsumeReservationByLog(logID)
	}
	return s.quotaService.ReleaseReservationByLog(logID)
}

func (s *IrrigationService) GetIrrigationHistory(zoneID *uint, startTime, endTime time.Time, limit int) ([]models.IrrigationLog, error) {
	var logs []models.IrrigationLog
	query := database.DB

	if zoneID != nil {
		query = query.Where("zone_id = ?", *zoneID)
	}
	if !startTime.IsZero() {
		query = query.Where("start_time >= ?", startTime)
	}
	if !endTime.IsZero() {
		query = query.Where("start_time <= ?", endTime)
	}

	if limit > 0 {
		query = query.Limit(limit)
	}

	if err := query.Order("start_time DESC").Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

type WaterUsageStats struct {
	TotalUsage   float64 `json:"total_usage"`
	Duration     int64   `json:"duration"`
	IrrigationCount int64 `json:"irrigation_count"`
}

func (s *IrrigationService) GetWaterUsageStats(zoneID *uint, startTime, endTime time.Time) (*WaterUsageStats, error) {
	var stats WaterUsageStats
	query := database.DB.Model(&models.IrrigationLog{}).
		Select("COALESCE(SUM(water_usage), 0) as total_usage, COALESCE(COUNT(*), 0) as irrigation_count").
		Where("status = ?", models.ExecutionStatusSuccess)

	if zoneID != nil {
		query = query.Where("zone_id = ?", *zoneID)
	}
	if !startTime.IsZero() {
		query = query.Where("start_time >= ?", startTime)
	}
	if !endTime.IsZero() {
		query = query.Where("start_time <= ?", endTime)
	}

	err := query.Scan(&stats).Error
	return &stats, err
}

type ZoneWaterUsage struct {
	ZoneID     uint    `json:"zone_id"`
	ZoneName   string  `json:"zone_name"`
	WaterUsage float64 `json:"water_usage"`
	Percentage float64 `json:"percentage"`
}

func (s *IrrigationService) GetZoneWaterUsage(startTime, endTime time.Time) ([]ZoneWaterUsage, error) {
	var zoneUsages []ZoneWaterUsage

	query := `
		SELECT
			z.id as zone_id,
			z.name as zone_name,
			COALESCE(SUM(il.water_usage), 0) as water_usage
		FROM irrigation_zones z
		LEFT JOIN irrigation_logs il ON z.id = il.zone_id
			AND il.status = 'success'
			AND il.start_time >= ?
			AND il.start_time <= ?
		GROUP BY z.id, z.name
		ORDER BY water_usage DESC
	`

	err := database.DB.Raw(query, startTime, endTime).Scan(&zoneUsages).Error
	if err != nil {
		return nil, err
	}

	var total float64
	for _, zu := range zoneUsages {
		total += zu.WaterUsage
	}

	if total > 0 {
		for i := range zoneUsages {
			zoneUsages[i].Percentage = (zoneUsages[i].WaterUsage / total) * 100
		}
	}

	return zoneUsages, nil
}

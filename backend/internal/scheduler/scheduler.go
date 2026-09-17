package scheduler

import (
	"fmt"
	"time"

	"go.uber.org/zap"

	"irrigation/internal/models"
	"irrigation/internal/services"
	"irrigation/pkg/logger"
)

type IrrigationScheduler struct {
	scheduleService  *services.ScheduleService
	irrigationService *services.IrrigationService
	quotaService    *services.WaterQuotaService
	sensorService   *services.SensorService
	deviceService  *services.DeviceService
	alertService   *services.AlertService
}

func NewIrrigationScheduler() *IrrigationScheduler {
	return &IrrigationScheduler{
		scheduleService:  services.NewScheduleService(),
		irrigationService: services.NewIrrigationService(),
		quotaService:    services.NewWaterQuotaService(),
		sensorService:   services.NewSensorService(),
		deviceService:  services.NewDeviceService(),
		alertService:   services.NewAlertService(),
	}
}

func (s *IrrigationScheduler) Start() {
	logger.Info("Starting irrigation scheduler started")

	go s.runScheduleCheck()
	go s.runDeviceHealthCheck()
}

func (s *IrrigationScheduler) runScheduleCheck() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.checkAndExecuteSchedules()
		s.checkDeferredSchedules()
	}
}

func (s *IrrigationScheduler) checkAndExecuteSchedules() {
	schedules, err := s.scheduleService.ListActiveSchedules()
	if err != nil {
		logger.Error("Failed to get active schedules", zap.Error(err))
		return
	}

	for _, schedule := range schedules {
		s.executeScheduleIfNeeded(schedule)
	}
}

func (s *IrrigationScheduler) executeScheduleIfNeeded(schedule models.IrrigationSchedule) {
	now := time.Now()

	if schedule.Type == models.ScheduleTypeTimed {
		if shouldExecuteTimedSchedule(schedule, now) {
			go s.executeIrrigation(schedule)
		}
	} else if schedule.Type == models.ScheduleTypeConditional {
		if shouldExecuteConditionalSchedule(schedule) {
			go s.executeIrrigation(schedule)
		}
	}
}

func shouldExecuteTimedSchedule(schedule models.IrrigationSchedule, now time.Time) bool {
	if schedule.StartTime == "" {
		return false
	}

	nowTime := now.Format("15:04")
	if schedule.StartTime == nowTime {
		switch schedule.RepeatMode {
		case models.RepeatModeOnce:
			return true
		case models.RepeatModeDaily:
			return true
		case models.RepeatModeWeekly:
			weekday := int(now.Weekday())
			for _, d := range schedule.RepeatDays {
				if d == weekday {
					return true
				}
			}
		case models.RepeatModeMonthly:
			if len(schedule.RepeatDays) > 0 {
				day := now.Day()
				for _, d := range schedule.RepeatDays {
					if d == day {
						return true
					}
				}
			}
		}
	}
	return false
}

func shouldExecuteConditionalSchedule(schedule models.IrrigationSchedule) bool {
	if schedule.ZoneID == nil || schedule.HumidityThreshold == nil {
		return false
	}

	avgHumidity, err := services.NewSensorService().GetAverageHumidity(*schedule.ZoneID, 1*time.Hour)
	if err != nil || avgHumidity == nil {
		return false
	}

	return *avgHumidity < *schedule.HumidityThreshold
}

// triggerIdempotencyKey 为计划触发生成幂等键，保证重复触发或重启不会重复扣减额度
func triggerIdempotencyKey(schedule models.IrrigationSchedule, now time.Time) string {
	date := now.Format("2006-01-02")
	if schedule.Type == models.ScheduleTypeTimed {
		return fmt.Sprintf("timed:%d:%s:%s", schedule.ID, date, schedule.StartTime)
	}
	return fmt.Sprintf("cond:%d:%s", schedule.ID, date)
}

func (s *IrrigationScheduler) executeIrrigation(schedule models.IrrigationSchedule) {
	logger.Info("Executing irrigation schedule", zap.Uint("schedule_id", schedule.ID))

	if schedule.RainSensorID != nil {
		rainfall, err := s.sensorService.CheckRecentRainfall(*schedule.RainSensorID, 2*time.Hour)
		if err == nil && rainfall > 5.0 {
			logger.Info("Skipping irrigation due to recent rainfall", zap.Float64("rainfall", rainfall))
			return
		}
	}

	var triggerType models.TriggerType
	if schedule.Type == models.ScheduleTypeTimed {
		triggerType = models.TriggerTypeTimed
	} else {
		triggerType = models.TriggerTypeConditional
	}

	estimatedWater := services.EstimateWaterAmount(schedule.Duration)
	result, err := s.irrigationService.RequestStart(&schedule.ID, schedule.ZoneID, triggerType,
		estimatedWater, triggerIdempotencyKey(schedule, time.Now()))
	if err != nil {
		logger.Error("Failed to start irrigation", zap.Error(err))
		s.alertService.CreateIrrigationFailedAlert(schedule.ZoneID, "启动灌溉失败: "+err.Error())
		return
	}

	if result.Duplicated {
		logger.Info("Skipping duplicated schedule trigger", zap.Uint("schedule_id", schedule.ID))
		return
	}
	if result.Deferred {
		logger.Info("Irrigation deferred due to insufficient daily quota",
			zap.Uint("schedule_id", schedule.ID),
			zap.Float64("required_water", result.Deferral.RequiredWater),
			zap.Time("earliest_exec_time", result.Deferral.EarliestExecTime))
		return
	}

	s.runIrrigationLog(schedule, result.Log.ID)
}

// runIrrigationLog 执行灌溉并按结果完成记录（成功消费预留，失败释放预留）
func (s *IrrigationScheduler) runIrrigationLog(schedule models.IrrigationSchedule, logID uint) {
	duration := time.Duration(schedule.Duration) * time.Second
	if schedule.Duration > 0 {
		time.Sleep(duration)

		waterUsage := services.EstimateWaterAmount(schedule.Duration)
		s.irrigationService.CompleteIrrigation(logID, true, &waterUsage, nil)
		logger.Info("Irrigation completed", zap.Uint("log_id", logID))
	} else {
		s.irrigationService.CompleteIrrigation(logID, false, nil, nil)
	}
}

// checkDeferredSchedules 检查到期的顺延记录，额度恢复后对同一计划只补偿一次
func (s *IrrigationScheduler) checkDeferredSchedules() {
	deferrals, err := s.quotaService.DueDeferrals(time.Now())
	if err != nil {
		logger.Error("Failed to get due deferrals", zap.Error(err))
		return
	}

	for _, deferral := range deferrals {
		s.compensateDeferral(deferral)
	}
}

func (s *IrrigationScheduler) compensateDeferral(deferral models.IrrigationDeferral) {
	schedule, err := s.scheduleService.GetScheduleByID(deferral.ScheduleID)
	if err != nil {
		logger.Warn("Deferral schedule not found, cancelling", zap.Uint("deferral_id", deferral.ID))
		s.quotaService.CancelDeferralsBySchedule(deferral.ScheduleID)
		return
	}
	if schedule.Status != models.ScheduleStatusActive {
		// 计划已停用，顺延记录已在停用时取消；此处兜底
		s.quotaService.CancelDeferralsBySchedule(deferral.ScheduleID)
		return
	}

	// 以顺延记录 ID 作为幂等键，重启或重复检查不会对同一顺延重复扣减
	key := fmt.Sprintf("deferral:%d", deferral.ID)
	result, err := s.irrigationService.RequestStart(&schedule.ID, schedule.ZoneID, deferral.TriggerType,
		deferral.RequiredWater, key)
	if err != nil {
		logger.Error("Failed to compensate deferral", zap.Uint("deferral_id", deferral.ID), zap.Error(err))
		return
	}

	if result.Deferred {
		logger.Info("Quota still insufficient, deferral postponed",
			zap.Uint("deferral_id", deferral.ID),
			zap.Time("earliest_exec_time", result.Deferral.EarliestExecTime))
		return
	}

	if err := s.quotaService.MarkDeferralCompensated(deferral.ID); err != nil {
		logger.Error("Failed to mark deferral compensated", zap.Uint("deferral_id", deferral.ID), zap.Error(err))
		return
	}

	if result.Duplicated {
		// 已补偿过（如重启前已预留），仅补齐状态
		return
	}

	logger.Info("Deferral compensated, irrigation started",
		zap.Uint("deferral_id", deferral.ID),
		zap.Uint("schedule_id", schedule.ID))
	go s.runIrrigationLog(*schedule, result.Log.ID)
}

func (s *IrrigationScheduler) runDeviceHealthCheck() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.checkDeviceHealth()
	}
}

func (s *IrrigationScheduler) checkDeviceHealth() {
	timeout := 5 * time.Minute
	devices, err := s.deviceService.CheckOfflineDevices(timeout)
	if err != nil {
		logger.Error("Failed to check offline devices", zap.Error(err))
		return
	}

	for _, device := range devices {
		if device.Status == models.DeviceStatusOnline {
			s.deviceService.MarkDeviceOffline(device.ID)
			s.alertService.CreateDeviceOfflineAlert(device.ID, device.Name)
			logger.Warn("Device marked as offline", zap.Uint("device_id", device.ID), zap.String("device_name", device.Name))
		}
	}
}

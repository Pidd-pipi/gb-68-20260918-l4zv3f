package scheduler

import (
	"time"

	"go.uber.org/zap"

	"irrigation/internal/models"
	"irrigation/internal/services"
	"irrigation/pkg/logger"
)

type IrrigationScheduler struct {
	scheduleService   *services.ScheduleService
	irrigationService *services.IrrigationService
	quotaService      *services.QuotaService
	sensorService     *services.SensorService
	deviceService     *services.DeviceService
	alertService      *services.AlertService
}

func NewIrrigationScheduler() *IrrigationScheduler {
	return &IrrigationScheduler{
		scheduleService:   services.NewScheduleService(),
		irrigationService: services.NewIrrigationService(),
		quotaService:      services.NewQuotaService(),
		sensorService:     services.NewSensorService(),
		deviceService:     services.NewDeviceService(),
		alertService:      services.NewAlertService(),
	}
}

func (s *IrrigationScheduler) Start() {
	logger.Info("Starting irrigation scheduler started")

	if err := s.quotaService.RecoverInterruptedQuotas(); err != nil {
		logger.Error("Failed to recover interrupted water quotas", zap.Error(err))
	}

	go s.runScheduleCheck()
	go s.runPostponementCheck()
	go s.runDeviceHealthCheck()
}

func (s *IrrigationScheduler) runScheduleCheck() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.checkAndExecuteSchedules()
	}
}

func (s *IrrigationScheduler) runPostponementCheck() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.checkAndExecutePostponements()
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

func (s *IrrigationScheduler) checkAndExecutePostponements() {
	postponements, err := s.quotaService.ListDuePostponements(time.Now())
	if err != nil {
		logger.Error("Failed to list due schedule postponements", zap.Error(err))
		return
	}
	for _, postponement := range postponements {
		schedule, err := s.scheduleService.GetScheduleByID(postponement.ScheduleID)
		if err != nil || schedule == nil {
			continue
		}
		if schedule.Status != models.ScheduleStatusActive {
			continue
		}

		result, err := s.quotaService.ExecuteDuePostponement(postponement.ID)
		if err != nil {
			logger.Error("Failed to execute postponed irrigation",
				zap.Uint("postponement_id", postponement.ID), zap.Error(err))
			continue
		}
		if result == nil || !result.Started || result.Log == nil {
			continue
		}

		go s.runStartedIrrigation(*schedule, result.Log.ID, postponement.ID)
	}
}

func (s *IrrigationScheduler) executeScheduleIfNeeded(schedule models.IrrigationSchedule) {
	now := time.Now()

	if schedule.Type == models.ScheduleTypeTimed {
		if shouldExecuteTimedSchedule(schedule, now) {
			go s.executeIrrigation(schedule, models.TriggerTypeTimed)
		}
	} else if schedule.Type == models.ScheduleTypeConditional {
		if shouldExecuteConditionalSchedule(schedule) {
			go s.executeIrrigation(schedule, models.TriggerTypeConditional)
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

func requiredWater(schedule models.IrrigationSchedule) float64 {
	if schedule.Duration <= 0 {
		return 0
	}
	return float64(schedule.Duration) * 0.1
}

func (s *IrrigationScheduler) executeIrrigation(schedule models.IrrigationSchedule, triggerType models.TriggerType) {
	logger.Info("Executing irrigation schedule",
		zap.Uint("schedule_id", schedule.ID), zap.String("trigger", string(triggerType)))

	if schedule.RainSensorID != nil {
		rainfall, err := s.sensorService.CheckRecentRainfall(*schedule.RainSensorID, 2*time.Hour)
		if err == nil && rainfall > 5.0 {
			logger.Info("Skipping irrigation due to recent rainfall", zap.Float64("rainfall", rainfall))
			return
		}
	}

	result, err := s.quotaService.StartScheduledIrrigation(schedule, triggerType, requiredWater(schedule))
	if err != nil {
		logger.Error("Failed to reserve irrigation water", zap.Error(err))
		s.alertService.CreateIrrigationFailedAlert(schedule.ZoneID, "预留灌溉水量失败: "+err.Error())
		return
	}
	if result.Skipped {
		logger.Info("Skipped irrigation trigger because it was already handled or postponed",
			zap.Uint("schedule_id", schedule.ID))
		return
	}
	if result.Postponed {
		logger.Info("Irrigation postponed due to insufficient daily water quota",
			zap.Uint("schedule_id", schedule.ID),
			zap.Float64("required", result.Postponement.RequiredAmount),
			zap.Time("earliest_execute_at", result.Postponement.EarliestExecuteAt))
		return
	}

	s.runStartedIrrigation(schedule, result.Log.ID, 0)
}

func (s *IrrigationScheduler) runStartedIrrigation(schedule models.IrrigationSchedule, logID uint, postponementID uint) {
	if schedule.Duration <= 0 {
		errMsg := "灌溉时长无效"
		var completeErr error
		if postponementID > 0 {
			completeErr = s.quotaService.CompletePostponedIrrigation(logID, postponementID, false, nil, &errMsg)
		} else {
			completeErr = s.irrigationService.CompleteIrrigation(logID, false, nil, &errMsg)
		}
		if completeErr != nil {
			logger.Error("Failed to complete invalid irrigation", zap.Uint("log_id", logID), zap.Error(completeErr))
		}
		s.alertService.CreateIrrigationFailedAlert(schedule.ZoneID, errMsg)
		return
	}

	time.Sleep(time.Duration(schedule.Duration) * time.Second)

	waterUsage := float64(schedule.Duration) * 0.1
	var completeErr error
	if postponementID > 0 {
		completeErr = s.quotaService.CompletePostponedIrrigation(logID, postponementID, true, &waterUsage, nil)
	} else {
		completeErr = s.irrigationService.CompleteIrrigation(logID, true, &waterUsage, nil)
	}
	if completeErr != nil {
		logger.Error("Failed to complete irrigation", zap.Uint("log_id", logID), zap.Error(completeErr))
		s.alertService.CreateIrrigationFailedAlert(schedule.ZoneID, "完成灌溉失败: "+completeErr.Error())
		return
	}
	logger.Info("Irrigation completed", zap.Uint("log_id", logID))
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

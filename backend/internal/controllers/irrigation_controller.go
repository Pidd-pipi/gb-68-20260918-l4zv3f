package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"irrigation/internal/models"
	"irrigation/internal/services"
	"irrigation/pkg/response"
)

type IrrigationController struct {
	irrigationService *services.IrrigationService
	quotaService      *services.QuotaService
}

func NewIrrigationController() *IrrigationController {
	return &IrrigationController{
		irrigationService: services.NewIrrigationService(),
		quotaService:      services.NewQuotaService(),
	}
}

// ManualIrrigate godoc
// @Summary 手动灌溉
// @Description 触发手动灌溉，需要先通过区域日供水额度预留水量
// @Tags 灌溉执行
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param zone_id body int true "区域ID"
// @Param duration_seconds body int false "灌溉时长（秒），默认按每秒0.1单位水量计算所需水量"
// @Param water_amount body number false "指定所需水量，优先于 duration_seconds"
// @Success 200 {object} models.IrrigationLog
// @Failure 409 {object} object "区域日供水额度不足"
// @Router /api/irrigation/manual [post]
func (c *IrrigationController) ManualIrrigate(ctx *gin.Context) {
	var req struct {
		ZoneID          uint     `json:"zone_id" binding:"required"`
		DurationSeconds *int     `json:"duration_seconds"`
		WaterAmount     *float64 `json:"water_amount"`
	}

	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	required := 0.0
	if req.WaterAmount != nil {
		required = *req.WaterAmount
	}
	if req.DurationSeconds != nil && req.WaterAmount == nil {
		required = float64(*req.DurationSeconds) * 0.1
	}
	if required < 0 || (req.DurationSeconds != nil && *req.DurationSeconds < 0) {
		response.BadRequest(ctx, "water amount and duration cannot be negative")
		return
	}

	log, status, err := c.quotaService.StartManualIrrigation(req.ZoneID, required)
	if err != nil {
		var insufficient *services.InsufficientQuotaError
		if errors.As(err, &insufficient) {
			ctx.JSON(http.StatusConflict, response.Response{
				Code:    http.StatusConflict,
				Message: "区域日供水额度不足",
				Data:    insufficient,
			})
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}

	// Preserve the original zone-only behavior: start and return in-progress.
	// When an explicit amount is provided without a duration, finish it
	// immediately; a positive duration is simulated asynchronously.
	runInBackground := req.DurationSeconds != nil && *req.DurationSeconds > 0
	completeImmediately := req.WaterAmount != nil && !runInBackground

	if runInBackground {
		duration := *req.DurationSeconds
		go func() {
			time.Sleep(time.Duration(duration) * time.Second)
			_ = c.irrigationService.CompleteIrrigation(log.ID, true, &required, nil)
		}()
	} else if completeImmediately {
		waterUsage := required
		if err := c.irrigationService.CompleteIrrigation(log.ID, true, &waterUsage, nil); err != nil {
			response.InternalServerError(ctx, err.Error())
			return
		}
		log.Status = models.ExecutionStatusSuccess
		status, _ = c.quotaService.GetQuotaStatus(req.ZoneID, time.Now())
	}

	response.Success(ctx, gin.H{
		"log":          log,
		"quota_status": status,
	})
}

// GetIrrigationHistory godoc
// @Summary 获取灌溉历史
// @Description 获取灌溉执行历史记录
// @Tags 灌溉执行
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id query int false "区域ID"
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Param limit query int false "返回数量限制" default(100)
// @Success 200 {array} models.IrrigationLog
// @Router /api/irrigation/history [get]
func (c *IrrigationController) GetHistory(ctx *gin.Context) {
	var zoneID *uint
	if zoneIDStr := ctx.Query("zone_id"); zoneIDStr != "" {
		id, _ := strconv.ParseUint(zoneIDStr, 10, 32)
		idUint := uint(id)
		zoneID = &idUint
	}

	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	}

	limit := 100
	if limitStr := ctx.Query("limit"); limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}

	logs, err := c.irrigationService.GetIrrigationHistory(zoneID, startTime, endTime, limit)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, logs)
}

// GetWaterUsageStats godoc
// @Summary 获取用水量统计
// @Description 获取指定时间段的用水量统计
// @Tags 用水统计
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id query int false "区域ID"
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Success 200 {object} services.WaterUsageStats
// @Router /api/statistics/water-usage [get]
func (c *IrrigationController) GetWaterUsageStats(ctx *gin.Context) {
	var zoneID *uint
	if zoneIDStr := ctx.Query("zone_id"); zoneIDStr != "" {
		id, _ := strconv.ParseUint(zoneIDStr, 10, 32)
		idUint := uint(id)
		zoneID = &idUint
	}

	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	}

	stats, err := c.irrigationService.GetWaterUsageStats(zoneID, startTime, endTime)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, stats)
}

// GetZoneWaterUsage godoc
// @Summary 获取各区域用水量
// @Description 获取各区域的用水量分布
// @Tags 用水统计
// @Security ApiKeyAuth
// @Produce json
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Success 200 {array} services.ZoneWaterUsage
// @Router /api/statistics/zone-usage [get]
func (c *IrrigationController) GetZoneWaterUsage(ctx *gin.Context) {
	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	} else {
		startTime = time.Now().AddDate(0, 0, -7)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	} else {
		endTime = time.Now()
	}

	usage, err := c.irrigationService.GetZoneWaterUsage(startTime, endTime)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, usage)
}

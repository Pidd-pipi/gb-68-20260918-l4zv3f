package controllers

import (
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"irrigation/internal/services"
	"irrigation/pkg/response"
)

type WaterQuotaController struct {
	quotaService *services.QuotaService
}

func NewWaterQuotaController() *WaterQuotaController {
	return &WaterQuotaController{quotaService: services.NewQuotaService()}
}

// GetZoneQuota godoc
// @Summary 查询区域日供水额度
// @Description 查询区域当日额度、已用、在途预留和可用水量
// @Tags 供水额度
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id path int true "区域ID"
// @Success 200 {object} services.QuotaStatus
// @Router /api/zones/{zone_id}/water-quota [get]
func (c *WaterQuotaController) GetZoneQuota(ctx *gin.Context) {
	zoneID, err := strconv.ParseUint(ctx.Param("zone_id"), 10, 32)
	if err != nil {
		response.BadRequest(ctx, "invalid zone id")
		return
	}
	status, err := c.quotaService.GetQuotaStatus(uint(zoneID), time.Now())
	if err != nil {
		response.NotFound(ctx, "zone not found")
		return
	}
	response.Success(ctx, status)
}

// SetZoneQuota godoc
// @Summary 设置区域日供水额度
// @Description 设置区域每日供水额度；传 null 表示不限额
// @Tags 供水额度
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param zone_id path int true "区域ID"
// @Param daily_water_quota body number false "每日供水额度，null 表示不限额"
// @Success 200 {object} response.Response
// @Router /api/zones/{zone_id}/water-quota [put]
func (c *WaterQuotaController) SetZoneQuota(ctx *gin.Context) {
	zoneID, err := strconv.ParseUint(ctx.Param("zone_id"), 10, 32)
	if err != nil {
		response.BadRequest(ctx, "invalid zone id")
		return
	}

	var req struct {
		DailyWaterQuota *float64 `json:"daily_water_quota"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	if err := c.quotaService.SetDailyQuota(uint(zoneID), req.DailyWaterQuota); err != nil {
		if errors.Is(err, services.ErrInvalidWaterQuota) {
			response.BadRequest(ctx, err.Error())
			return
		}
		response.NotFound(ctx, err.Error())
		return
	}
	status, err := c.quotaService.GetQuotaStatus(uint(zoneID), time.Now())
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}
	response.Success(ctx, status)
}

// ListPostponements godoc
// @Summary 查询灌溉计划顺延记录
// @Description 查询因区域日供水额度不足产生的自动计划顺延记录
// @Tags 供水额度
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id query int false "区域ID"
// @Param schedule_id query int false "计划ID"
// @Param status query string false "状态: pending/executing/completed/failed/canceled"
// @Param start_time query string false "开始日期或RFC3339时间"
// @Param end_time query string false "结束日期或RFC3339时间"
// @Param limit query int false "返回数量限制" default(100)
// @Success 200 {array} models.SchedulePostponement
// @Router /api/irrigation/postponements [get]
func (c *WaterQuotaController) ListPostponements(ctx *gin.Context) {
	var zoneID, scheduleID *uint
	if zoneIDStr := ctx.Query("zone_id"); zoneIDStr != "" {
		id, _ := strconv.ParseUint(zoneIDStr, 10, 32)
		value := uint(id)
		zoneID = &value
	}
	if scheduleIDStr := ctx.Query("schedule_id"); scheduleIDStr != "" {
		id, _ := strconv.ParseUint(scheduleIDStr, 10, 32)
		value := uint(id)
		scheduleID = &value
	}

	var status *string
	if value := ctx.Query("status"); value != "" {
		status = &value
	}

	var startTime, endTime time.Time
	if value := ctx.Query("start_time"); value != "" {
		startTime, _ = time.Parse(time.RFC3339, value)
	}
	if value := ctx.Query("end_time"); value != "" {
		endTime, _ = time.Parse(time.RFC3339, value)
	}
	limit, _ := strconv.Atoi(ctx.DefaultQuery("limit", "100"))

	postponements, err := c.quotaService.ListPostponements(zoneID, scheduleID, status, startTime, endTime, limit)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}
	response.Success(ctx, postponements)
}

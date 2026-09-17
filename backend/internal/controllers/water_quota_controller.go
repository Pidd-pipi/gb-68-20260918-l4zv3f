package controllers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"irrigation/internal/services"
	"irrigation/pkg/response"
)

type WaterQuotaController struct {
	quotaService *services.WaterQuotaService
}

func NewWaterQuotaController() *WaterQuotaController {
	return &WaterQuotaController{
		quotaService: services.NewWaterQuotaService(),
	}
}

// SetZoneQuota godoc
// @Summary 设置区域日供水额度
// @Description 设置或更新指定区域的每日供水额度
// @Tags 供水额度
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path int true "区域ID"
// @Param request body object true "额度信息" example({"daily_quota": 100.5})
// @Success 200 {object} models.ZoneWaterQuota
// @Router /api/zones/{id}/quota [put]
func (c *WaterQuotaController) SetZoneQuota(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	var req struct {
		DailyQuota float64 `json:"daily_quota" binding:"required"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	quota, err := c.quotaService.SetZoneQuota(uint(id), req.DailyQuota)
	if err != nil {
		response.BadRequest(ctx, err.Error())
		return
	}

	response.Success(ctx, quota)
}

// GetZoneQuota godoc
// @Summary 查询区域日供水额度
// @Description 查询指定区域的日供水额度及当日已用、在途、可用水量
// @Tags 供水额度
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "区域ID"
// @Success 200 {object} services.QuotaUsage
// @Router /api/zones/{id}/quota [get]
func (c *WaterQuotaController) GetZoneQuota(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	usage, err := c.quotaService.GetQuotaUsage(uint(id), time.Now())
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, usage)
}

// ListDeferrals godoc
// @Summary 查询灌溉顺延记录
// @Description 查询因额度不足顺延的灌溉计划记录，支持按计划与状态筛选
// @Tags 供水额度
// @Security ApiKeyAuth
// @Produce json
// @Param schedule_id query int false "计划ID"
// @Param status query string false "顺延状态 (pending/compensated/cancelled)"
// @Success 200 {array} models.IrrigationDeferral
// @Router /api/irrigation/deferrals [get]
func (c *WaterQuotaController) ListDeferrals(ctx *gin.Context) {
	var scheduleID *uint
	if idStr := ctx.Query("schedule_id"); idStr != "" {
		id, _ := strconv.ParseUint(idStr, 10, 32)
		idUint := uint(id)
		scheduleID = &idUint
	}

	var status *string
	if s := ctx.Query("status"); s != "" {
		status = &s
	}

	deferrals, err := c.quotaService.ListDeferrals(scheduleID, status)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, deferrals)
}

package service

import (
	"context"
	"time"
)

// DeliveryWindow describes only intervals for which the gateway published measurements.
type DeliveryWindow struct {
	Seconds          int64          `json:"seconds"`
	From             *time.Time     `json:"from"`
	To               *time.Time     `json:"to"`
	Unit             string         `json:"unit"`
	Output           int64          `json:"output"`
	TokenCoverage    *float64       `json:"token_coverage"`
	Requests         int64          `json:"requests"`
	QPS              OpsRateSummary `json:"qps"`
	Rate             OpsRateSummary `json:"rate"`
	CurrentAvailable bool           `json:"current_available"`
	Complete         int64          `json:"completed"`
	Failed           int64          `json:"failed"`
	Limited          int64          `json:"limited"`
	Cancelled        int64          `json:"cancelled"`
	Attempts         int64          `json:"attempts"`
	AttemptErrors    int64          `json:"attempt_errors"`
	AttemptLimited   int64          `json:"limited_attempts"`
	TTFT             OpsPercentiles `json:"ttft"`
	Duration         OpsPercentiles `json:"duration"`
	Speed            OpsOutputTPS   `json:"speed"`
	SpeedUnit        string         `json:"speed_unit"`
	Missing          bool           `json:"missing"`
}
type OpsDeliveryReader interface {
	DeliveryWindow(context.Context, *OpsDashboardFilter, bool) (*DeliveryWindow, error)
}

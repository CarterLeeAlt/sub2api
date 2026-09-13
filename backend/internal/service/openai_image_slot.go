package service

import (
	"github.com/gin-gonic/gin"
)

const (
	openAIImageSlotAcquirerContextKey = "openai_image_slot_acquirer"
	openAIImageSlotReleaseContextKey  = "openai_image_slot_release"
)

// OpenAIImageSlotAcquirer 由 handler 注入，供 Forward 在 imageIntent 事后升级时
// 补偿占用生图并发槽：handler 只能按客户端显式声明预判生图意图，而 Codex 桥接
// 的 image_generation 工具注入发生在 service 层，若不补偿占槽，该路径会绕过
// ImageConcurrency 治理。实现方负责在占槽失败时写出 429 响应。
type OpenAIImageSlotAcquirer interface {
	AcquireForForward(c *gin.Context) (release func(), acquired bool)
}

// SetOpenAIImageSlotAcquirer 在 handler 侧注入补偿占槽器。handler 已按显式意图
// 占槽时不应注入，否则同一请求会重复计数。
func SetOpenAIImageSlotAcquirer(c *gin.Context, acquirer OpenAIImageSlotAcquirer) {
	if c == nil || acquirer == nil {
		return
	}
	c.Set(openAIImageSlotAcquirerContextKey, acquirer)
}

func openAIImageSlotAcquirerFrom(c *gin.Context) OpenAIImageSlotAcquirer {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(openAIImageSlotAcquirerContextKey); ok {
		if acquirer, ok := v.(OpenAIImageSlotAcquirer); ok {
			return acquirer
		}
	}
	return nil
}

// acquireOpenAIImageSlotForForward 在 Forward 内补偿占用生图并发槽；release 挂到
// gin context，由 handler 出口统一释放。已占用（含 failover 前序 attempt 占槽）
// 或未注入 acquirer 时为 no-op。
func acquireOpenAIImageSlotForForward(c *gin.Context) bool {
	if c == nil {
		return true
	}
	if _, ok := c.Get(openAIImageSlotReleaseContextKey); ok {
		return true
	}
	acquirer := openAIImageSlotAcquirerFrom(c)
	if acquirer == nil {
		return true
	}
	release, acquired := acquirer.AcquireForForward(c)
	if !acquired {
		return false
	}
	if release != nil {
		c.Set(openAIImageSlotReleaseContextKey, release)
	}
	return true
}

// ReleaseOpenAIImageSlotForForward 释放 Forward 内补偿占用的生图并发槽，供
// handler 出口 defer 调用；未补偿占槽时为 no-op。
func ReleaseOpenAIImageSlotForForward(c *gin.Context) {
	if c == nil {
		return
	}
	if v, ok := c.Get(openAIImageSlotReleaseContextKey); ok {
		if release, ok := v.(func()); ok && release != nil {
			release()
		}
	}
}

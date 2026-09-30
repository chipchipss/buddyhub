package zai

import (
	"log"
	"net"
	"net/http"
	"time"
)

// 上游端点（协议事实，来源：zcode2api app/constants.py + 实测）。
// 用 var 而非 const：测试注入 mock 上游需要替换（SetEndpoints）。
var (
	// PlanOrigin Coding Plan（JWT）通道的 origin —— 注意**不是** api.z.ai。
	PlanOrigin = "https://zcode.z.ai"
	// PlanMessagesURL JWT 通道的 messages 端点（需验证码头）。
	PlanMessagesURL = PlanOrigin + "/api/v1/zcode-plan/anthropic/v1/messages"
	// FallbackOrigin API Key 回退通道。
	FallbackOrigin = "https://api.z.ai"
	// FallbackMessagesURL API Key 通道（免验证码）。
	FallbackMessagesURL = FallbackOrigin + "/api/anthropic/v1/messages"
	// BigModelMessagesURL 智谱开放平台（Anthropic 兼容同形端点）。
	BigModelMessagesURL = "https://open.bigmodel.cn/api/anthropic/v1/messages"

	// BillingBase 额度查询族（billing/current|balance、usage）。
	BillingBase = PlanOrigin + "/api/v1/zcode-plan"
	// OAuthInitPath / OAuthPollPath OAuth CLI 免密登录。
	OAuthInitPath = "/api/v1/oauth/cli/init"
	OAuthPollPath = "/api/v1/oauth/cli/poll"

	// ClientAppVersion 官方桌面端现行版本（UA / X-ZCode-App-Version 取值）。
	ClientAppVersion = "3.11.2"
	// AnthropicVersion Anthropic 协议版本头。
	AnthropicVersion = "2023-06-01"
	// Referer 上游要求携带的 Referer。
	Referer = "https://zcode.z.ai/"
)

var zaiClient = &http.Client{
	Timeout: 0, // 流式请求不设总超时，靠 ctx / 传输层超时控制
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	},
}

func zaiHTTP() *http.Client { return zaiClient }

// SetHTTPClient 允许宿主替换（测试注入 mock 上游）。
func SetHTTPClient(c *http.Client) {
	if c != nil {
		zaiClient = c
	}
}

func logf(format string, args ...any) { log.Printf(format, args...) }

// SetEndpoints 替换上游端点（测试注入 mock 上游用）；空串表示保持不变。
func SetEndpoints(planMessages, fallbackMessages, bigModelMessages, billingBase string) {
	if planMessages != "" {
		PlanMessagesURL = planMessages
	}
	if fallbackMessages != "" {
		FallbackMessagesURL = fallbackMessages
	}
	if bigModelMessages != "" {
		BigModelMessagesURL = bigModelMessages
	}
	if billingBase != "" {
		BillingBase = billingBase
	}
}

// SetOrigins 替换 OAuth / 兑换链的 origin（测试注入 mock 上游用）。
func SetOrigins(planOrigin, fallbackOrigin string) {
	if planOrigin != "" {
		PlanOrigin = planOrigin
	}
	if fallbackOrigin != "" {
		FallbackOrigin = fallbackOrigin
	}
}

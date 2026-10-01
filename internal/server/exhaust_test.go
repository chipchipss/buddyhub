package server

import (
	"net/http"
	"testing"
)

// isExhausted 的表直接对**桥接真实会产生的文案**（11 条桥接的三种拼法）。
//
// 关键区分：**只有「这个平台现在用不了」才该换平台**。5xx / 超时 / 模型不存在
// 换过去大概率还是一样的错，还可能拿到语义不同的模型——不换是正确行为。
func TestIsExhausted(t *testing.T) {
	yes := []string{
		// 腾讯语义：402 恒为余额耗尽
		"上游 HTTP 402：{\"error\":\"insufficient credits\"}",
		// 429（用户口径里的额度类错误；当前平台被限流时换一家正是解法）
		"上游 HTTP 429：{\"error\":\"rate limit\"}",
		"上游返回 429: {\"msg\":\"too many requests\"}", // raccoon.UpstreamError.Error
		"HTTP 429: {\"error\":\"quota exceeded\"}",  // qoder / loomy
		// 非 429 的耗尽关键词（zai/classify.go 同表）
		"上游 HTTP 400：{\"msg\":\"余额不足\"}",
		"上游 HTTP 403：{\"code\":60001,\"msg\":\"quota exceeded\"}",
		"上游 HTTP 400：{\"msg\":\"额度用尽，明日重置\"}",
		// 6004：腾讯的「该模型在此账号被限流」，换一个平台正好解决
		`上游 HTTP 400：{"code":6004,"msg":"该模型在本账号限流"}`,
	}
	for _, m := range yes {
		if !isExhausted(m) {
			t.Errorf("应判为耗尽（该换平台）: %s", m)
		}
	}

	no := []string{
		"上游 HTTP 500：Internal Server Error",
		"上游 HTTP 404：未找到资源",
		"上游 HTTP 400：请求参数错误",             // 参数错换平台没用
		"请求体解析失败：unexpected end of JSON", // 客户端错误
		// traework：既无状态码也无 body → 取不出 → 保守不降级
		"发送消息失败：连接被重置",
		"建会话失败：context deadline exceeded",
		// 桥接自身的本地错误
		"凭据解析失败",
		"账号缺少 access_token",
		"",
	}
	for _, m := range no {
		if isExhausted(m) {
			t.Errorf("不该降级（换过去没用）: %s", m)
		}
	}
}

// 三种文案形状都要能取出状态码——这是"从 lastErr 还原上游状态"的前提。
func TestIsExhaustedRecognizesAllBridgeWording(t *testing.T) {
	// 7 条桥接：`上游 HTTP N：…`
	if !statusRe.MatchString("上游 HTTP 402：x") {
		t.Error("认不出 `上游 HTTP N：`")
	}
	// raccoon：`上游返回 N: …`
	if !statusRe.MatchString("上游返回 402: x") {
		t.Error("认不出 `上游返回 N:`")
	}
	// qoder / loomy：`HTTP N: …`
	if !statusRe.MatchString("HTTP 402: x") {
		t.Error("认不出 `HTTP N:`")
	}
	// traework：中文包装，取不到（这正是它不参与降级的原因）
	if statusRe.MatchString("发送消息失败：连接被重置") {
		t.Error("不该在没有状态码的文案里硬抠出状态")
	}
}

// 腾讯池轮转到底时的判据。**`no_healthy_account` 必须返回 false** ——
// 它是本地调度状态（号全在冷却/禁用），不是上游的额度信号；放行它会：
//  1. 违背"只在额度耗尽时降级"的口径，把配置问题换个平台掩盖掉
//  2. 顺带触发一次模型目录索引构建（10 个平台的目录，冷缓存单个 15–20s），
//     在"请求已经失败"的那一刻再等这么久是最糟的时机
func TestExhaustedForTerminal(t *testing.T) {
	yes := []struct {
		status int
		code   string
		msg    string
		why    string
	}{
		{429, "rate_limit_exceeded", "rate limited: all accounts are cooling down", "上游明确限流"},
		{http.StatusTooManyRequests, "", "", "429 作为状态码传入"},
		{http.StatusPaymentRequired, "", "", "402 作为状态码传入"},
		{503, "", "上游返回 402：{\"error\":\"insufficient credits\"}", "上游原文里有耗尽信号"},
	}
	for _, c := range yes {
		if !exhaustedForTerminal(c.status, c.code, c.msg) {
			t.Errorf("该降级（%s）: status=%d code=%q", c.why, c.status, c.code)
		}
	}

	no := []struct {
		status int
		code   string
		msg    string
		why    string
	}{
		{503, "no_healthy_account", "all accounts are temporarily unavailable", "本地调度状态，非额度信号"},
		{503, "", "内部错误", "取不出状态码"},
		{500, "", "", "上游 5xx 换平台没用"},
	}
	for _, c := range no {
		if exhaustedForTerminal(c.status, c.code, c.msg) {
			t.Errorf("不该降级（%s）: status=%d code=%q", c.why, c.status, c.code)
		}
	}
}

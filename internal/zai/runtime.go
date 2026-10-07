package zai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client 把「选号 → 取验证码 → 构造请求 → 分类失败 → 更新账号状态」串成一次调用。
//
// 重试阶梯（对齐 zcode2api 的实测结论，逐条落在状态机上）：
//
//	验证码挑战(3007)  换一枚 token 原地重试同一账号，最多 captchaRetries 次；
//	                  期间清空预解池（那批 token 已被风控盯上）
//	429               按 Retry-After 原地等待重试（等待期间不占账号），
//	                  耗尽后换号——**账号不冷却**（限流不是账号的错）
//	5xx               重试 serverRetries 次后冷却该账号，换号
//	额度用完(402)     标记 EXHAUSTED + 再探间隔，换号
//	401/403(非验证码) 标记 INVALID，换号
//	3012/405 真风控   标记 DISABLED（保护资产，须人工恢复），换号
type Client struct {
	Pool    *Pool
	Captcha *CaptchaManager

	// SystemBlocks Plan 通道要前置的身份块（空则不加；上游可能因此拒为 3012）
	SystemBlocks []SystemBlock

	MaxAttempts     int           // 总尝试次数上限（跨账号）
	CaptchaRetries  int           // 单账号验证码重试上限
	ServerRetries   int           // 5xx 重试上限
	RateLimitWait   time.Duration // 429 原地等待上限（超过就换号）
	CoolDuration    time.Duration // 5xx 耗尽后的冷却时长
	ExhaustProbeGap time.Duration // 额度用完后的再探间隔

	// 逐模型惩罚（B 计划）：某模型上游抖动只冷却该(账号,模型)，不动整号。
	ModelCoolBase     time.Duration // 阶梯基值（第 1 次惩罚时长）
	ModelCoolMax      time.Duration // 阶梯上限（封顶，防雪崩）
	TransientRetryGap time.Duration // 5xx 瞬时重试的原地小睡基值
}

// NewClient 建默认编排器。
func NewClient(pool *Pool, captcha *CaptchaManager, blocks []SystemBlock) *Client {
	return &Client{
		Pool: pool, Captcha: captcha, SystemBlocks: blocks,
		MaxAttempts:       6,
		CaptchaRetries:    3,
		ServerRetries:     3,
		RateLimitWait:     20 * time.Second,
		CoolDuration:      300 * time.Second,
		ExhaustProbeGap:   10 * time.Minute,
		ModelCoolBase:     30 * time.Second,
		ModelCoolMax:      5 * time.Minute,
		TransientRetryGap: 500 * time.Millisecond,
	}
}

// Result 一次成功建流的结果。
type Result struct {
	Resp     *http.Response
	Account  *Account
	UsedPlan bool
}

// Do 执行一次上游调用，返回**已建流成功**的响应（调用方负责读流并 Close）。
// 全部账号/尝试耗尽时返回错误。
func (c *Client) Do(ctx context.Context, body []byte) (*Result, error) {
	if c.Pool == nil {
		return nil, ErrNoAccount
	}
	attempts := c.MaxAttempts
	if attempts <= 0 {
		attempts = 6
	}
	// 惩罚按「模型代码」记（Anthropic 体里的 model，即真正发给上游的那个），一次上游
	// 抖动只影响该模型，不牵连同账号其他模型。
	model := modelFromBody(body)

	var lastErr error
	// 同一账号的验证码重试计数（换号即清零）
	var curAccount *Account
	captchaTries := 0
	serverTries := 0
	rateWaits := 0

	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Plan 通道可用 = 有验证码求解器 **且** 已配置身份块（system_file）。缺任一者时，
		// JWT 走 Plan 不是被上游判 3012（缺身份块）就是根本无法建流（缺求解器）——这是
		// 本地配置的结构性缺失，不是账号故障。故此时让「只能走 Plan」的号（无回退 Key）
		// 让位，把请求交给真正可用的通道（自带回退 Key 的 JWT、或独立的 API Key 号），
		// 而不是空转、或触发团灭守卫把整池挡在门外。
		planUsable := c.Captcha != nil && c.Captcha.Enabled() && len(c.SystemBlocks) > 0
		wantPlan := planUsable
		// usable：本请求此刻能真正建流的账号。纯 API Key 号恒可用；带回退 Key 的 JWT
		// 号可用（走回退）；只能走 Plan 的 JWT 号仅在 Plan 结构性可用时才算数。再叠加
		// 逐模型惩罚过滤——该模型正在冷却的号跳过，但同号其他模型不受牵连。
		usable := func(a *Account) bool {
			var chanOK bool
			if a.Mode == ModeAPIKey || a.HasKeyFallback() {
				chanOK = true
			} else {
				chanOK = planUsable && a.HasJWTPath()
			}
			return chanOK && !a.ModelPenalized(model, time.Now())
		}
		acc, err := c.Pool.Pick(wantPlan, usable)
		if err != nil {
			// 区分「本模型在可用号上都冷却了」与「根本没有能用该通道的号」——前者是
			// 上游抖动的临时态（会自愈），给可操作的稍后重试提示；后者才谈配置/凭证。
			if c.modelCooledOut(model, planUsable, time.Now()) {
				if lastErr != nil {
					return nil, fmt.Errorf("模型 %s 暂时不可用（上游抖动，各可用号已在短冷却中，稍后自动恢复）；最近失败：%v", model, lastErr)
				}
				return nil, fmt.Errorf("模型 %s 暂时不可用（上游抖动，各可用号已在短冷却中，稍后自动恢复）", model)
			}
			if lastErr != nil {
				return nil, fmt.Errorf("%w（最后一次失败：%v）", err, lastErr)
			}
			if !planUsable {
				return nil, fmt.Errorf("没有可用的 Z.AI 账号：池内账号只能走 Plan 通道，但 Plan 未就绪（缺验证码求解器或 system_file 身份块）；配好其一，或添加 API Key 账号")
			}
			return nil, err
		}
		if curAccount == nil || curAccount.ID != acc.ID {
			curAccount = acc
			captchaTries, serverTries, rateWaits = 0, 0, 0
		}

		forceFallback := false
		verifyParam, verifyRegion := "", ""
		if acc.HasJWTPath() {
			if !planUsable {
				// Plan 结构性不可用：能回退就回退，纯 Plan 号直接跳过（绝不改账号状态）
				if acc.HasKeyFallback() {
					forceFallback = true
				} else {
					c.Pool.Release(acc.ID)
					lastErr = fmt.Errorf("账号 %s 只能走 Plan 通道，但 Plan 不可用（缺验证码求解器或 system_file 身份块）", acc.Name)
					continue
				}
			} else {
				param, region, cerr := c.Captcha.Get(ctx)
				if cerr != nil {
					// 求解失败：有回退 Key 就走回退，否则跳过该号
					if acc.HasKeyFallback() {
						forceFallback = true
					} else {
						c.Pool.Release(acc.ID)
						lastErr = fmt.Errorf("验证码求解失败：%w", cerr)
						continue
					}
				} else {
					verifyParam, verifyRegion = param, region
				}
			}
		}

		url, headers, outBody := BuildRequest(acc, body, verifyParam, verifyRegion, forceFallback, c.SystemBlocks)
		usedPlan := !forceFallback && acc.HasJWTPath()
		acc.MarkUsed()

		resp, err := postMessages(ctx, url, headers, outBody)
		if err != nil {
			// 传输层错误（超时/断连）：当作上游错误处理，换号重试
			c.Pool.Release(acc.ID)
			_ = c.Pool.Store().Update(acc.ID, func(a *Account) { a.MarkFail(err.Error()) })
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusOK {
			c.Pool.Release(acc.ID)
			_ = c.Pool.Store().Update(acc.ID, func(a *Account) {
				a.MarkOK()
				a.ClearModelPenalty(model)
			})
			return &Result{Resp: resp, Account: acc, UsedPlan: usedPlan}, nil
		}

		// 失败：读一小段体判因（不能全读——流式错误体很小，但保险起见限长）
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, raw, resp.Header)
		retryAfter := RetryAfter(resp.Header, int(c.RateLimitWait.Seconds()))
		c.Pool.Release(acc.ID)

		switch kind {
		case FailCaptcha:
			if c.Captcha != nil {
				c.Captcha.Invalidate() // 那批 token 已不可信
			}
			captchaTries++
			if captchaTries > c.CaptchaRetries {
				lastErr = fmt.Errorf("验证码连续 %d 次被拒（账号 %s）", captchaTries, acc.Name)
				_ = c.Pool.Store().Update(acc.ID, func(a *Account) { a.MarkFail(lastErr.Error()) })
				continue
			}
			lastErr = fmt.Errorf("验证码挑战，重试中")
			attempt-- // 验证码重试不占总次数预算（同一账号原地重试）
			continue

		case FailRateLimited:
			rateWaits++
			if rateWaits > 2 || time.Duration(retryAfter)*time.Second > c.RateLimitWait {
				// 反复限流或等待过久：只给该(账号,模型)一个短惩罚（尊重 Retry-After，
				// 缺省用阶梯基值），账号保持可选——其他模型仍可服务。
				gap := c.modelPenaltyGap(rateWaits)
				if retryAfter > 0 && time.Duration(retryAfter)*time.Second < c.ModelCoolMax {
					gap = time.Duration(retryAfter) * time.Second
				}
				_ = c.Pool.Store().Update(acc.ID, func(a *Account) {
					a.PenalizeModel(model, gap, PenaltyRate, "上游限流 429")
				})
				lastErr = fmt.Errorf("上游限流（%s，模型 %s 短冷却 %s）", acc.Name, model, gap)
				continue
			}
			wait := time.Duration(retryAfter) * time.Second
			logf("zai: 上游 429，等待 %s 后重试账号 %s", wait, acc.Name)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			attempt-- // 等待重试不占预算
			continue

		case FailExhausted:
			// 对话实测该模型额度用完：按(账号,模型)记惩罚，不动账号级 Status——
			// 同号其他（更便宜/免费）模型仍可服务。账号级 EXHAUSTED 由额度轮询器管。
			_ = c.Pool.Store().Update(acc.ID, func(a *Account) {
				a.PenalizeModel(model, c.ExhaustProbeGap, PenaltyExhausted, "该模型额度用完")
				a.MarkFail("额度用完（模型 " + model + "）")
			})
			lastErr = fmt.Errorf("账号 %s 的模型 %s 额度用完", acc.Name, model)
			continue

		case FailRiskControl:
			_ = c.Pool.Store().Update(acc.ID, func(a *Account) {
				a.BanForRisk("上游风控（3012/405 unusual activity）")
			})
			logf("zai: 账号 %s 命中上游风控，已禁用保护（需人工确认恢复）", acc.Name)
			lastErr = fmt.Errorf("账号 %s 命中上游风控", acc.Name)
			continue

		case FailInvalid:
			_ = c.Pool.Store().Update(acc.ID, func(a *Account) {
				a.Invalidate(fmt.Sprintf("HTTP %d：%s", resp.StatusCode, truncate(string(raw), 160)))
			})
			lastErr = fmt.Errorf("账号 %s 凭证失效（HTTP %d）", acc.Name, resp.StatusCode)
			continue

		case FailServerError:
			serverTries++
			if serverTries >= c.ServerRetries {
				// 原地重试仍未成功：只给该(账号,模型)一个按阶梯的短冷却，绝不冷却整号。
				gap := c.modelPenaltyGap(serverTries)
				_ = c.Pool.Store().Update(acc.ID, func(a *Account) {
					a.PenalizeModel(model, gap, PenaltyServer,
						fmt.Sprintf("上游 HTTP %d 连续 %d 次", resp.StatusCode, serverTries))
					a.MarkFail(fmt.Sprintf("上游 %d 连续 %d 次（模型 %s）", resp.StatusCode, serverTries, model))
				})
				lastErr = fmt.Errorf("上游错误 HTTP %d（模型 %s，短冷却 %s）", resp.StatusCode, model, gap)
			} else {
				// 瞬时抖动（免费端点常见 500 "Internal Network Failure"）：先原地小睡后
				// 重试同一账号同一模型，多数情况下一次即成，不打惩罚。
				lastErr = fmt.Errorf("上游错误 HTTP %d", resp.StatusCode)
				if bo := c.serverRetryBackoff(serverTries); bo > 0 {
					select {
					case <-time.After(bo):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				attempt-- // 瞬时重试不占总次数预算
			}
			continue

		default:
			_ = c.Pool.Store().Update(acc.ID, func(a *Account) {
				a.MarkFail(fmt.Sprintf("HTTP %d：%s", resp.StatusCode, truncate(string(raw), 160)))
			})
			lastErr = fmt.Errorf("上游返回 HTTP %d：%s", resp.StatusCode, truncate(string(raw), 200))
			continue
		}
	}
	if lastErr == nil {
		lastErr = ErrNoAccount
	}
	return nil, lastErr
}

// modelFromBody 从 Anthropic 请求体取模型代码（即真正发给上游的那个）。失败返回空串。
func modelFromBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	return m.Model
}

// channelUsable：不考虑逐模型惩罚时，该账号能否为本请求通道服务。与 Do 里 usable 的
// 通道判定一致（纯 API Key / 带回退 Key 的 JWT 恒可；纯 Plan 的 JWT 仅 Plan 就绪时可）。
func channelUsable(a *Account, planUsable bool) bool {
	if a.Mode == ModeAPIKey || a.HasKeyFallback() {
		return true
	}
	return planUsable && a.HasJWTPath()
}

// modelCooledOut：是否存在「本可服务该通道、但该模型正冷却」的账号——用于把「上游抖动
// 导致该模型暂全线短冷却」与「根本没有能走该通道的号」两种 ErrNoAccount 区分开。
func (c *Client) modelCooledOut(model string, planUsable bool, now time.Time) bool {
	if c.Pool == nil {
		return false
	}
	anyChannelUsable := false
	for _, a := range c.Pool.Store().List() {
		if !a.Selectable(now) || !channelUsable(a, planUsable) {
			continue
		}
		anyChannelUsable = true
		if !a.ModelPenalized(model, now) {
			return false // 还有一个没冷却——那不是「全线冷却」
		}
	}
	return anyChannelUsable && model != ""
}

// modelPenaltyGap 逐模型惩罚阶梯：base * 2^(fails-1)，封顶 ModelCoolMax。
func (c *Client) modelPenaltyGap(fails int) time.Duration {
	base := c.ModelCoolBase
	if base <= 0 {
		base = 30 * time.Second
	}
	max := c.ModelCoolMax
	if max <= 0 {
		max = 5 * time.Minute
	}
	if fails < 1 {
		fails = 1
	}
	gap := base
	for i := 1; i < fails && gap < max; i++ {
		gap *= 2
	}
	if gap > max {
		gap = max
	}
	return gap
}

// serverRetryBackoff 瞬时 5xx 原地重试的小睡（有上限，别把请求拖死）。
func (c *Client) serverRetryBackoff(fails int) time.Duration {
	if c.TransientRetryGap <= 0 {
		return 0
	}
	gap := c.TransientRetryGap * time.Duration(fails)
	if gap > 2*time.Second {
		gap = 2 * time.Second
	}
	return gap
}

// Store 账号池存储（面板增删改走它）。
func (c *Client) Store() *Store {
	if c.Pool == nil {
		return nil
	}
	return c.Pool.Store()
}

// Snapshots 池内账号即时视图（面板展示）。
func (c *Client) Snapshots() []Snapshot {
	if c.Pool == nil {
		return nil
	}
	return c.Pool.Snapshots()
}

// Stats 状态计数（面板状态条）。
func (c *Client) Stats() map[string]int {
	if c.Pool == nil {
		return nil
	}
	return c.Pool.Store().Stats()
}

// postMessages 发一次上游请求（流式：不设总超时，由 ctx 控制）。
func postMessages(ctx context.Context, url string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	return zaiHTTP().Do(req)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Count 池内账号数。
func (c *Client) Count() int {
	if c.Pool == nil {
		return 0
	}
	return c.Pool.Count()
}

// HasSelectable 是否存在可选中账号（模型列表是否对外提供 zai: 的依据）。
func (c *Client) HasSelectable() bool {
	if c.Pool == nil {
		return false
	}
	return c.Pool.HasSelectable()
}

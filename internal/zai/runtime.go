package zai

import (
	"bytes"
	"context"
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
}

// NewClient 建默认编排器。
func NewClient(pool *Pool, captcha *CaptchaManager, blocks []SystemBlock) *Client {
	return &Client{
		Pool: pool, Captcha: captcha, SystemBlocks: blocks,
		MaxAttempts:     6,
		CaptchaRetries:  3,
		ServerRetries:   3,
		RateLimitWait:   20 * time.Second,
		CoolDuration:    300 * time.Second,
		ExhaustProbeGap: 10 * time.Minute,
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

		// 选号：有验证码能力时优先 Plan 通道（消耗订阅额度）
		wantPlan := c.Captcha != nil && c.Captcha.Enabled()
		acc, err := c.Pool.Pick(wantPlan)
		if err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("%w（最后一次失败：%v）", err, lastErr)
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
			if c.Captcha == nil || !c.Captcha.Enabled() {
				// 无求解器：JWT 通道不可用，退回该账号自带的 API Key
				if acc.HasKeyFallback() {
					forceFallback = true
				} else {
					c.Pool.Release(acc.ID)
					lastErr = fmt.Errorf("账号 %s 是 JWT 且未配置验证码求解器，也无回退 Key", acc.Name)
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
			_ = c.Pool.Store().Update(acc.ID, func(a *Account) { a.MarkOK() })
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
				// 反复限流或等待过久：换号，账号保持可用（不冷却）
				lastErr = fmt.Errorf("上游限流（%s）", acc.Name)
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
			_ = c.Pool.Store().Update(acc.ID, func(a *Account) {
				a.Exhaust(c.ExhaustProbeGap)
				a.MarkFail("额度用完")
			})
			lastErr = fmt.Errorf("账号 %s 额度用完", acc.Name)
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
				_ = c.Pool.Store().Update(acc.ID, func(a *Account) {
					a.Cool(c.CoolDuration)
					a.MarkFail(fmt.Sprintf("上游 %d 连续 %d 次", resp.StatusCode, serverTries))
				})
			}
			lastErr = fmt.Errorf("上游错误 HTTP %d", resp.StatusCode)
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

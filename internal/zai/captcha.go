package zai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 上游验证码配置（client/configs 拉取失败时的兜底值；region 线上实测为 cn）。
const (
	defaultSceneID  = "11xygtvd"
	defaultRegion   = "cn"
	defaultPrefix   = "no8xfe"
	clientConfigs   = "https://zcode.z.ai/api/v1/client/configs"
	clientAppVer    = "3.11.2"
	configCacheTTL  = 30 * time.Minute
	solveRetries    = 3
	defaultPoolMin  = 4
	defaultPoolMax  = 12
	defaultTokenTTL = 100 * time.Second // verifyParam 实际有效期约 2 分钟，留安全余量
)

// regionFallbacks 求解区域候选。
//
// 上游 client/configs 声明 region=cn，但该 scene（11xygtvd）在部分出口 IP 上用
// cn 只会返回**降级 token**（只有 certifyId、无 securityToken，上游必然判 3007），
// 换 sgp 才拿得到完整 token。故按 [配置区域, sgp, cn] 依次尝试，命中即缓存，
// 之后直接走 last-good 区域，避免每次都白解一遍 cn。
var regionFallbacks = []string{"sgp", "cn"}

// errDegradedParam 求解器返回了降级 token（缺 securityToken）。这是**确定性**
// 失败——同区域再解还是短的，故 solve 遇到它立即换区域，不空耗 solveRetries。
var errDegradedParam = errors.New("求解结果过短，疑似降级")

// SolverConfig 外部验证码求解器配置。
//
// **本包不内嵌求解器实现**：阿里云无痕验证的求解需要在模拟浏览器里跑官方 SDK
// （zcode2api 用 Node + happy-dom 实现，AGPL-3.0）。buddyhub 只约定契约——
//
//	<command> <script> <sceneId> <region> <prefix>
//
// 求解器把结果按 `VERIFY_PARAM=<param>` 打到 stdout 即可（zcode2api 的
// captcha_node/solver.js 天然满足）。这样 MIT 许可不受影响，求解器也可独立升级。
type SolverConfig struct {
	Command  string        // 可执行文件，缺省 node
	Script   string        // solver.js 路径；空 = 未配置（Plan 通道自动降级为不可用）
	Timeout  time.Duration // 单次求解超时，缺省 40s
	PoolMin  int           // 预热池下限
	PoolMax  int           // 预热池上限
	TokenTTL time.Duration // 单枚 token 最大可用时长
}

func (c SolverConfig) normalized() SolverConfig {
	if c.Command == "" {
		c.Command = "node"
	}
	if c.Timeout <= 0 {
		c.Timeout = 40 * time.Second
	}
	if c.PoolMin <= 0 {
		c.PoolMin = defaultPoolMin
	}
	if c.PoolMax < c.PoolMin {
		c.PoolMax = max(defaultPoolMax, c.PoolMin)
	}
	if c.TokenTTL <= 0 {
		c.TokenTTL = defaultTokenTTL
	}
	return c
}

// Enabled 求解器是否已配置。
func (c SolverConfig) Enabled() bool { return strings.TrimSpace(c.Script) != "" }

type captchaToken struct {
	param  string
	region string
	bornAt time.Time
}

func (t captchaToken) expired(ttl time.Duration) bool { return time.Since(t.bornAt) >= ttl }

// CaptchaManager 验证码预解池。
//
// 热路径永不等待：请求到来时直接从池里取一枚已解好的 token（亚毫秒），
// 后台循环持续补货。上游返回验证码挑战时整池作废——那批 token/指纹很可能
// 已被风控盯上，继续复用只会连环 3007。
type CaptchaManager struct {
	cfg SolverConfig

	mu       sync.Mutex
	pool     []captchaToken
	refill   bool
	lastErr  string
	// lastRegion 最近一次求解成功的区域；非空时优先复用（跳过已知降级的区域）。
	lastRegion string
	cfgCache   struct {
		scene, region, prefix string
		at                    time.Time
	}

	// gate 决定是否允许预热（只有存在可走 Plan 的账号才解，避免空转产生上游流量）
	gate func() bool

	stop chan struct{}
	once sync.Once
}

// NewCaptchaManager 建管理器。gate 返回 false 时后台循环只淘汰过期 token，不解新码。
func NewCaptchaManager(cfg SolverConfig, gate func() bool) *CaptchaManager {
	if gate == nil {
		gate = func() bool { return true }
	}
	return &CaptchaManager{cfg: cfg.normalized(), gate: gate, stop: make(chan struct{})}
}

// Enabled 是否可用（求解器已配置）。
func (m *CaptchaManager) Enabled() bool { return m.cfg.Enabled() }

// Start 启动后台预热循环（幂等）。
func (m *CaptchaManager) Start() {
	m.once.Do(func() { go m.loop() })
}

// Stop 停止后台循环。
func (m *CaptchaManager) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
}

// LastError 最近一次求解失败原因（面板展示）。
func (m *CaptchaManager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// PoolSize 当前池内有效 token 数。
func (m *CaptchaManager) PoolSize() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, t := range m.pool {
		if !t.expired(m.cfg.TokenTTL) {
			n++
		}
	}
	return n
}

func (m *CaptchaManager) loop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.evictExpired()
			if !m.cfg.Enabled() || !m.gate() {
				continue
			}
			if m.PoolSize() < m.cfg.PoolMin {
				m.refillBatch(m.cfg.PoolMin - m.PoolSize())
			}
		}
	}
}

// Get 取一枚可用 token：优先池内现成的，池空则同步现解一次。
// 返回 (verifyParam, region)。求解器未配置时返回错误（调用方降级到 API Key 通道）。
func (m *CaptchaManager) Get(ctx context.Context) (string, string, error) {
	if !m.cfg.Enabled() {
		return "", "", fmt.Errorf("未配置验证码求解器（zai.captcha_solver），Plan 通道不可用")
	}
	m.mu.Lock()
	for len(m.pool) > 0 {
		t := m.pool[0]
		m.pool = m.pool[1:]
		if !t.expired(m.cfg.TokenTTL) {
			m.mu.Unlock()
			go m.refillBatch(1) // 后台补货（防重入在 refillBatch 内）
			return t.param, t.region, nil
		}
	}
	m.mu.Unlock()

	// 池空：同步现解（首启兜底；正常情况下后台循环已预热）
	cfg := m.fetchConfig(ctx)
	tok, err := m.solve(ctx, cfg)
	if err != nil {
		return "", "", err
	}
	return tok.param, tok.region, nil
}

// Invalidate 上游返回验证码挑战时清空整池。
func (m *CaptchaManager) Invalidate() {
	m.mu.Lock()
	n := len(m.pool)
	m.pool = nil
	m.mu.Unlock()
	if n > 0 {
		logf("zai-captcha: 验证码失效，清空池 %d 枚", n)
	}
}

func (m *CaptchaManager) evictExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.pool[:0]
	for _, t := range m.pool {
		if !t.expired(m.cfg.TokenTTL) {
			kept = append(kept, t)
		}
	}
	m.pool = kept
}

func (m *CaptchaManager) put(t captchaToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pool) >= m.cfg.PoolMax {
		return
	}
	m.pool = append(m.pool, t)
}

func (m *CaptchaManager) refillBatch(n int) {
	m.mu.Lock()
	if m.refill {
		m.mu.Unlock()
		return
	}
	m.refill = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.refill = false
		m.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Timeout*time.Duration(n+1))
	defer cancel()
	cfg := m.fetchConfig(ctx)
	for i := 0; i < n; i++ {
		if !m.cfg.Enabled() || !m.gate() {
			return
		}
		tok, err := m.solve(ctx, cfg)
		if err != nil {
			m.mu.Lock()
			m.lastErr = err.Error()
			m.mu.Unlock()
			return
		}
		m.put(tok)
	}
}

// captchaCfg 上游下发的验证码参数。
type captchaCfg struct{ scene, region, prefix string }

func (m *CaptchaManager) fetchConfig(ctx context.Context) captchaCfg {
	m.mu.Lock()
	if !m.cfgCache.at.IsZero() && time.Since(m.cfgCache.at) < configCacheTTL {
		c := captchaCfg{m.cfgCache.scene, m.cfgCache.region, m.cfgCache.prefix}
		m.mu.Unlock()
		return c
	}
	m.mu.Unlock()

	c := captchaCfg{defaultSceneID, defaultRegion, defaultPrefix}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		clientConfigs+"?app_version="+clientAppVer, nil)
	if err == nil {
		req.Header.Set("User-Agent", "ZCode/"+clientAppVer)
		if resp, rerr := zaiHTTP().Do(req); rerr == nil {
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			var body struct {
				Data struct {
					Configs struct {
						Captcha struct {
							SceneID string `json:"sceneId"`
							Region  string `json:"region"`
							Prefix  string `json:"prefix"`
						} `json:"captcha"`
					} `json:"configs"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &body) == nil {
				cc := body.Data.Configs.Captcha
				if cc.SceneID != "" {
					c.scene = cc.SceneID
				}
				if cc.Region != "" {
					c.region = cc.Region
				}
				if cc.Prefix != "" {
					c.prefix = cc.Prefix
				}
			}
		}
	}

	m.mu.Lock()
	m.cfgCache.scene, m.cfgCache.region, m.cfgCache.prefix = c.scene, c.region, c.prefix
	m.cfgCache.at = time.Now()
	m.mu.Unlock()
	return c
}

// solve 跑外部求解器，解析 stdout 的 VERIFY_PARAM=。
//
// 按 regionCandidates 依次尝试各区域：某区域返回降级 token（errDegradedParam）
// 是确定性失败，立即换下一个区域而不空耗重试；其余错误（启动失败/超时无输出）
// 在同区域内重试至多 solveRetries 次。命中后把区域记为 last-good，后续直接复用。
func (m *CaptchaManager) solve(ctx context.Context, cfg captchaCfg) (captchaToken, error) {
	var lastErr error
	for _, region := range m.regionCandidates(cfg.region) {
		rcfg := captchaCfg{scene: cfg.scene, region: region, prefix: cfg.prefix}
		for attempt := 1; attempt <= solveRetries; attempt++ {
			param, err := m.runSolver(ctx, rcfg)
			if err == nil && param != "" {
				m.setLastRegion(region)
				return captchaToken{param: param, region: region, bornAt: time.Now()}, nil
			}
			if err == nil {
				err = fmt.Errorf("求解器未输出 VERIFY_PARAM")
			}
			lastErr = err
			if ctx.Err() != nil {
				return captchaToken{}, fmt.Errorf("验证码求解失败：%w", lastErr)
			}
			if errors.Is(err, errDegradedParam) {
				break // 该区域确定性降级：换区域，别在同区域空转
			}
		}
	}
	return captchaToken{}, fmt.Errorf("验证码求解失败：%w", lastErr)
}

// regionCandidates 去重后的区域尝试顺序：last-good → 配置区域 → 兜底(sgp/cn)。
func (m *CaptchaManager) regionCandidates(primary string) []string {
	m.mu.Lock()
	last := m.lastRegion
	m.mu.Unlock()
	out := make([]string, 0, len(regionFallbacks)+2)
	seen := map[string]bool{}
	add := func(r string) {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			return
		}
		seen[r] = true
		out = append(out, r)
	}
	add(last)
	add(primary)
	for _, r := range regionFallbacks {
		add(r)
	}
	return out
}

func (m *CaptchaManager) setLastRegion(region string) {
	m.mu.Lock()
	m.lastRegion = region
	m.mu.Unlock()
}

func (m *CaptchaManager) runSolver(ctx context.Context, cfg captchaCfg) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, m.cfg.Command, m.cfg.Script, cfg.scene, cfg.region, cfg.prefix)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = nil // 求解器 stderr 是排障信息，不并入结果
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("无法启动求解器 %s: %w", m.cfg.Command, err)
	}

	param := ""
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "VERIFY_PARAM="); ok {
			param = strings.TrimSpace(v)
		}
	}
	_ = cmd.Wait()
	if runCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("求解超时（%s）", m.cfg.Timeout)
	}
	if param == "" {
		return "", fmt.Errorf("求解器无输出（退出码 %v）", cmd.ProcessState)
	}
	if len(param) < 200 {
		// 短参数是降级结果（缺 securityToken），上游必然 3007 —— 确定性失败，换区域再解
		return "", fmt.Errorf("%w（%d 字符）", errDegradedParam, len(param))
	}
	return param, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

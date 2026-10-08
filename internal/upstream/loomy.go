package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/atomicfile"
)

// DefaultLoomyBaseURL Loomy 官方生产环境任务服务地址。
const DefaultLoomyBaseURL = "https://loomyad.xunfei.cn"

// LoomyTaskMeta 任务元信息。
type LoomyTaskMeta struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Points   int    `json:"points"`
}

// LoomyRegistry 全部 8 项新手任务定义（总计 10000 积分）。
var LoomyRegistry = []LoomyTaskMeta{
	{Key: "first_message", Title: "发送你的第一条消息", Category: "初识 Loomy", Points: 500},
	{Key: "pick_skill", Title: "试试选择一个技能", Category: "初识 Loomy", Points: 1000},
	{Key: "generate_ppt", Title: "生成第一份 PPT", Category: "初识 Loomy", Points: 1500},
	{Key: "set_schedule", Title: "设置定时任务", Category: "打造你的专属 Loomy", Points: 1000},
	{Key: "install_skill", Title: "安装一个新技能", Category: "打造你的专属 Loomy", Points: 1500},
	{Key: "configure_remote", Title: "配置一个连接器", Category: "打造你的专属 Loomy", Points: 1000},
	{Key: "create_soul", Title: "创建你的 AI 搭子", Category: "遇见你的 AI 搭子", Points: 1500},
	{Key: "share_soul", Title: "分享你的 AI 搭子", Category: "遇见你的 AI 搭子", Points: 2000},
}

// LoomyTaskItem 带当前完成状态的任务项。
type LoomyTaskItem struct {
	LoomyTaskMeta
	Completed bool `json:"completed"`
}

// LoomySession 本地已登录会话信息。
type LoomySession struct {
	Session     string `json:"session"`
	UserID      string `json:"userid"`
	Phone       string `json:"phone"`
	UserDataDir string `json:"userDataDir"`
}

// LoomyStatus 对外暴露的状态聚合视图。
type LoomyStatus struct {
	HasAccount  bool            `json:"has_account"`
	UserID      string          `json:"userid,omitempty"`
	PhoneMasked string          `json:"phone_masked,omitempty"`
	UserDataDir string          `json:"user_data_dir,omitempty"`
	Earned      int             `json:"earned"`
	Total       int             `json:"total"`
	Tasks       map[string]bool `json:"tasks"`
	Items       []LoomyTaskItem `json:"items"`
	Error       string          `json:"error,omitempty"`
}

// LoomyClient 封装 Loomy 任务接口调用。
type LoomyClient struct {
	BaseURL string
	HTTP    *http.Client
	mu      sync.Mutex
}

// loomyDialIPs loomyad.xunfei.cn 的直连 IP，按实测链路质量排序
// （.245 8/8、.244 4/8，2026-10-04 实测）。两个都试过再报错。
var loomyDialIPs = []string{"114.118.75.245", "114.118.75.244"}

// NewLoomyClient 创建客户端。
func NewLoomyClient(baseURL string) *LoomyClient {
	if baseURL == "" {
		baseURL = DefaultLoomyBaseURL
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}
			if addr == "loomyad.xunfei.cn:443" {
				// 直连公网真实 IP，避免 VPN/代理 Fake-IP (198.18.*) 导致的握手超时。
				// 该域名 DNS 轮询 .244 / .245 两个 IP，.244 在部分机房（实测
				// Contabo DE）握手成功率只有 ~50%，随机踩中就 TLS handshake
				// timeout。故按实测成功率排序直连，且**不回落 DNS**——回落即随机。
				for _, ip := range loomyDialIPs {
					if conn, err := dialer.DialContext(ctx, network, ip+":443"); err == nil {
						return conn, nil
					}
				}
				return nil, fmt.Errorf("loomyad.xunfei.cn: 直连 IP 均不可达 %v", loomyDialIPs)
			}
			return dialer.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			ServerName: "loomyad.xunfei.cn",
		},
	}
	return &LoomyClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP: &http.Client{
			Transport: tr,
			Timeout:   20 * time.Second,
		},
	}
}

// loomySessionDir 面板单号凭据文件所在目录，默认 "data"（历史行为：相对 CWD）。
// 启动装配时用 SetLoomySessionDir 改写为 state_file 的同目录。
var loomySessionDir = "data"

// SetLoomySessionDir 指定 loomy-session.json 的目录（cmd/server 传 state_file
// 的兄弟目录，与 usage.json / model.json / task-state.json 同一条规则）。
//
// 为什么需要一个 setter 而不是继续写死 "data"：数据目录挪走之后（systemd 换了
// WorkingDirectory、或 config 里改了 state_file），这里仍会往**当前目录**新建
// 一个 data/，于是面板 Loomy 页读到空目录、桥的凭据来源 2 失效——而账号在
// 外部池里明明是好用的。空串与 "." 一律忽略，绝不把凭据摊到当前目录。
func SetLoomySessionDir(dir string) {
	if dir != "" && dir != "." {
		loomySessionDir = dir
	}
}

// loomySessionPath 面板与 loomy 桥共用的单号凭据路径。
func loomySessionPath() string { return filepath.Join(loomySessionDir, "loomy-session.json") }

// FindLoomySession 自动探测或读取 Loomy 客户端的登录态。
func FindLoomySession() (*LoomySession, error) {
	// 0. 优先检查本地面板保存的会话（目录见 loomySessionPath）
	if data, err := os.ReadFile(loomySessionPath()); err == nil {
		var raw LoomySession
		if json.Unmarshal(data, &raw) == nil && raw.Session != "" {
			return &raw, nil
		}
	}

	// 1. 优先使用环境变量指定的路径
	if envDir := os.Getenv("LOOMY_USERDATA"); envDir != "" {
		if s, err := readAuthSessionFromDir(envDir); err == nil {
			return s, nil
		}
	}

	// 2. 检索公共目录 <PUBLIC>\Loomy\*\userData\auth-session.json。PUBLIC 是
	//    Windows 独有变量；原先 PUBLIC 未设时会伪造一个 C:\ 字面量继续找，在
	//    Linux/macOS 上永远匹配不到（filepath.Glob 不把反斜杠当分隔符），白跑
	//    一趟还让仓库里留了一条跨平台假路径。
	publicLoomy := ""
	if pub := os.Getenv("PUBLIC"); pub != "" {
		publicLoomy = filepath.Join(pub, "Loomy")
	} else if runtime.GOOS == "windows" {
		publicLoomy = `C:\Users\Public\Loomy` // 精简环境无 PUBLIC 变量时的等价路径
	}
	if publicLoomy != "" {
		if s, ok := firstAuthSessionUnder(publicLoomy); ok {
			return s, nil
		}
	}

	// 3. 检索当前用户的客户端目录。os.UserConfigDir 在三平台各自给出正确位置
	//    （Windows=%APPDATA%，Linux=~/.config，macOS=~/Library/Application
	//    Support），Electron 系客户端的 userData 都落在这里——原先只查
	//    APPDATA，Linux/macOS 上这一步直接跳过。
	if cfgDir, err := os.UserConfigDir(); err == nil {
		if s, ok := firstAuthSessionUnder(filepath.Join(cfgDir, "Loomy")); ok {
			return s, nil
		}
	}

	return nil, fmt.Errorf("未找到本地 Loomy 客户端登录态 (auth-session.json)")
}

// firstAuthSessionUnder 在 <base>/*/userData/auth-session.json 里取第一个可用会话。
func firstAuthSessionUnder(base string) (*LoomySession, bool) {
	matches, _ := filepath.Glob(filepath.Join(base, "*", "userData", "auth-session.json"))
	for _, m := range matches {
		if s, err := readAuthSessionFromDir(filepath.Dir(m)); err == nil && s.Session != "" {
			return s, true
		}
	}
	return nil, false
}

// SaveLoomySession 保存 Loomy 凭证到 loomy-session.json（目录随 state_file 走）。
func SaveLoomySession(s *LoomySession) error {
	if s == nil || s.Session == "" {
		return fmt.Errorf("session 为空")
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// 走共享原子写入器（0600 + 临时文件改名 + 同路径串行）。这里原先是
	// os.WriteFile(..., 0644)：Windows 忽略权限位所以本地看不出问题，Linux
	// 上等于把这个号的登录态摊给同机所有用户和任何能读目录的进程。
	return atomicfile.Write(loomySessionPath(), data)
}

func readAuthSessionFromDir(dir string) (*LoomySession, error) {
	path := filepath.Join(dir, "auth-session.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Session string `json:"session"`
		UserID  string `json:"userid"`
		Phone   string `json:"phone"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.Session == "" {
		return nil, fmt.Errorf("session token 为空")
	}
	return &LoomySession{
		Session:     raw.Session,
		UserID:      raw.UserID,
		Phone:       raw.Phone,
		UserDataDir: dir,
	}, nil
}

// MaskPhone 手机号脱敏（前3后4，中间打码）。
func MaskPhone(phone string) string {
	if len(phone) >= 7 {
		return phone[:3] + "****" + phone[len(phone)-4:]
	}
	return phone
}

// GetTasks 从云端获取任务进度状态。
func (c *LoomyClient) GetTasks(session string) (tasks map[string]bool, earned int, total int, err error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/api/v1/onboarding/tasks", nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("token", session)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, 0, err
	}

	var envelope struct {
		Code string `json:"code"`
		Desc string `json:"desc"`
		Data struct {
			Tasks  map[string]bool `json:"tasks"`
			Earned int             `json:"earned"`
			Total  int             `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, 0, 0, fmt.Errorf("解析 Loomy 响应失败: %w", err)
	}
	if envelope.Code != "000000" {
		return nil, 0, 0, fmt.Errorf("Loomy API 错误: %s (%s)", envelope.Desc, envelope.Code)
	}

	resTasks := make(map[string]bool)
	for _, meta := range LoomyRegistry {
		resTasks[meta.Key] = envelope.Data.Tasks[meta.Key]
	}
	computedEarned := 0
	for _, meta := range LoomyRegistry {
		if resTasks[meta.Key] {
			computedEarned += meta.Points
		}
	}
	return resTasks, computedEarned, 10000, nil
}

// CompleteTask 完成单个任务。
func (c *LoomyClient) CompleteTask(session, key string) (alreadyCompleted bool, balance int, err error) {
	reqBody, _ := json.Marshal(map[string]string{"key": key})
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/v1/onboarding/tasks/complete", bytes.NewReader(reqBody))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("token", session)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, 0, err
	}

	var envelope struct {
		Code string `json:"code"`
		Desc string `json:"desc"`
		Data struct {
			AlreadyCompleted bool `json:"alreadyCompleted"`
			Balance          int  `json:"balance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false, 0, fmt.Errorf("解析 Loomy 响应失败: %w", err)
	}
	if envelope.Code != "000000" {
		return false, 0, fmt.Errorf("Loomy API 错误: %s (%s)", envelope.Desc, envelope.Code)
	}

	return envelope.Data.AlreadyCompleted, envelope.Data.Balance, nil
}

// SyncLocalCache 同步写入 Loomy 客户端本地缓存 onboarding-tasks.json。
func SyncLocalCache(userDataDir string, tasks map[string]bool, earned int) error {
	if userDataDir == "" {
		return nil
	}
	targetPath := filepath.Join(userDataDir, "onboarding-tasks.json")
	payload := map[string]any{
		"version":   1,
		"tasks":     tasks,
		"earned":    earned,
		"updatedAt": time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := fmt.Sprintf("%s.%d.tmp", targetPath, os.Getpid())
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, targetPath)
}

// CompleteAll 自动完成所有未完成的任务，并持久化到本地缓存。
func (c *LoomyClient) CompleteAll(session *LoomySession) (completedKeys []string, status *LoomyStatus, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	tasks, earned, total, err := c.GetTasks(session.Session)
	if err != nil {
		return nil, nil, fmt.Errorf("拉取任务状态失败: %w", err)
	}

	var newlyCompleted []string
	for _, meta := range LoomyRegistry {
		if tasks[meta.Key] {
			continue
		}
		// 任务未完成，提交完成上报
		_, _, err := c.CompleteTask(session.Session, meta.Key)
		if err != nil {
			// 单任务失败记录日志并继续，保证其它任务不受影响
			continue
		}
		tasks[meta.Key] = true
		earned += meta.Points
		newlyCompleted = append(newlyCompleted, meta.Key)
		time.Sleep(300 * time.Millisecond)
	}

	// 本地缓存同步
	if session.UserDataDir != "" {
		_ = SyncLocalCache(session.UserDataDir, tasks, earned)
	}

	items := make([]LoomyTaskItem, len(LoomyRegistry))
	for i, meta := range LoomyRegistry {
		items[i] = LoomyTaskItem{
			LoomyTaskMeta: meta,
			Completed:     tasks[meta.Key],
		}
	}

	status = &LoomyStatus{
		HasAccount:  true,
		UserID:      session.UserID,
		PhoneMasked: MaskPhone(session.Phone),
		UserDataDir: session.UserDataDir,
		Earned:      earned,
		Total:       total,
		Tasks:       tasks,
		Items:       items,
	}
	return newlyCompleted, status, nil
}

// QueryStatus 查询聚合状态。
func (c *LoomyClient) QueryStatus(session *LoomySession) *LoomyStatus {
	if session == nil || session.Session == "" {
		return &LoomyStatus{
			HasAccount: false,
			Earned:     0,
			Total:      10000,
			Tasks:      map[string]bool{},
			Items:      nil,
		}
	}

	tasks, earned, total, err := c.GetTasks(session.Session)
	if err != nil {
		// 若云端请求失败，尝试从本地缓存读取兜底
		if session.UserDataDir != "" {
			cachePath := filepath.Join(session.UserDataDir, "onboarding-tasks.json")
			if raw, errRead := os.ReadFile(cachePath); errRead == nil {
				var cache struct {
					Tasks  map[string]bool `json:"tasks"`
					Earned int             `json:"earned"`
				}
				if json.Unmarshal(raw, &cache) == nil && cache.Tasks != nil {
					items := make([]LoomyTaskItem, len(LoomyRegistry))
					for i, meta := range LoomyRegistry {
						items[i] = LoomyTaskItem{
							LoomyTaskMeta: meta,
							Completed:     cache.Tasks[meta.Key],
						}
					}
					return &LoomyStatus{
						HasAccount:  true,
						UserID:      session.UserID,
						PhoneMasked: MaskPhone(session.Phone),
						UserDataDir: session.UserDataDir,
						Earned:      cache.Earned,
						Total:       10000,
						Tasks:       cache.Tasks,
						Items:       items,
					}
				}
			}
		}
		return &LoomyStatus{
			HasAccount:  true,
			UserID:      session.UserID,
			PhoneMasked: MaskPhone(session.Phone),
			UserDataDir: session.UserDataDir,
			Error:       err.Error(),
			Total:       10000,
		}
	}

	items := make([]LoomyTaskItem, len(LoomyRegistry))
	for i, meta := range LoomyRegistry {
		items[i] = LoomyTaskItem{
			LoomyTaskMeta: meta,
			Completed:     tasks[meta.Key],
		}
	}

	// 成功获取后，顺带更新本地缓存
	if session.UserDataDir != "" {
		_ = SyncLocalCache(session.UserDataDir, tasks, earned)
	}

	return &LoomyStatus{
		HasAccount:  true,
		UserID:      session.UserID,
		PhoneMasked: MaskPhone(session.Phone),
		UserDataDir: session.UserDataDir,
		Earned:      earned,
		Total:       total,
		Tasks:       tasks,
		Items:       items,
	}
}

// ---------------------------------------------------------------------------
// 积分池：每日额度签到与余额查询（协议对齐 Jet-Hub loomy-credits.ts）
// ---------------------------------------------------------------------------

// LoomyCreditDetail 双积分池明细：永久积分 + 每日赠送池。
type LoomyCreditDetail struct {
	Permanent  int    `json:"permanent"`
	Daily      int    `json:"daily"`
	Total      int    `json:"total"`
	DailyQuota int    `json:"daily_quota,omitempty"`
	HasQuota   bool   `json:"has_quota,omitempty"`
	Consumed   int    `json:"daily_consumed,omitempty"`
	CycleDate  string `json:"daily_cycle_date,omitempty"`
}

// LoomyCheckinResult 每日额度签到结果。
type LoomyCheckinResult struct {
	AlreadyProcessed bool   `json:"already_processed"`
	Granted          int    `json:"granted"`
	DailyBalance     int    `json:"daily_balance"`
	DailyQuota       int    `json:"daily_quota"`
	HasQuota         bool   `json:"has_quota,omitempty"`
	Message          string `json:"message"`
}

// loomyEnvelope 统一拆 Loomy 业务信封：业务失败恒 HTTP 200，成败只看 body.code。
func loomyEnvelope(body []byte) (code, desc string, data json.RawMessage, err error) {
	var env struct {
		Code    string          `json:"code"`
		Desc    string          `json:"desc"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", "", nil, fmt.Errorf("解析 Loomy 响应失败: %w", err)
	}
	return env.Code, env.Desc, env.Data, nil
}

// creditCacheTTL 积分明细缓存有效期。
//
// loomyad.xunfei.cn 从海外机房访问链路极不稳定：同一小时内实测 .245 8/8、
// 半小时后 2/12，.244 同期 3/12，两个 IP 都在抖（DNS 只有这两个 A 记录，
// 换 IP 无解）。面板每次刷新都查一次，不缓存等于把抖动原样糊到 UI 上，
// 用户看到的就是「一会正常一会 TLS handshake timeout」。
const creditCacheTTL = 5 * time.Minute

// creditStaleTTL 缓存最长保鲜期：重试全败时宁可给略旧的真数，也不给红字。
const creditStaleTTL = 30 * time.Minute

type creditEntry struct {
	at time.Time
	d  LoomyCreditDetail
}

// 缓存是包级的：extstore 每次查余额都 NewLoomyClient()，挂在实例上等于没缓存。
var (
	creditMu    sync.Mutex
	creditCache = map[string]creditEntry{}
)

// GetCreditDetail 查询双积分池明细（只读，走 /points/records，无副作用）。
//
// 带 5 分钟缓存 + 3 次重试：上游抖动是常态，单次失败不代表账号有问题。
func (c *LoomyClient) GetCreditDetail(session string) (*LoomyCreditDetail, error) {
	if d, ok := lookupCredit(session, creditCacheTTL); ok {
		return d, nil
	}
	var lastErr error
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 400 * time.Millisecond)
		}
		d, err := c.getCreditDetailOnce(session)
		if err == nil {
			creditMu.Lock()
			creditCache[session] = creditEntry{at: time.Now(), d: *d}
			creditMu.Unlock()
			return d, nil
		}
		lastErr = err
	}
	if d, ok := lookupCredit(session, creditStaleTTL); ok {
		return d, nil
	}
	return nil, lastErr
}

// lookupCredit 取缓存副本（maxAge 内），返回拷贝避免调用方改到共享数据。
func lookupCredit(session string, maxAge time.Duration) (*LoomyCreditDetail, bool) {
	if session == "" {
		return nil, false
	}
	creditMu.Lock()
	defer creditMu.Unlock()
	e, ok := creditCache[session]
	if !ok || time.Since(e.at) > maxAge {
		return nil, false
	}
	d := e.d
	return &d, true
}

// getCreditDetailOnce 单次真实请求（无缓存无重试）。
func (c *LoomyClient) getCreditDetailOnce(session string) (*LoomyCreditDetail, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/api/v1/points/records?pageNo=1&pageSize=1&recordType=all", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("token", session)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	code, desc, data, err := loomyEnvelope(body)
	if err != nil {
		return nil, err
	}
	if code != "000000" {
		return nil, fmt.Errorf("Loomy API 错误: %s (%s)", desc, code)
	}
	var raw struct {
		Balance          *float64 `json:"balance"`
		DailyBalance     *float64 `json:"dailyBalance"`
		AvailableBalance *float64 `json:"availableBalance"`
		DailyQuota       *float64 `json:"dailyQuota"`
		DailyConsumed    *float64 `json:"dailyConsumed"`
		DailyCycleDate   string   `json:"dailyCycleDate"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析积分明细失败: %w", err)
	}
	if raw.Balance == nil {
		return nil, fmt.Errorf("积分响应缺少 balance 字段")
	}
	toInt := func(f *float64) int {
		if f == nil {
			return 0
		}
		return int(*f)
	}
	detail := &LoomyCreditDetail{
		Permanent: toInt(raw.Balance),
		Daily:     toInt(raw.DailyBalance),
		Total:     toInt(raw.Balance) + toInt(raw.DailyBalance),
		CycleDate: raw.DailyCycleDate,
	}
	if raw.AvailableBalance != nil {
		detail.Total = toInt(raw.AvailableBalance)
	}
	if raw.DailyQuota != nil {
		detail.DailyQuota = toInt(raw.DailyQuota)
		detail.HasQuota = true
		detail.Consumed = toInt(raw.DailyConsumed)
	}
	return detail, nil
}

// CheckinDailyQuota 触发每日赠送额度（POST /points/first-login，幂等）。
// 语义是「触发每日额度重置」而非「+5000 积分」：dailyBalance = dailyQuota - dailyConsumed。
func (c *LoomyClient) CheckinDailyQuota(session string) (*LoomyCheckinResult, error) {
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/v1/points/first-login", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("token", session)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	code, desc, data, err := loomyEnvelope(body)
	if err != nil {
		return nil, err
	}
	if code != "000000" {
		return nil, fmt.Errorf("Loomy API 错误: %s (%s)", desc, code)
	}
	var raw struct {
		AlreadyProcessed bool     `json:"alreadyProcessed"`
		DailyQuota       *float64 `json:"dailyQuota"`
		DailyBalance     *float64 `json:"dailyBalance"`
		DailyConsumed    *float64 `json:"dailyConsumed"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析签到响应失败: %w", err)
	}
	toInt := func(f *float64) int {
		if f == nil {
			return 0
		}
		return int(*f)
	}
	res := &LoomyCheckinResult{
		AlreadyProcessed: raw.AlreadyProcessed,
		DailyBalance:     toInt(raw.DailyBalance),
	}
	if raw.DailyQuota != nil {
		res.DailyQuota = toInt(raw.DailyQuota)
		res.HasQuota = true
	}
	if raw.AlreadyProcessed {
		if res.HasQuota {
			res.Message = fmt.Sprintf("今日额度已初始化（每日 %d/%d）", res.DailyBalance, res.DailyQuota)
		} else {
			res.Message = "今日额度已初始化"
		}
		return res, nil
	}
	// 首次处理：本次新发放额度 = dailyQuota - dailyConsumed；两者缺其一时回退 dailyQuota（不编造差值）。
	if raw.DailyQuota != nil && raw.DailyConsumed != nil {
		res.Granted = res.DailyQuota - toInt(raw.DailyConsumed)
		if res.Granted < 0 {
			res.Granted = 0
		}
	} else {
		res.Granted = res.DailyQuota
	}
	res.Message = fmt.Sprintf("每日额度已激活 +发放 %d", res.Granted)
	return res, nil
}

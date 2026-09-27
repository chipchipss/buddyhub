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
	"strings"
	"sync"
	"time"
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
				// 直连公网真实 IP，避免 VPN/代理 Fake-IP (198.18.*) 导致的握手超时
				conn, err := dialer.DialContext(ctx, network, "114.118.75.245:443")
				if err == nil {
					return conn, nil
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			ServerName: "loomyad.xunfei.cn",
		},
	}
	return &LoomyClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP: &http.Client{
			Transport: tr,
			Timeout:   15 * time.Second,
		},
	}
}

// FindLoomySession 自动探测或读取 Loomy 客户端的登录态。
func FindLoomySession() (*LoomySession, error) {
	// 0. 优先检查本地面板保存的会话 data/loomy-session.json
	dataPath := filepath.Join("data", "loomy-session.json")
	if data, err := os.ReadFile(dataPath); err == nil {
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

	// 2. 检索公共目录 C:\Users\Public\Loomy\*\userData\auth-session.json
	publicLoomy := filepath.Join(os.Getenv("PUBLIC"), "Loomy")
	if publicLoomy == "" || publicLoomy == "Loomy" {
		publicLoomy = `C:\Users\Public\Loomy`
	}
	matches, _ := filepath.Glob(filepath.Join(publicLoomy, "*", "userData", "auth-session.json"))
	for _, m := range matches {
		dir := filepath.Dir(m)
		if s, err := readAuthSessionFromDir(dir); err == nil && s.Session != "" {
			return s, nil
		}
	}

	// 3. 检索当前用户 AppData
	if appData := os.Getenv("APPDATA"); appData != "" {
		matches, _ = filepath.Glob(filepath.Join(appData, "Loomy", "*", "userData", "auth-session.json"))
		for _, m := range matches {
			dir := filepath.Dir(m)
			if s, err := readAuthSessionFromDir(dir); err == nil && s.Session != "" {
				return s, nil
			}
		}
	}

	return nil, fmt.Errorf("未找到本地 Loomy 客户端登录态 (auth-session.json)")
}

// SaveLoomySession 保存 Loomy 凭证到 data/loomy-session.json。
func SaveLoomySession(s *LoomySession) error {
	if s == nil || s.Session == "" {
		return fmt.Errorf("session 为空")
	}
	if err := os.MkdirAll("data", 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join("data", "loomy-session.json"), data, 0644)
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

// GetCreditDetail 查询双积分池明细（只读，走 /points/records，无副作用）。
func (c *LoomyClient) GetCreditDetail(session string) (*LoomyCreditDetail, error) {
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

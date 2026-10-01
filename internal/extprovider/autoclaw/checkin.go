package autoclaw

// checkin.go AutoClaw 每日签到。
//
// 协议来自 agent2api 的 autoclaw/checkin.rs（该文件注释写明：不是移植而是
// **逆向**，接口从桌面端 app.asar 里读出并实测确认）。
//
//	POST {userapi}/autoclaw-proxy/proxy/autoclaw-task-complete
//	     body: {"task_id":"daily_signin"}
//	     → 200 {"data":{"already_completed":true,"reward_points":0,
//	                      "success":false,"task_id":"daily_signin"}}
//
// ── 三个坑 ──────────────────────────────────────────────────────
//  1. **判据不是 HTTP 状态码**：上游恒回 200。判「本次真领到」看
//     `reward_points > 0 || (success && !already_completed)` ——
//     只看 200 会把「今天已领过」记成一次成功领取。
//  2. **不是签到专用接口**：`autoclaw-task-complete` 是「完成客户端任务」的
//     通用入口，`daily_signin` 只是其中一个 task_id（同级还有
//     `daily_inspiration_center` / `upgrade_pc_app`），所以请求体只有一个字段。
//  3. 走与积分/刷新**同一套**签名（`userAPIHeaders`），不要另写一份——
//     appId/appKey 或时间戳单位一旦分叉就是稳定的 400002。
import (
	"context"
	"encoding/json"
	"fmt"
)

// TaskCompletePath 客户端任务完成接口（通用入口，非签到专用）。
const TaskCompletePath = "/autoclaw-proxy/proxy/autoclaw-task-complete"

// TaskListPath 任务列表（只在需要归因时才拉，正常路径不多花一次往返）。
const TaskListPath = "/autoclaw-proxy/proxy/autoclaw-task-list"

// DailySigninTaskID 每日签到的 task_id。
const DailySigninTaskID = "daily_signin"

// CheckinResult 签到结果（形状与 extstore.CheckinResult 对齐）。
type CheckinResult struct {
	Kind    string // claimed | already-claimed | failed
	Credit  float64
	Message string
}

// CheckinDaily 执行每日签到。
//
// `Kind` 的语义是「**本次真的领到了吗**」，不是「接口通了吗」——
// 今天已领过时是 `already-claimed`，避免重复领取被当成成功上报。
//
// 包级函数（本包没有 Client 类型，HTTP 走共享的 httpClient），
// 与 SendCode / LoginWithCode 同形。
func CheckinDaily(ctx context.Context, cred *Credential) *CheckinResult {
	if cred == nil || cred.Token == "" {
		return &CheckinResult{Kind: "failed", Message: "凭据里没有 token，需重新登录"}
	}
	body, _ := json.Marshal(map[string]string{"task_id": DailySigninTaskID})
	raw, err := postJSON(ctx, cred.Region.UserAPI()+TaskCompletePath, body, userAPIHeaders(cred.Token))
	if err != nil {
		return &CheckinResult{Kind: "failed", Message: "签到失败：" + err.Error()}
	}

	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AlreadyCompleted bool  `json:"already_completed"`
			Success          bool  `json:"success"`
			RewardPoints     int64 `json:"reward_points"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return &CheckinResult{Kind: "failed", Message: "签到回执解析失败"}
	}
	if doc.Code != 0 {
		return &CheckinResult{Kind: "failed", Message: describeLoginError(doc.Code, doc.Msg)}
	}

	d := doc.Data
	// 判「本次真领到」：HTTP 恒 200，只有 reward_points 或 success 才算数
	claimed := d.RewardPoints > 0 || (d.Success && !d.AlreadyCompleted)
	if claimed {
		msg := "签到成功"
		if d.RewardPoints > 0 {
			msg = fmt.Sprintf("签到成功，获得 %d 积分", d.RewardPoints)
		}
		return &CheckinResult{Kind: "claimed", Credit: float64(d.RewardPoints), Message: msg}
	}
	if d.AlreadyCompleted {
		return &CheckinResult{Kind: "already-claimed", Message: "今日已签到"}
	}
	return &CheckinResult{Kind: "failed", Message: "签到未生效，稍后重试"}
}

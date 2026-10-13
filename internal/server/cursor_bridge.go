// Package server: cursor_bridge.go — Cursor 通道（magpie 二批收尾）。
//
// 模型名带 "cursor:" 前缀时走本路径。
//
// 协议事实（magpie-plugins packages/cursor/index.mjs，HEAD f573cfd）：
//   - 上游对话是 **Connect bidi-stream**（AgentService/Run over HTTP/2；
//     HTTP/2 不可用时 RunSSE + BidiAppend over HTTP/1.1），消息体是
//     **protobuf 帧**（fork 自带 pb 编解码 L1005+），且 Cursor 把对话历史
//     存在**客户端**（每条 AI SDK 消息一个 sha256 命名 blob，服务端按需
//     向客户端索要）——意味着完整移植需要：protobuf 编解码 + blob 存储 +
//     MCP 工具回调（CallDynamicTool）+ Connect 帧层，四件套。
//   - fork 自己就有 2346 行（其中 ~500 行是 pb 编解码与 Connect 帧），
//     Go 侧等价实现预估 800-1200 行。
//
// 本文件的定位（Chief 口径：诚实收口，不假装接通）：
//  1. 账号选择/健康过滤/错误分类与 lapsed 标注（已接线，与 ZCode 同构）；
//  2. 上游连通性探测（unary GetMe/GetPlanInfo，fork L205-214）——这条是
//     普通 Connect unary JSON，**不走 protobuf 帧**，可直接验证凭据有效；
//  3. 对话路径返回明确错误（cursor_protocol_unwired），提示协议未接线，
//     不静默失败、不返回假回复。
//
// 后续批次要做对话时：protobuf 帧 + blob + MCP 回调 + Connect 帧层，
// 见 docs/KNOWN-LIMITATIONS。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/cursor"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// cursorModelPrefix Cursor 路由前缀。
const cursorModelPrefix = "cursor:"

// isCursorModel 判断裸模型名是否请求 Cursor 通道。
func isCursorModel(bare string) bool { return strings.HasPrefix(bare, cursorModelPrefix) }

// cursorUnaryPath Connect unary 调用路径（fork L207-208：RPC 全限定名）。
const (
	cursorGetMePath       = "aiserver.v1.DashboardService/GetMe"
	cursorGetPlanInfoPath = "aiserver.v1.DashboardService/GetPlanInfo"
)

// CursorChatStream Cursor 通道入口。
//
// 当前实现：账号解析 + unary 连通性探测 + 对话路径如实报"协议未接线"。
// 返回 false = 无可用账号（调用方给 no_cursor_account 详情）。
func (h *Handler) CursorChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	h.lastCursorErr = ""
	if h.cfg.ExtAccounts == nil {
		return false
	}
	model := strings.TrimPrefix(bareModel, cursorModelPrefix)

	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PCursor && !a.Disabled && a.SignInState != "expired" {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		h.lastCursorErr = "没有可用的 Cursor 账号（面板-添加账号-外部平台-Cursor）"
		return false
	}

	lastErr := ""
	for _, a := range accounts {
		var cred cursor.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if cred.AccessToken == "" {
			lastErr = "凭据里没有 access_token"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		// "耗尽即弃"（Chief 口径 2 / fork L264-267）：到期即标 lapsed，
		// **不套 authcore 的 refresh-lead**（Cursor 的 refresh token
		// 不能换新 access token，只能重登）。
		if cred.Lapsed(time.Now()) {
			lastErr = "Cursor 登录已过期（refresh 不可用），请重新授权"
			_ = h.extManager.MarkSignInExpired(a.Provider, a.ID, lastErr)
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		// unary 连通性探测（fork whoIs L205-214）：既验证凭据，也拿
		// 得到 email/plan 充实 detail。这一步是普通 Connect unary JSON，
		// 不涉及 protobuf 帧。
		info, perr := cursorProbe(r, cred.AccessToken)
		if perr != nil {
			// 网络故障保留登录态（与 zcode 桥同口径）。
			lastErr = perr.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if info.email == "" {
			lastErr = "上游未返回账号身份（凭据可能已失效）"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		h.noteChat(a.Provider, a.ID, nil)

		// 对话路径：如实报协议未接线（不静默、不假造回复）。
		detail := fmt.Sprintf("Cursor 账号 %s 可用，但 Connect bidi-stream 对话协议尚未接线（%s）", info.email, model)
		writeOpenAIError(w, http.StatusNotImplemented, "cursor_protocol_unwired", detail)
		log.Printf("cursor-bridge: acct=%s 对话请求未接线（协议待移植）model=%s", a.ID, model)
		return true
	}
	h.lastCursorErr = lastErr
	return false
}

// cursorProbeResult unary 探测结果。
type cursorProbeResult struct {
	email string
	plan  string
}

// cursorProbe 并发拉 GetMe + GetPlanInfo（fork whoIs：allSettled 语义——
// 单个失败不拖垮另一个）。超时 15s（fork unary AbortSignal.timeout(15_000)）。
func cursorProbe(r *http.Request, token string) (cursorProbeResult, error) {
	type res struct {
		path string
		raw  []byte
		err  error
	}
	client := &http.Client{Timeout: 15 * time.Second}
	ch := make(chan res, 2)
	for _, p := range []string{cursorGetMePath, cursorGetPlanInfoPath} {
		go func(path string) {
			req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
				cursor.API+"/"+path, strings.NewReader("{}"))
			if err != nil {
				ch <- res{path: path, err: err}
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Connect-Protocol-Version", "1")
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("x-cursor-client-type", "cli")
			resp, err := client.Do(req)
			if err != nil {
				ch <- res{path: path, err: err}
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				ch <- res{path: path, err: fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)}
				return
			}
			ch <- res{path: path, raw: raw}
		}(p)
	}
	out := cursorProbeResult{}
	for i := 0; i < 2; i++ {
		got := <-ch
		if got.err != nil {
			continue // allSettled：单个失败不抛
		}
		var doc struct {
			Email    string `json:"email"`
			PlanInfo struct {
				PlanName string `json:"planName"`
			} `json:"planInfo"`
		}
		if json.Unmarshal(got.raw, &doc) == nil {
			if e := strings.TrimSpace(doc.Email); e != "" {
				out.email = e
			}
			if p := strings.TrimSpace(doc.PlanInfo.PlanName); p != "" {
				out.plan = p
			}
		}
	}
	return out, nil
}

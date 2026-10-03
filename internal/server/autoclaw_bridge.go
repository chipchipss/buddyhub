// Package server: autoclaw_bridge.go — AutoClaw（智谱 autoglm）直连路由。
//
// 模型名带 "autoclaw:" 前缀时走本路径（`autoclaw:glm-5.3` 等）。
// 上游是 OpenAI 协议，网关不做翻译，原样透传。
//
// 与其它通道的差异：**账号分属两个地区**（国内 / 国际，两套域名）。地区是凭据的
// 属性——同一个账号只属于一个站点——所以选号时按各自凭据里的地区去打对应域名，
// 而不是在路由层区分。
package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/autoclaw"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// autoclawChatStream 转发到 AutoClaw。返回 false = 无可用账号。
func (h *Handler) autoclawChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PAutoClaw && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	lastErr := ""
	for _, a := range accounts {
		var cred autoclaw.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if cred.NeedsRefresh() {
			fresh, err := h.refreshAutoClawCred(r.Context(), a.ID, &cred)
			if err != nil {
				lastErr = err.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				log.Printf("autoclaw-bridge: %s 续期失败: %v", a.ID, err)
				continue
			}
			cred = *fresh
		}

		// 上游 2026-09-22 起有 system 提示词闸门：system 必须以 OpenClaw 身份句
		// 开头且带 ## Tooling 段，否则 **406 空响应体**（客户端发的通用提示词
		// 正好命中）。出站前规范化：前置身份句 + 改写外来身份句，见 prompt.go。
		outBody := autoclaw.NormalizePrompt(body)
		if debugOut := os.Getenv("AUTOCLAW_DEBUG_BODY"); debugOut != "" {
			log.Printf("autoclaw-bridge: 出站体（前 600 字节）: %s", string(outBody[:min(600, len(outBody))]))
		}

		// 上游有两个模型标识（autoclaw.Chat 注释）：
		//   X-Request-Model = 带前缀路由 ID（zai_glm-5.3-flash）
		//   body.model      = 剥前缀模型 ID（glm-5.3-flash）
		// bareModel 已剥掉 autoclaw: 前缀、剩路由 ID（zai_glm-5.3-flash），
		// 直接作 X-Request-Model；body.model 用再剥一层的裸名。
		bodyModel := autoclaw.StripRoutePrefix(bareModel)
		outBody = rewriteModel(outBody, bodyModel)

		resp, err := autoclaw.Chat(r.Context(), &cred, bareModel, outBody)
		if err != nil {
			lastErr = err.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("autoclaw-bridge: %s chat 失败: %v", a.ID, err)
			continue
		}
		// 沙箱 relay 路径会回填 cred.SandboxID/Endpoint/EndTimestamp（首次 EnsureSandbox
		// 申请到的沙箱）——回写账号表让下一轮命中缓存，避免每次对话都打 sandbox/list。
		if cred.SandboxID != "" && h.extManager != nil {
			if raw, merr := json.Marshal(&cred); merr == nil {
				h.extManager.ReplaceCred(extstore.PAutoClaw, a.ID, raw)
			}
		}

		// 401 = 令牌被提前失效，强制续期重试一次
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			fresh, rerr := h.refreshAutoClawCred(r.Context(), a.ID, &cred)
			if rerr != nil {
				lastErr = rerr.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				continue
			}
			cred = *fresh
			resp, err = autoclaw.Chat(r.Context(), &cred, bareModel, outBody)
			if err != nil {
				lastErr = err.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				continue
			}
		}

		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			lastErr = "上游 HTTP " + strconv.Itoa(resp.StatusCode) + "：" + shorten(string(raw), 200)
			// 406 空响应体的归因（实测排查结论，2026-10-02 → 2026-10-03 更新）：
			// 直连 /autoclaw-proxy/… 路径的 406 是**模型校验后的权限层**（JWT power=0
			// 账号即便请求形状完全正确也恒 406）。autoclaw.Chat 现在**优先走沙箱
			// relay**（1.18.x 官方客户端路径，zai-org/feedback #804 确认旧直连 406、
			// relay 可用），只有沙箱也失败才落到这条兜底。见到这里的 406 = 沙箱与
			// 直连都不通，多半是账号无对话权限。
			if resp.StatusCode == http.StatusNotAcceptable && strings.TrimSpace(string(raw)) == "" {
				lastErr += "（上游 406 空响应：沙箱 relay 与直连均被拒——该账号当前无对话权限；可到 AutoClaw 客户端确认账号可用后重试，或换有权限的账号）"
			}
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("autoclaw-bridge: %s 上游 %d: %s", a.ID, resp.StatusCode, shorten(string(raw), 200))
			continue
		}

		log.Printf("autoclaw-bridge: acct=%s region=%s model=%s 建流成功",
			a.ID, autoclaw.ParseRegion(string(cred.Region)).Label(), bareModel)
		h.passThroughAutoClaw(w, resp, h.clientWantsStream(body))
		return true
	}
	h.lastAutoClawErr = lastErr
	return false
}

// refreshAutoClawCred 单飞续期 + 回写账号表。
//
// AutoClaw 服务端每次刷新会**轮换 refresh_token**：并发刷新会互相作废并把人
// 踢下线，故同账号的续期合并成一次。
func (h *Handler) refreshAutoClawCred(ctx context.Context, accountID string, cred *autoclaw.Credential) (*autoclaw.Credential, error) {
	fresh, err := h.autoclawFlights.Do(accountID, func() (*autoclaw.Credential, error) {
		return autoclaw.Refresh(ctx, cred)
	})
	if err == nil && h.extManager != nil {
		if raw, merr := json.Marshal(fresh); merr == nil {
			h.extManager.ReplaceCred(extstore.PAutoClaw, accountID, raw)
		}
	}
	return fresh, err
}

// passThroughAutoClaw 原样透传上游响应（上游即 OpenAI 格式，无需翻译）。
func (h *Handler) passThroughAutoClaw(w http.ResponseWriter, resp *http.Response, wantStream bool) {
	defer resp.Body.Close()

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		h.proxyQoderSSE(w, resp.Body)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_read", err.Error())
		return
	}
	if !wantStream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(append([]byte("data: "), raw...), []byte("\n\ndata: [DONE]\n\n")...))
	flusher.Flush()
}

/* ── 模型目录（实时拉取 + 10 分钟缓存） ───────────────────────── */

var (
	autoclawCatMu   sync.Mutex
	autoclawCatAt   time.Time
	autoclawCatMods []autoclaw.Model
)

// autoclawCatalog 拉取模型目录（按地区各取一次，合并去重）。
//
// ⚠️ 目录请求必须带 `X-Version`——上游按它做**版本门控**，不带时只下发 3–4 条
// 模型，看起来像账号没有这些模型。
func (h *Handler) autoclawCatalog() []autoclaw.Model {
	if h.cfg.ExtAccounts == nil {
		return nil
	}
	autoclawCatMu.Lock()
	if time.Since(autoclawCatAt) < 10*time.Minute && autoclawCatMods != nil {
		mods := autoclawCatMods
		autoclawCatMu.Unlock()
		return mods
	}
	autoclawCatMu.Unlock()

	// 每个地区取一个可用账号即可（目录与账号无关）
	byRegion := map[autoclaw.Region]string{}
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider != extstore.PAutoClaw || a.Disabled {
			continue
		}
		var cred autoclaw.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			continue
		}
		r := autoclaw.ParseRegion(string(cred.Region))
		if _, ok := byRegion[r]; !ok {
			byRegion[r] = cred.Token
		}
	}
	if len(byRegion) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	seen := map[string]bool{}
	var mods []autoclaw.Model
	for region, token := range byRegion {
		got, err := autoclaw.ListModels(ctx, region, token)
		if err != nil {
			log.Printf("autoclaw-bridge: 拉取 %s 模型目录失败: %v", region.Label(), err)
			continue
		}
		for _, m := range got {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			mods = append(mods, m)
		}
	}
	if len(mods) == 0 {
		autoclawCatMu.Lock()
		prev := autoclawCatMods
		autoclawCatMu.Unlock()
		return prev
	}
	autoclawCatMu.Lock()
	autoclawCatMods, autoclawCatAt = mods, time.Now()
	autoclawCatMu.Unlock()
	return mods
}

// autoclawModelPrefix AutoClaw 路由前缀。
const autoclawModelPrefix = "autoclaw:"

// isAutoClawModel 判断裸模型名是否请求 AutoClaw 通道。
func isAutoClawModel(bare string) bool { return strings.HasPrefix(bare, autoclawModelPrefix) }

// min 取小值（调试日志用）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

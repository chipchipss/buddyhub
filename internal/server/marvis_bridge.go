// Package server: marvis_bridge.go — 腾讯 Marvis（马维斯）直连路由。
//
// 模型名带 "marvis:" 前缀时走本路径。上游是**标准 OpenAI 协议**
// （含 SSE / tool_calls），与 raccoon 同形：body 原样透传、SSE 直通、
// 只回写响应帧里的 model 名。
//
// 与 raccoon 的两处差异（见 extprovider/marvis 模块头）：
//   - 鉴权是 Ual-Access-* 头组（uskey 每请求新生成）
//   - **严禁压测**：上游按账号做自适应风控，密集突发触发 403 {4100404} 冷却
package server

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/chipchipss/buddyhub/internal/extprovider/marvis"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// marvisChatStream 转发到 Marvis。返回 false = 无可用账号。
func (h *Handler) marvisChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PMarvis && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	// body 原样透传（上游就是 OpenAI 形状）；model 字段由分发层已剥前缀。
	outBody := body

	lastErr := ""
	for _, a := range accounts {
		var cred marvis.Credential
		if json.Unmarshal(a.Cred, &cred) != nil || cred.AccessToken == "" {
			lastErr = "凭据解析失败或缺少 access_token"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		resp, err := marvis.Chat(r.Context(), &cred, outBody)
		if err != nil {
			lastErr = err.Error()
			log.Printf("marvis-bridge: %s chat 失败: %v", a.ID, err)
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			lastErr = "上游 HTTP " + itoa(resp.StatusCode) + "：" + shorten(string(raw), 200)
			// 风控冷却（403 4100404）与 token 失效（401 4100403）都要说明，
			// 用户对着裸状态码没法行动。
			if resp.StatusCode == http.StatusForbidden && strings.Contains(string(raw), "4100404") {
				lastErr += "（Marvis 账号级风控：请求过密触发冷却，上游会自动恢复——切勿压测，保持正常节奏）"
			} else if resp.StatusCode == http.StatusUnauthorized {
				lastErr += "（token 失效或锁定：需从 Marvis 客户端重新抓包获取 mv_ token）"
			}
			log.Printf("marvis-bridge: %s 上游 %d: %s", a.ID, resp.StatusCode, shorten(string(raw), 200))
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}

		log.Printf("marvis-bridge: acct=%s model=%s 建流成功", a.ID, bareModel)
		h.forwardMarvis(w, resp, body, bareModel)
		h.noteChat(a.Provider, a.ID, nil)
		return true
	}
	h.lastMarvisErr = lastErr
	return false
}

// forwardMarvis 流式逐帧（回写 model）/ 非流式分形态处理。
// 与 raccoon 同构（上游是 OpenAI 兼容直通）。
func (h *Handler) forwardMarvis(w http.ResponseWriter, resp *http.Response, reqBody []byte, model string) {
	defer resp.Body.Close()

	if !h.clientWantsStream(reqBody) {
		// 上游两种形态都要接：stream:false 可能直接回纯 JSON（先探首字节），
		// 也可能回 SSE（本地聚合）。
		br := bufio.NewReader(resp.Body)
		if first, err := br.Peek(1); err == nil && len(first) > 0 && first[0] == '{' {
			raw, _ := io.ReadAll(br)
			out := rewriteJSONObject(string(raw), "model", model)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, out)
			return
		}
		doc, err := upstream.Aggregate(br)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		if doc != nil {
			if _, ok := doc["model"].(string); ok {
				doc["model"] = model
			}
		}
		writeJSON(w, http.StatusOK, doc)
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

	// sc.Text() 而不是 sc.Bytes()：后者是复用缓冲
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(line[len("data:"):])
			if out := rewriteJSONObject(payload, "model", model); out != payload {
				line = "data: " + out
			}
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return // 客户端断开
		}
		flusher.Flush()
	}
}

/* ── 模型目录（静态清单） ─────────────────────────────────────── */

var (
	marvisCatMu   sync.Mutex
	marvisCatList []marvis.Model
)

// marvisCatalog 静态模型清单（Marvis 云端不暴露注册表）。
func marvisCatalog() []marvis.Model {
	marvisCatMu.Lock()
	defer marvisCatMu.Unlock()
	if marvisCatList == nil {
		marvisCatList = marvis.Models()
	}
	return marvisCatList
}

// marvisModelPrefix Marvis 路由前缀。
const marvisModelPrefix = "marvis:"

// isMarvisModel 判断裸模型名是否请求 Marvis 通道。
func isMarvisModel(bare string) bool { return strings.HasPrefix(bare, marvisModelPrefix) }

// Package server: qclaw_bridge.go — QClaw（腾讯）直连路由。
//
// 模型名带 "qclaw:" 前缀时走本路径。上游 AIZone 域就是 OpenAI 协议，
// 网关不做翻译，原样透传。
package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/qclaw"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// qclawChatStream 转发到 QClaw。返回 false = 无可用账号。
func (h *Handler) qclawChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PQClaw && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	lastErr := ""
	for _, a := range accounts {
		var cred qclaw.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			continue
		}
		// QClaw 的对话走建出来的 sk key（长期有效），JWT 只用于业务域；
		// 没有 sk key 说明当初建 key 那步没成，直接跳过。
		if cred.APIKey == "" {
			lastErr = "账号缺少对话用的 sk key，请重新登录"
			continue
		}

		resp, err := qclaw.Chat(r.Context(), &cred, body)
		if err != nil {
			lastErr = err.Error()
			log.Printf("qclaw-bridge: %s chat 失败: %v", a.ID, err)
			continue
		}

		// 401 = key 被失效，业务域能救则救一次
		if resp.StatusCode == http.StatusUnauthorized && cred.JWT != "" {
			resp.Body.Close()
			if fresh, rerr := h.refreshQClawCred(r.Context(), a.ID, &cred); rerr == nil {
				cred = *fresh
				resp, err = qclaw.Chat(r.Context(), &cred, body)
				if err != nil {
					lastErr = err.Error()
					continue
				}
			}
		}

		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			lastErr = "上游 HTTP " + strconv.Itoa(resp.StatusCode) + "：" + shorten(string(raw), 200)
			log.Printf("qclaw-bridge: %s 上游 %d: %s", a.ID, resp.StatusCode, shorten(string(raw), 200))
			continue
		}

		log.Printf("qclaw-bridge: acct=%s model=%s 建流成功", a.ID, bareModel)
		h.passThroughQClaw(w, resp, h.clientWantsStream(body))
		return true
	}
	h.lastQClawErr = lastErr
	return false
}

// refreshQClawCred 用业务域重新建一把 sk key（JPRX 的 X-New-Token 会轮换 JWT）。
//
// 单飞：JWT 会轮换，并发刷新会互相作废。
func (h *Handler) refreshQClawCred(ctx context.Context, accountID string, cred *qclaw.Credential) (*qclaw.Credential, error) {
	fresh, err := h.qclawFlights.Do(accountID, func() (*qclaw.Credential, error) {
		return qclaw.ReissueKey(ctx, cred)
	})
	if err == nil && h.extManager != nil {
		if raw, merr := json.Marshal(fresh); merr == nil {
			h.extManager.ReplaceCred(extstore.PQClaw, accountID, raw)
		}
	}
	return fresh, err
}

// passThroughQClaw 原样透传上游响应（上游即 OpenAI 格式，无需翻译）。
func (h *Handler) passThroughQClaw(w http.ResponseWriter, resp *http.Response, wantStream bool) {
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
	qclawCatMu   sync.Mutex
	qclawCatAt   time.Time
	qclawCatMods []qclaw.Model
)

// qclawCatalog 拉模型列表（JPRX 4320）；拿不到时回落到静态名单。
func (h *Handler) qclawCatalog() []qclaw.Model {
	if h.cfg.ExtAccounts == nil {
		return nil
	}
	qclawCatMu.Lock()
	if time.Since(qclawCatAt) < 10*time.Minute && qclawCatMods != nil {
		mods := qclawCatMods
		qclawCatMu.Unlock()
		return mods
	}
	qclawCatMu.Unlock()

	var cred *qclaw.Credential
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider != extstore.PQClaw || a.Disabled {
			continue
		}
		var c qclaw.Credential
		if json.Unmarshal(a.Cred, &c) == nil {
			cred = &c
			break
		}
	}
	if cred == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mods, err := qclaw.ListModels(ctx, cred)
	if err != nil {
		log.Printf("qclaw-bridge: 拉取模型列表失败（回落到静态名单）: %v", err)
	}
	if len(mods) == 0 {
		return nil
	}
	qclawCatMu.Lock()
	qclawCatMods, qclawCatAt = mods, time.Now()
	qclawCatMu.Unlock()
	return mods
}

// qclawModelPrefix QClaw 路由前缀。
const qclawModelPrefix = "qclaw:"

// isQClawModel 判断裸模型名是否请求 QClaw 通道。
func isQClawModel(bare string) bool { return strings.HasPrefix(bare, qclawModelPrefix) }

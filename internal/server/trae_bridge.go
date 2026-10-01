// Package server: trae_bridge.go — Trae（字节 SOLO）直连路由。
//
// 模型名带 "trae:" 前缀时走本路径。上游是 SOLO 信封：
// **出站请求体白名单重建**（多带字段会被拒），**响应是自定义事件流**（不是标准
// OpenAI chunk，且不发 [DONE]）——两侧都要转换，不能像 Cline/QClaw 那样透传。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/trae"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// traeChatStream 转发到 Trae。返回 false = 无可用账号。
func (h *Handler) traeChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PTrae && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	// 出站体：白名单重建 + 四处 SOLO 变形
	outBody, err := trae.PrepareBody(body, bareModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体解析失败："+err.Error())
		return true
	}

	lastErr := ""
	for _, a := range accounts {
		var cred trae.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if cred.NeedsRefresh() {
			fresh, rerr := h.refreshTraeCred(r.Context(), a.ID, &cred)
			if rerr != nil {
				lastErr = rerr.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				log.Printf("trae-bridge: %s 续期失败: %v", a.ID, rerr)
				continue
			}
			cred = *fresh
		}

		// 上游只支持流式；非流式由网关本地聚合
		resp, cerr := trae.Chat(r.Context(), &cred, outBody, true)
		if cerr != nil {
			lastErr = cerr.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("trae-bridge: %s chat 失败: %v", a.ID, cerr)
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			fresh, rerr := h.refreshTraeCred(r.Context(), a.ID, &cred)
			if rerr == nil {
				cred = *fresh
				resp, cerr = trae.Chat(r.Context(), &cred, outBody, true)
			}
			if cerr != nil {
				lastErr = cerr.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				continue
			}
		}
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			lastErr = "上游 HTTP " + strconv.Itoa(resp.StatusCode) + "：" + shorten(string(raw), 200)
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("trae-bridge: %s 上游 %d: %s", a.ID, resp.StatusCode, shorten(string(raw), 200))
			continue
		}

		log.Printf("trae-bridge: acct=%s model=%s 建流成功", a.ID, bareModel)
		h.translateTrae(w, resp, body, bareModel)
		return true
	}
	h.lastTraeErr = lastErr
	return false
}

// refreshTraeCred 单飞续期 + 回写账号表。
func (h *Handler) refreshTraeCred(ctx context.Context, accountID string, cred *trae.Credential) (*trae.Credential, error) {
	fresh, err := h.traeFlights.Do(accountID, func() (*trae.Credential, error) {
		return trae.Refresh(ctx, cred)
	})
	if err == nil && h.extManager != nil {
		if raw, merr := json.Marshal(fresh); merr == nil {
			h.extManager.ReplaceCred(extstore.PTrae, accountID, raw)
		}
	}
	return fresh, err
}

// translateTrae 把 SOLO 事件流转成客户端要的形态。
//
// 上游**不发 [DONE]**、业务错误藏在 200 的流里、usage 欠在下一帧——
// 这三件事都由 trae.SoloTranslator 处理（见那边的注释）。
func (h *Handler) translateTrae(w http.ResponseWriter, resp *http.Response, reqBody []byte, model string) {
	defer resp.Body.Close()

	if !h.clientWantsStream(reqBody) {
		raw, err := trae.AggregateSolo(resp.Body, newChunkID(), time.Now().Unix(), model)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
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

	tr := trae.NewSoloTranslator(newChunkID(), time.Now().Unix(), model)
	write := func(frames [][]byte) bool {
		for _, f := range frames {
			if _, err := w.Write(f); err != nil {
				return false
			}
		}
		flusher.Flush()
		return true
	}
	_ = trae.ScanSoloSSE(resp.Body, func(event, data string) {
		write(tr.Translate(trae.ParseSoloEvent(event, data)))
	})
	// 上游没发 done 也要补 [DONE]，否则客户端永远等最后一片
	write(tr.Finish())
}

// newChunkID 生成 chat.completion 的 id（每次请求不同）。
func newChunkID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

/* ── 模型目录（实时拉取 + 10 分钟缓存） ───────────────────────── */

var (
	traeCatMu   sync.Mutex
	traeCatAt   time.Time
	traeCatMods []trae.Model
)

func (h *Handler) traeCatalog() []trae.Model {
	if h.cfg.ExtAccounts == nil {
		return nil
	}
	traeCatMu.Lock()
	if time.Since(traeCatAt) < 10*time.Minute && traeCatMods != nil {
		mods := traeCatMods
		traeCatMu.Unlock()
		return mods
	}
	traeCatMu.Unlock()

	var cred *trae.Credential
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider != extstore.PTrae || a.Disabled {
			continue
		}
		var c trae.Credential
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
	mods, err := trae.ListModels(ctx, cred)
	if err != nil {
		log.Printf("trae-bridge: 拉取模型目录失败: %v", err)
		traeCatMu.Lock()
		prev := traeCatMods
		traeCatMu.Unlock()
		return prev
	}
	if len(mods) == 0 {
		return nil
	}
	traeCatMu.Lock()
	traeCatMods, traeCatAt = mods, time.Now()
	traeCatMu.Unlock()
	return mods
}

// traeModelPrefix Trae 路由前缀。
const traeModelPrefix = "trae:"

// isTraeModel 判断裸模型名是否请求 Trae 通道。
func isTraeModel(bare string) bool { return strings.HasPrefix(bare, traeModelPrefix) }

// Package server: accio_bridge.go — Accio（阿里）直连路由。
//
// 模型名带 "accio:" 前缀时走本路径。上游是 ADK（Gemini 风格）信封：
// **出站体与响应流两侧都要完整翻译**，不能透传。
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

	"github.com/chipchipss/buddyhub/internal/extprovider/accio"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// accioChatStream 转发到 Accio。返回 false = 无可用账号。
func (h *Handler) accioChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PAccio && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	lastErr := ""
	for _, a := range accounts {
		var cred accio.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			continue
		}
		if cred.NeedsRefresh() {
			fresh, rerr := h.refreshAccioCred(r.Context(), a.ID, &cred)
			if rerr != nil {
				lastErr = rerr.Error()
				h.noteChat(a.Provider, a.ID, errOf(lastErr))
				log.Printf("accio-bridge: %s 续期失败: %v", a.ID, rerr)
				continue
			}
			cred = *fresh
		}

		// request_id 同时进 body 与 URL 的 sg_k 签名，必须同一个
		reqID := accio.NewRequestID()
		outBody, perr := accio.BuildUpstreamBody(body, bareModel, cred.AccessToken, reqID, cred.DeviceID)
		if perr != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", perr.Error())
			return true
		}

		resp, cerr := accio.Chat(r.Context(), &cred, outBody, reqID)
		if cerr != nil {
			lastErr = cerr.Error()
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("accio-bridge: %s chat 失败: %v", a.ID, cerr)
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			fresh, rerr := h.refreshAccioCred(r.Context(), a.ID, &cred)
			if rerr == nil {
				cred = *fresh
				reqID = accio.NewRequestID()
				outBody, _ = accio.BuildUpstreamBody(body, bareModel, cred.AccessToken, reqID, cred.DeviceID)
				resp, cerr = accio.Chat(r.Context(), &cred, outBody, reqID)
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
			log.Printf("accio-bridge: %s 上游 %d: %s", a.ID, resp.StatusCode, shorten(string(raw), 200))
			continue
		}

		if reason := accio.InterceptReason(resp); reason != "" {
			// 风控/拦截页是 HTTP 200 的 HTML，SSE 侧看不出任何异常——不在写响应头
			// 之前拦下来，客户端就会收到「200 空正文」，账号被拦的原因永远浮不出来。
			resp.Body.Close()
			lastErr = reason
			h.noteChat(a.Provider, a.ID, errOf(lastErr))
			log.Printf("accio-bridge: %s 上游 200 干扰: %s", a.ID, reason)
			// 拦截按设备/IP 判定，换下一个账号只会多打一次上游，直接收手。
			break
		}

		log.Printf("accio-bridge: acct=%s region=%s model=%s 建流成功",
			a.ID, accio.ParseRegion(string(cred.Region)).Label(), bareModel)
		// translateAccio 一旦开始写响应就回不去下一个账号了（头已发），
		// 所以流内错误只记账、不换号重试。
		if terr := h.translateAccio(w, resp, body, bareModel); terr != nil {
			h.noteChat(a.Provider, a.ID, terr)
			h.lastAccioErr = terr.Error()
			return true
		}
		h.noteChat(a.Provider, a.ID, nil)
		return true
	}
	h.lastAccioErr = lastErr
	return false
}

// refreshAccioCred 单飞续期 + 回写账号表。
func (h *Handler) refreshAccioCred(ctx context.Context, accountID string, cred *accio.Credential) (*accio.Credential, error) {
	fresh, err := h.accioFlights.Do(accountID, func() (*accio.Credential, error) {
		return accio.Refresh(ctx, cred)
	})
	if err == nil && h.extManager != nil {
		if raw, merr := json.Marshal(fresh); merr == nil {
			h.extManager.ReplaceCred(extstore.PAccio, accountID, raw)
		}
	}
	return fresh, err
}

// translateAccio 把 ADK 帧流转成客户端要的形态。返回非 nil = 这一号这单已失败
// （响应多半已经写出，调用方只记账，不再换号重试）。
func (h *Handler) translateAccio(w http.ResponseWriter, resp *http.Response, reqBody []byte, model string) error {
	defer resp.Body.Close()

	if !h.clientWantsStream(reqBody) {
		raw, err := accio.Aggregate(resp.Body, newChunkID(), time.Now().Unix(), model)
		if err != nil {
			// 上游 200 但流内藏业务错误（invalid params / 模型名无效等）——
			// 此前这里不记日志、不上报退避，排障时对着空日志只能瞎猜。
			// 补上：账号级 lastErr + 退避 + 一行日志。
			log.Printf("accio-bridge: model=%s 流内错误: %v", model, err)
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
			return err
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return nil
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return errOf("服务器不支持流式响应 Flush")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	tr := accio.NewTranslator(newChunkID(), time.Now().Unix(), model)
	write := func(s string) {
		if s == "" {
			return
		}
		if _, err := io.WriteString(w, s); err != nil {
			return
		}
		flusher.Flush()
	}
	sawFrame := false
	_ = accio.ScanSSEAll(resp.Body, func(data string) {
		sawFrame = true
		write(tr.Translate(accio.ParseFrame(data)))
	}, nil)
	if !sawFrame {
		// 头已经发出，退不成 HTTP 错误，但至少要让客户端看见原因而不是等来一段静默。
		err := errOf("上游 200 但没有收到任何 ADK 帧（响应被拦截或形状不符）")
		write(accio.SseFrame(map[string]any{"error": map[string]any{
			"message": err.Error(), "type": "upstream_error",
		}}))
		write(tr.Finish())
		return err
	}
	// 上游没发 turnComplete 也要补 [DONE]
	write(tr.Finish())
	return nil
}

/* ── 模型目录（实时拉取 + 10 分钟缓存） ───────────────────────── */

var (
	accioCatMu   sync.Mutex
	accioCatAt   time.Time
	accioCatMods []accio.Model
)

func (h *Handler) accioCatalog() []accio.Model {
	if h.cfg.ExtAccounts == nil {
		return nil
	}
	accioCatMu.Lock()
	if time.Since(accioCatAt) < 10*time.Minute && accioCatMods != nil {
		mods := accioCatMods
		accioCatMu.Unlock()
		return mods
	}
	accioCatMu.Unlock()

	var cred *accio.Credential
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider != extstore.PAccio || a.Disabled {
			continue
		}
		var c accio.Credential
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
	mods, err := accio.ListModels(ctx, cred)
	if err != nil {
		log.Printf("accio-bridge: 拉取模型目录失败: %v", err)
		accioCatMu.Lock()
		prev := accioCatMods
		accioCatMu.Unlock()
		return prev
	}
	if len(mods) == 0 {
		return nil
	}
	accioCatMu.Lock()
	accioCatMods, accioCatAt = mods, time.Now()
	accioCatMu.Unlock()
	return mods
}

// accioModelPrefix Accio 路由前缀。
const accioModelPrefix = "accio:"

// isAccioModel 判断裸模型名是否请求 Accio 通道。
func isAccioModel(bare string) bool { return strings.HasPrefix(bare, accioModelPrefix) }

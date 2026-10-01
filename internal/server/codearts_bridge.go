// Package server: codearts_bridge.go — 华为云 CodeArts 直连路由。
//
// 模型名带 "codearts:" 前缀时走本路径。端点与积分/签到同域
// （SnapEngineURL + /api/v2/chat/completions），但**签名口径不同**：
// 对话那套不签 host（见 extprovider/codearts/chat.go 模块头）。
//
// 上游响应是 SSE，帧形为标准 OpenAI chunk，因此走直通 + model 回写；
// **已知局限**：参考实现还做了流内错误信封识别（stream_fault.rs——上游会把
// 业务错误藏在 200 的流里），本桥未移植，那种情况会被当成正文透给客户端。
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/codearts"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// codeartsChatStream 转发到 CodeArts。返回 false = 无可用账号。
func (h *Handler) codeartsChatStream(w http.ResponseWriter, r *http.Request, body []byte, bareModel string) bool {
	if h.cfg.ExtAccounts == nil {
		return false
	}
	var accounts []*extstore.ExtAccount
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider == extstore.PCodeArts && !a.Disabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return false
	}

	cli := codearts.New()
	lastErr := ""
	for _, a := range accounts {
		var cred codearts.Credential
		if json.Unmarshal(a.Cred, &cred) != nil {
			lastErr = "凭据解析失败"
			continue
		}
		if cred.AccessKeyID == "" || cred.SecretAccessKey == "" {
			lastErr = "凭据缺少 AK/SK"
			continue
		}

		stream := h.clientWantsStream(body)
		resp, cerr := cli.Chat(r.Context(), &cred, bareModel, body, stream, false)
		if cerr != nil {
			lastErr = cerr.Error()
			log.Printf("codearts-bridge: %s chat 失败: %v", a.ID, cerr)
			continue
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			resp.Body.Close()
			lastErr = "上游 HTTP " + itoa(resp.StatusCode) + "：" + shorten(string(raw), 200)
			log.Printf("codearts-bridge: %s 上游 %d: %s", a.ID, resp.StatusCode, shorten(string(raw), 200))
			continue
		}

		log.Printf("codearts-bridge: acct=%s model=%s 建流成功", a.ID, bareModel)
		h.forwardCodeArts(w, resp, body, bareModel, stream)
		return true
	}
	h.lastCodeArtsErr = lastErr
	return false
}

// forwardCodeArts 流式逐帧（回写 model）/ 非流式本地聚合。
func (h *Handler) forwardCodeArts(w http.ResponseWriter, resp *http.Response, reqBody []byte, model string, wantStream bool) {
	defer resp.Body.Close()

	if !wantStream {
		doc, err := upstream.Aggregate(resp.Body)
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

	// 用 Text() 而不是 Bytes()：后者是复用缓冲，写出去之前下次 Scan 就覆盖了
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

// rewriteJSONObject 把 JSON 对象里某个字符串字段改成 want，重新序列化。
// 没有该字段或不是对象时原样返回（`data: [DONE]` 这类非 JSON 行因此零开销）。
func rewriteJSONObject(payload, field, want string) string {
	if want == "" || !strings.Contains(payload, `"`+field+`"`) {
		return payload
	}
	var doc map[string]any
	if json.Unmarshal([]byte(payload), &doc) != nil {
		return payload
	}
	cur, ok := doc[field].(string)
	if !ok || cur == "" || cur == want {
		return payload
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return payload
	}
	return string(out)
}

// itoa 小整数转串（省一个 strconv 的 import 面）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

/* ── 模型目录（签名 GET + 10 分钟缓存） ──────────────────────── */

var (
	codeArtsCatMu   sync.Mutex
	codeArtsCatAt   time.Time
	codeArtsCatMods []codearts.Model
)

func (h *Handler) codeartsCatalog() []codearts.Model {
	if h.cfg.ExtAccounts == nil {
		return nil
	}
	codeArtsCatMu.Lock()
	if time.Since(codeArtsCatAt) < 10*time.Minute && codeArtsCatMods != nil {
		mods := codeArtsCatMods
		codeArtsCatMu.Unlock()
		return mods
	}
	codeArtsCatMu.Unlock()

	var cred *codearts.Credential
	for _, a := range h.cfg.ExtAccounts() {
		if a.Provider != extstore.PCodeArts || a.Disabled {
			continue
		}
		var c codearts.Credential
		if json.Unmarshal(a.Cred, &c) == nil && c.AccessKeyID != "" {
			cred = &c
			break
		}
	}
	if cred == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mods, err := codearts.New().ListModels(ctx, cred)
	if err != nil {
		log.Printf("codearts-bridge: 拉取模型目录失败，保留旧缓存: %v", err)
		codeArtsCatMu.Lock()
		prev := codeArtsCatMods
		codeArtsCatMu.Unlock()
		return prev
	}
	if len(mods) == 0 {
		return nil
	}
	codeArtsCatMu.Lock()
	codeArtsCatMods, codeArtsCatAt = mods, time.Now()
	codeArtsCatMu.Unlock()
	return mods
}

// codeartsModelPrefix CodeArts 路由前缀。
const codeartsModelPrefix = "codearts:"

// isCodeArtsModel 判断裸模型名是否请求 CodeArts 通道。
func isCodeArtsModel(bare string) bool { return strings.HasPrefix(bare, codeartsModelPrefix) }

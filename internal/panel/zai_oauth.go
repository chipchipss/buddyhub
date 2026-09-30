package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/zai"
)

// zaiOAuthFlows 进行中的 Z.AI OAuth 流程（flow_id → 流程状态）。
//
// 与腾讯 OAuth 的 logins 同构：面板常驻进程，流程 15 分钟未完成即回收，
// 防止"点了登录就走开"的会话无限滞留。
type zaiOAuthFlow struct {
	flow      *zai.OAuthFlow
	created   time.Time
	name      string
	exchanged bool
}

const zaiOAuthTTL = 15 * time.Minute

var (
	zaiOAuthMu    sync.Mutex
	zaiOAuthStore = map[string]*zaiOAuthFlow{}
)

func reapZaiOAuth() {
	now := time.Now()
	for id, f := range zaiOAuthStore {
		if now.Sub(f.created) > zaiOAuthTTL {
			delete(zaiOAuthStore, id)
		}
	}
}

// zaiOAuthStart POST /panel/api/zai/oauth/start —— 发起授权，返回授权链接。
func (p *Panel) zaiOAuthStart(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "Z.AI 账号池未启用"})
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	flow, err := zai.StartOAuth(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	zaiOAuthMu.Lock()
	reapZaiOAuth()
	zaiOAuthStore[flow.FlowID] = &zaiOAuthFlow{flow: flow, created: time.Now(), name: strings.TrimSpace(req.Name)}
	zaiOAuthMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id":       flow.FlowID,
		"authorize_url": flow.AuthorizeURL,
	})
}

// zaiOAuthPoll GET /panel/api/zai/oauth/poll?flow_id=... —— 轮询授权状态。
//
// 授权完成后：兑换 API Key（失败不影响 Plan 通道）→ JWT + Key 一并入池。
func (p *Panel) zaiOAuthPoll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Zai == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "Z.AI 账号池未启用"})
		return
	}
	flowID := strings.TrimSpace(r.URL.Query().Get("flow_id"))
	if flowID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 flow_id"})
		return
	}
	zaiOAuthMu.Lock()
	f := zaiOAuthStore[flowID]
	zaiOAuthMu.Unlock()
	if f == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "授权会话不存在或已过期，请重新发起"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := f.flow.Poll(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	if !res.Done {
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}

	// 授权完成：兑换回退 Key（失败只记提示，账号照常入池）
	apiKey := ""
	keyNote := ""
	exCtx, exCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer exCancel()
	if k, kerr := zai.ExchangeAPIKey(exCtx, res.AccessToken); kerr == nil {
		apiKey = k
	} else {
		keyNote = "（回退 Key 兑换失败：" + kerr.Error() + "，仅 Plan 通道可用）"
	}

	name := f.name
	if name == "" {
		if res.Email != "" {
			name = res.Email
		} else {
			name = "zai-" + flowID[:8]
		}
	}
	acc := zai.NewAccount(name, res.AccessToken)
	acc.APIKey = apiKey
	if err := p.cfg.Zai.Store().Add(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	zaiOAuthMu.Lock()
	delete(zaiOAuthStore, flowID)
	zaiOAuthMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"done":    true,
		"id":      acc.ID,
		"name":    acc.Name,
		"has_key": apiKey != "",
		"message": "账号已入池（Plan 通道）" + keyNote,
	})
}

// Package upstream: loomy_gateway.go — Loomy 模型网关客户端。
//
// 逆向自 Patrick130306/loomy2api 的 PROTOCOL.md（来源：官方桌面客户端 0.9.38
// 未加密的 main-process 源码）。要点：
//   - 模型网关 https://loomyad.xunfei.cn/api/v1 本身 OpenAI 兼容；
//     登录 session 就是 API key（客户端 opencode.json 声明 useSessionAuth）。
//   - 每个请求必须带：Authorization: Bearer <session> 与 token: <session>
//     （两种拼写上游都认，客户端双发）、traceparent（缺失时上游挂起到超时）、
//     loomy-version（仅存在性检查）。
//   - 密码登录全自动：getPuKey 返回 RSA-1024 公钥 + rcode（服务端 nonce，
//     非验证码）→ 密码 RSA-PKCS1v15 加密 → /login/account/byPwd，
//     expire=1209600（14 天）。无刷新 token，过期须重登。
//   - 账号服务（account.xfinfr.com）请求需 HMAC-SHA1 签名（9 行 stringToSign，
//     客户端 .env.prod 内置 key）。模型网关不签名。
package upstream

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LoomyModelGatewayBase Loomy 官方模型网关（OpenAI 兼容）。
const LoomyModelGatewayBase = "https://loomyad.xunfei.cn/api/v1"

// LoomyAccountBase 讯飞账号服务（登录/session 签发）。
const LoomyAccountBase = "https://account.xfinfr.com"

// 客户端 .env.prod 内置的账号服务签名 key（与 loomy2api 一致）。
const (
	loomyAKID     = "2thryby66wxi53sk"
	loomyAKSecret = "FqALy0eToG5KnyToYfdDNEXZGvt9d8jM"
)

// LoomySessionExpireSec 登录请求的 session 有效期：14 天。
const LoomySessionExpireSec = 14 * 24 * 3600

// loomyLoginEnvelope 账号服务通用请求封套。
type loomyLoginEnvelope struct {
	Base  map[string]any `json:"base"`
	Param map[string]any `json:"param"`
}

// LoomyLoginResult 密码/短信登录产物。
type LoomyLoginResult struct {
	Session string `json:"session"`
	UserID  string `json:"userid"`
	Phone   string `json:"phone"`
}

// loomyHTTP 独立 HTTP 客户端（账号服务 + 模型网关共用 TLS 指纹设置与腾讯池一致）。
func loomyHTTP() *http.Client {
	return &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{ServerName: ""},
			ForceAttemptHTTP2: true,
		},
	}
}

// loomyTraceID 32-hex 随机 traceid（每请求刷新）。
func loomyTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LoomyModelHeaders 模型网关请求头（双 token + traceparent 必带）。
func LoomyModelHeaders(session, version string) map[string]string {
	h := map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + session,
		"token":         session,
		"traceparent":   "00-" + loomyTraceID() + "-" + loomyTraceID()[:16] + "-01",
	}
	if version != "" {
		h["loomy-version"] = version
	}
	return h
}

// loomySign 账号服务 HMAC-SHA1 签名（9 行 stringToSign，后两行空串也必须有）。
func loomySign(method, escapedPath, escapedQuery, contentMD5, contentType, date, nonce string) string {
	sts := strings.Join([]string{
		method, escapedPath, escapedQuery, contentMD5, contentType,
		date, nonce, "", "",
	}, "\n")
	mac := sha1.New()
	mac.Write([]byte(loomyAKSecret))
	mac.Write([]byte(sts))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// LoomyPasswordLogin 密码登录（全自动）：getPuKey → RSA 加密 → byPwd。
func LoomyPasswordLogin(phone, password string) (*LoomyLoginResult, error) {
	cli := loomyHTTP()

	// 1) getPuKey：RSA 公钥 + rcode
	puBody, _ := json.Marshal(loomyLoginEnvelope{
		Base:  loomyBaseEnvelope(),
		Param: map[string]any{},
	})
	puReq, err := http.NewRequest(http.MethodPost, LoomyAccountBase+"/login/account/getPuKey", bytes.NewReader(puBody))
	if err != nil {
		return nil, err
	}
	loomySignRequest(puReq, string(puBody))
	puResp, err := cli.Do(puReq)
	if err != nil {
		return nil, fmt.Errorf("getPuKey: %w", err)
	}
	defer puResp.Body.Close()
	puRaw, _ := io.ReadAll(puResp.Body)
	if puResp.StatusCode != 200 {
		return nil, fmt.Errorf("getPuKey HTTP %d: %s", puResp.StatusCode, truncate(string(puRaw), 200))
	}
	var pu struct {
		Data struct {
			PuKey string `json:"pukey"`
			RCode string `json:"rcode"`
		} `json:"data"`
		Code string `json:"code"`
		Desc string `json:"desc"`
	}
	if err := json.Unmarshal(puRaw, &pu); err != nil {
		return nil, fmt.Errorf("getPuKey 解析: %w", err)
	}
	if pu.Data.PuKey == "" || pu.Data.RCode == "" {
		return nil, fmt.Errorf("getPuKey 缺少 pukey/rcode: code=%s desc=%s", pu.Code, pu.Desc)
	}

	// 2) RSA-PKCS1v15 加密密码
	encPwd, err := loomyRSAPKCS1Encrypt(pu.Data.PuKey, password)
	if err != nil {
		return nil, fmt.Errorf("密码加密: %w", err)
	}

	// 3) byPwd
	lpBody, _ := json.Marshal(loomyLoginEnvelope{
		Base: loomyBaseEnvelope(),
		Param: map[string]any{
			"loginid":  phone,
			"password": encPwd,
			"rcode":    pu.Data.RCode,
			"type":     1,
			"expire":   LoomySessionExpireSec,
		},
	})
	lpReq, err := http.NewRequest(http.MethodPost, LoomyAccountBase+"/login/account/byPwd", bytes.NewReader(lpBody))
	if err != nil {
		return nil, err
	}
	loomySignRequest(lpReq, string(lpBody))
	lpResp, err := cli.Do(lpReq)
	if err != nil {
		return nil, fmt.Errorf("byPwd: %w", err)
	}
	defer lpResp.Body.Close()
	lpRaw, _ := io.ReadAll(lpResp.Body)
	var lp struct {
		Code string `json:"code"`
		Desc string `json:"desc"`
		Data struct {
			Session string `json:"session"`
			UserID  string `json:"userid"`
			Phone   string `json:"phone"`
		} `json:"data"`
	}
	if err := json.Unmarshal(lpRaw, &lp); err != nil {
		return nil, fmt.Errorf("byPwd 解析: %w", err)
	}
	if lp.Data.Session == "" {
		return nil, fmt.Errorf("密码登录失败: code=%s desc=%s", lp.Code, lp.Desc)
	}
	phoneMasked := lp.Data.Phone
	if phoneMasked == "" {
		phoneMasked = phone
	}
	return &LoomyLoginResult{Session: lp.Data.Session, UserID: lp.Data.UserID, Phone: phoneMasked}, nil
}

// LoomySendSMSCode 短信验证码下发（返回 msgid）。
func LoomySendSMSCode(phone string) (string, error) {
	body, _ := json.Marshal(loomyLoginEnvelope{
		Base: loomyBaseEnvelope(),
		Param: map[string]any{
			"ccode":  "86",
			"phone":  phone,
			"expire": 300,
		},
	})
	req, err := http.NewRequest(http.MethodPost, LoomyAccountBase+"/login/phone/sendMsgCode", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	loomySignRequest(req, string(body))
	resp, err := loomyHTTP().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Code string `json:"code"`
		Desc string `json:"desc"`
		Data struct {
			MsgID string `json:"msgid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("sendMsgCode 解析: %w", err)
	}
	if out.Data.MsgID == "" {
		return "", fmt.Errorf("验证码下发失败: code=%s desc=%s", out.Code, out.Desc)
	}
	return out.Data.MsgID, nil
}

// LoomySMSLogin 短信验证码登录。
func LoomySMSLogin(phone, code, msgID string) (*LoomyLoginResult, error) {
	body, _ := json.Marshal(loomyLoginEnvelope{
		Base: loomyBaseEnvelope(),
		Param: map[string]any{
			"ccode":  "86",
			"phone":  phone,
			"mcode":  code,
			"msgid":  msgID,
			"expire": LoomySessionExpireSec,
		},
	})
	req, err := http.NewRequest(http.MethodPost, LoomyAccountBase+"/login/phone/checkCode", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	loomySignRequest(req, string(body))
	resp, err := loomyHTTP().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Code string `json:"code"`
		Desc string `json:"desc"`
		Data struct {
			Session string `json:"session"`
			UserID  string `json:"userid"`
			Phone   string `json:"phone"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("checkCode 解析: %w", err)
	}
	if out.Data.Session == "" {
		return nil, fmt.Errorf("短信登录失败: code=%s desc=%s", out.Code, out.Desc)
	}
	phoneMasked := out.Data.Phone
	if phoneMasked == "" {
		phoneMasked = phone
	}
	return &LoomyLoginResult{Session: out.Data.Session, UserID: out.Data.UserID, Phone: phoneMasked}, nil
}

// LoomyChatStream 发起模型网关 chat/completions，返回原始响应体 + Content-Type。
// 上游流式返回 SSE（text/event-stream），非流式返回标准 chat.completion JSON
// （application/json）——调用方必须按 Content-Type 分流，不能假设 SSE。
func LoomyChatStream(session, body string) (io.ReadCloser, int, string, error) {
	req, err := http.NewRequest(http.MethodPost, LoomyModelGatewayBase+"/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, 0, "", err
	}
	for k, v := range LoomyModelHeaders(session, "0.9.38") {
		req.Header.Set(k, v)
	}
	resp, err := loomyHTTP().Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, resp.StatusCode, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return resp.Body, 200, resp.Header.Get("Content-Type"), nil
}

// LoomyModels 拉模型目录（原样透出，无别名）。
func LoomyModels(session string) ([]map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, LoomyModelGatewayBase+"/models", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range LoomyModelHeaders(session, "0.9.38") {
		req.Header.Set(k, v)
	}
	resp, err := loomyHTTP().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ---- 内部工具 ----

func loomyBaseEnvelope() map[string]any {
	return map[string]any{
		"appid":   "GM3LOOMY",
		"modelid": "Web",
		"version": "1.0.0",
		"devid":   "web",
		"ua":      "Loomy|Desktop|Electron|macOS",
		"traceid": loomyTraceID(),
	}
}

// loomySignRequest 给账号服务请求加签名头（Authorization: account <id>:<sig>）。
func loomySignRequest(req *http.Request, body string) {
	md5Sum := ""
	if body != "" {
		md5Sum = loomyContentMD5(body)
	}
	loc := time.Now().UTC().Format(http.TimeFormat)
	nonce := loomyTraceID()
	path := req.URL.EscapedPath()
	query := req.URL.RawQuery
	sig := loomySign(req.Method, path, query, md5Sum, "application/json", loc, nonce)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Date", loc)
	req.Header.Set("Nonce", nonce)
	req.Header.Set("Authorization", "account "+loomyAKID+":"+sig)
}

func loomyContentMD5(s string) string {
	sum := md5.Sum([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// loomyRSAPKCS1Encrypt RSA PKCS#1 v1.5 加密（pukey 为 base64 DER 公钥）。
func loomyRSAPKCS1Encrypt(puKeyB64, plaintext string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(puKeyB64)
	if err != nil {
		return "", fmt.Errorf("公钥 base64 解码: %w", err)
	}
	pubAny, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		// 某些实现给的是 PKCS#1 裸公钥
		rsaPub, err2 := x509.ParsePKCS1PublicKey(der)
		if err2 != nil {
			return "", fmt.Errorf("公钥解析: %v / %v", err, err2)
		}
		pubAny = rsaPub
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("非 RSA 公钥")
	}
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

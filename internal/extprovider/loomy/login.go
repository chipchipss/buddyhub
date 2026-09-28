// Package loomy 讯飞 Loomy 账号服务：密码/短信登录（协议对齐 Patrick130306/loomy2api）。
//
// 账号服务（account.xfinfr.com）与业务 API（loomyad.xunfei.cn）分离：
// 登录拿 14 天 session 后，session 直接用于业务头的 token 字段。
//
// 认证：HMAC-SHA1 签名（对齐客户端 electron/xfyun/sign.js）：
//
//	stringToSign = METHOD \n ESCAPED_PATH \n ESCAPED_QUERY \n Content-MD5 \n
//	               Content-Type \n Date \n Nonce \n SignedHeaders \n CanonicalizedHeaders
//	signature    = base64(hmac_sha1(accessKeySecret, stringToSign))
//	Authorization = "account <accessKeyId>:<signature>"
//
// 密码加密：getPuKey 返回 RSA 公钥（DER SubjectPublicKeyInfo base64）+ rcode，
// 密码 RSA/PKCS#1 v1.5 加密后提交 /login/account/byPwd（无验证码）。
package loomy

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	AccountBase     = "https://account.xfinfr.com"
	DefaultAKID     = "2thryby66wxi53sk"
	DefaultAKSecret = "zsak6eadrbawz683wf5r3m2snrwj868r"
	DefaultAppID    = "GM3LOOMY"
	WebModelID      = "Web"
	ClientVersion   = "1.0.0"
	ClientUA        = "Loomy|Desktop|Electron|macOS"
	SessionExpire   = 14 * 24 * 3600
	requestTimeout  = 30 * time.Second
)

// Identity 每账号设备身份（首登生成、持久化复用，保持设备一致性）。
type Identity struct {
	DevID   string `json:"devid"`
	Campus  string `json:"campus_device_id,omitempty"`
	ModelID string `json:"modelid"`
	Version string `json:"version"`
	UA      string `json:"ua"`
}

// NewIdentity 生成 per-account 设备身份。
func NewIdentity() *Identity {
	return &Identity{
		DevID:   "web-" + randHex(16),
		ModelID: WebModelID,
		Version: ClientVersion,
		UA:      ClientUA,
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Client 账号服务客户端。
type Client struct {
	AKID     string
	AKSecret string
	HTTP     *http.Client
}

// NewClient 创建客户端（缺省内置 AK 对，即客户端公开常量）。
func NewClient() *Client {
	return &Client{
		AKID:     DefaultAKID,
		AKSecret: DefaultAKSecret,
		HTTP:     &http.Client{Timeout: requestTimeout},
	}
}

// contentMD5 base64(md5(body))；空 body 为空串。
func contentMD5(body string) string {
	if body == "" {
		return ""
	}
	sum := md5.Sum([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// rfc3986Escape RFC 3986 严格转义（Go 默认保留子定界符，需手工补）。
func rfc3986Escape(s string) string {
	hex := "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// escapedPath 按段转义路径（保留 /）。
func escapedPath(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		segs[i] = rfc3986Escape(seg)
	}
	return strings.Join(segs, "/")
}

// buildHeaders 构造一次账号服务请求的完整头（签名在内部完成）。
func (c *Client) buildHeaders(method, path, body string) (map[string]string, error) {
	date := time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	nonce := randHex(32) // uuid4().hex 同长度
	bodyMD5 := contentMD5(body)

	// stringToSign 九行：METHOD/PATH/QUERY/MD5/CTYPE/DATE/NONCE/SIGNED/CANON
	// （无自定义 x-* 头时后两行为空串但必须存在）。
	stringToSign := strings.Join([]string{
		method,
		escapedPath(path),
		"", // query：本服务全部走 body，无 query
		bodyMD5,
		"application/json",
		date,
		nonce,
		"", // signed headers
		"", // canonicalized headers
	}, "\n")

	mac := hmac.New(sha1.New, []byte(c.AKSecret))
	mac.Write([]byte(stringToSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return map[string]string{
		"Authorization": fmt.Sprintf("account %s:%s", c.AKID, sig),
		"Date":          date,
		"Nonce":         nonce,
		"Content-Type":  "application/json",
		"Content-MD5":   bodyMD5,
	}, nil
}

// call 发一次账号服务请求并校验信封（code!="000000" 即失败）。
func (c *Client) call(path string, body any) (map[string]any, error) {
	var bodyStr string
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyStr = string(raw)
	}
	headers, err := c.buildHeaders(http.MethodPost, path, bodyStr)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, AccountBase+path, strings.NewReader(bodyStr))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("account service unreachable: %w", err)
	}
	defer resp.Body.Close()

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("HTTP %d: 响应不是 JSON", resp.StatusCode)
	}
	code, _ := payload["code"].(string)
	if code == "" {
		if ec, ok := payload["errorCode"].(string); ok {
			code = ec
		}
	}
	if code != "" && code != "000000" {
		msg, _ := payload["desc"].(string)
		if msg == "" {
			msg, _ = payload["message"].(string)
		}
		if msg == "" {
			msg = code
		}
		return nil, fmt.Errorf("%s failed: %s", path, msg)
	}
	return payload, nil
}

// base 构造请求 base 块（设备身份 + 应用标识）。
func (c *Client) base(ident *Identity) map[string]any {
	if ident == nil {
		ident = NewIdentity()
	}
	return map[string]any{
		"appid":   DefaultAppID,
		"modelid": ident.ModelID,
		"version": ident.Version,
		"devid":   ident.DevID,
		"ua":      ident.UA,
		"traceid": randHex(16),
	}
}

// LoginResult 登录成功提取的会话字段。
type LoginResult struct {
	Session string `json:"session"`
	UserID  string `json:"userid"`
	Phone   string `json:"phone"`
}

// extractSession 从任意返回形态提取 session/userid/phone。
func extractSession(payload map[string]any) (*LoginResult, error) {
	node := payload
	if data, ok := payload["data"].(map[string]any); ok {
		node = data
	}
	session, _ := node["session"].(string)
	if session == "" {
		return nil, fmt.Errorf("login succeeded but returned no session")
	}
	userID, _ := node["userid"].(string)
	if userID == "" {
		userID, _ = node["userId"].(string)
	}
	phone, _ := node["phone"].(string)
	if phone == "" {
		phone, _ = node["loginid"].(string)
	}
	return &LoginResult{Session: session, UserID: userID, Phone: phone}, nil
}

// LoginByPassword 密码登录：getPuKey → RSA 加密 → byPwd。
func (c *Client) LoginByPassword(loginID, password string, ident *Identity) (*LoginResult, error) {
	if ident == nil {
		ident = NewIdentity()
	}
	// 1) 公钥 + rcode（零副作用探测）
	pukeyPayload, err := c.call("/login/account/getPuKey", map[string]any{"base": c.base(ident)})
	if err != nil {
		return nil, err
	}
	data, _ := pukeyPayload["data"].(map[string]any)
	if data == nil {
		return nil, fmt.Errorf("getPuKey returned no data")
	}
	pukey, _ := data["pukey"].(string)
	rcode, _ := data["rcode"].(string)
	if pukey == "" {
		return nil, fmt.Errorf("getPuKey returned no public key")
	}

	// 2) RSA/PKCS#1 v1.5 加密密码
	encrypted, err := RSAEncryptPKCS1v15FromB64DER(pukey, password)
	if err != nil {
		return nil, err
	}

	// 3) byPwd 提交
	payload, err := c.call("/login/account/byPwd", map[string]any{
		"base": c.base(ident),
		"param": map[string]any{
			"loginid":  loginID,
			"password": encrypted,
			"rcode":    rcode,
			"type":     1,
			"expire":   SessionExpire,
		},
	})
	if err != nil {
		return nil, err
	}
	return extractSession(payload)
}

// SendSMSCode 下发短信验证码（返回 msgid，需人工读码）。
func (c *Client) SendSMSCode(phone string, ident *Identity) (string, error) {
	payload, err := c.call("/login/phone/sendMsgCode", map[string]any{
		"base": c.base(ident),
		"param": map[string]any{
			"ccode":  "86",
			"phone":  phone,
			"expire": 300,
		},
	})
	if err != nil {
		return "", err
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return "", fmt.Errorf("sendMsgCode returned no data")
	}
	msgid, _ := data["msgid"].(string)
	if msgid == "" {
		return "", fmt.Errorf("sendMsgCode returned no msgid")
	}
	return msgid, nil
}

// LoginBySMS 短信验证码登录。
func (c *Client) LoginBySMS(phone, code, msgID string, ident *Identity) (*LoginResult, error) {
	payload, err := c.call("/login/phone/checkCode", map[string]any{
		"base": c.base(ident),
		"param": map[string]any{
			"ccode":  "86",
			"phone":  phone,
			"mcode":  code,
			"msgid":  msgID,
			"expire": SessionExpire,
		},
	})
	if err != nil {
		return nil, err
	}
	return extractSession(payload)
}

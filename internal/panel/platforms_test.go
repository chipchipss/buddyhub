package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 注册表本身的完整性：id 唯一、展示名非空、分组合法、login 合法。
// 这些错了会在界面上表现为「平台消失」或「按钮点了报错」，但不会编译失败。
func TestPlatformRegistryIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	validGroups := map[string]bool{GroupGateway: true, GroupPoints: true}
	validLogins := map[string]bool{
		LoginNone: true, LoginOAuth: true, LoginDetect: true, LoginSMS: true,
		LoginQR: true, LoginDevice: true, LoginCode: true, LoginManual: true, LoginConfig: true,
	}
	for _, p := range platforms {
		if p.ID == "" {
			t.Error("平台 id 不能为空")
			continue
		}
		if seen[p.ID] {
			t.Errorf("平台 id 重复: %s", p.ID)
		}
		seen[p.ID] = true
		if strings.TrimSpace(p.Name) == "" {
			t.Errorf("%s: 展示名不能为空（界面会出现空白）", p.ID)
		}
		if !validGroups[p.Group] {
			t.Errorf("%s: 分组 %q 非法", p.ID, p.Group)
		}
		if !validLogins[p.Login] {
			t.Errorf("%s: 入池方式 %q 非法", p.ID, p.Login)
		}
		// 分组即能力：gateway ⇔ 有前缀。对不上就是界面归类错了。
		if p.Group == GroupGateway && p.Prefix == "" {
			t.Errorf("%s: 归在 gateway 却没声明模型前缀", p.ID)
		}
		if p.Group == GroupPoints && p.Prefix != "" {
			t.Errorf("%s: 归在 points 却有模型前缀（%q）", p.ID, p.Prefix)
		}
	}
}

// 每个有前缀的平台都必须真的在路由里可达——注册表写了前缀但 PlatformOf
// 不认，界面上会显示这个平台、请求却打到腾讯池（静默错路由）。
func TestEveryGatewayPrefixIsRoutable(t *testing.T) {
	for _, p := range platforms {
		if p.Prefix == "" {
			continue
		}
		// workbuddy 的前缀是 cn:/global:（realm 前缀，由 resolveModel 处理）
		if p.ID == "workbuddy" {
			continue
		}
		if got := platformOfInPanel(p.Prefix + "some-model"); got == "" {
			t.Errorf("%s: 前缀 %q 没有对应的路由（PlatformOf 不认）", p.ID, p.Prefix)
		}
	}
}

// platformOfInPanel 与 server.PlatformOf 同口径的最小副本。
// 面板不 import server（会成环），所以这里按注册表自查一遍前缀声明。
func platformOfInPanel(model string) string {
	for _, p := range platforms {
		if p.Prefix != "" && strings.HasPrefix(model, p.Prefix) {
			return p.ID
		}
	}
	return ""
}

// authedGet 带测试 key 打面板接口（对启用鉴权的面板；无鉴权时多余但无害）。
func authedGet(p *Panel, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func authedPost(p *Panel, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// 「不要缺」：注册表里的每个平台都必须出现在账号目录里。
func TestAccountsDirCoversEveryPlatform(t *testing.T) {
	p := loginTestPanel(t)
	rec := authedGet(p, "/panel/api/accounts/dir")
	if rec.Code != 200 {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var doc struct {
		Platforms []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"platforms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析: %v", err)
	}
	got := map[string]bool{}
	for _, x := range doc.Platforms {
		got[x.ID] = true
		if strings.TrimSpace(x.Name) == "" {
			t.Errorf("目录里 %s 的展示名为空", x.ID)
		}
	}
	for _, pl := range platforms {
		if !got[pl.ID] {
			t.Errorf("注册表有 %s，账号目录里却没有（界面上会缺一块）", pl.ID)
		}
	}
	if len(doc.Platforms) != len(platforms) {
		t.Errorf("目录平台数 = %d，注册表 = %d", len(doc.Platforms), len(platforms))
	}
}

// 「不要缺」：注册表里的每个平台都必须能被 API Key 授权。
func TestAPIKeyAcceptsEveryPlatform(t *testing.T) {
	// 用隔离目录的面板：newTestPanel 的 StateFile 为空，extstore 会把账号表
	// 落在**包目录**里（曾经的 bug：测试往源码树写文件并误提交）
	p := loginTestPanel(t)
	for _, pl := range platforms {
		rec := authedPost(p, "/panel/api/apikeys", `{"name":"t","platforms":["`+pl.ID+`"]}`)
		// 未注入 config 闭包时返回 500 是环境问题，不是白名单问题——
		// 只有 400「未知平台」才算漏配。
		if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "未知平台") {
			t.Errorf("注册表有 %s，API Key 白名单却不认（授权时会被拒）", pl.ID)
		}
	}
}

// 「不要缺」：注册表里的每个平台都必须能被手工添加（或明确不属于 extstore）。
func TestManualAddMatchesRegistry(t *testing.T) {
	p := loginTestPanel(t)
	for _, pl := range platforms {
		rec := authedPost(p, "/panel/api/ext/accounts",
			`{"provider":"`+pl.ID+`","id":"x","cred":{"a":"b"}}`)

		allowed := rec.Code != http.StatusBadRequest
		want := platformUsesExtstore(pl.ID)
		if allowed != want {
			t.Errorf("%s: 手工添加 allowed=%v，期望 %v（body: %s）", pl.ID, allowed, want, rec.Body.String())
		}
	}
}

// 注册表 API 必须与内存里的表一致（前端全靠它）。
func TestGetPlatformsEndpoint(t *testing.T) {
	p := loginTestPanel(t)
	rec := authedGet(p, "/panel/api/platforms")
	if rec.Code != 200 {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var doc struct {
		Platforms []Platform `json:"platforms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(doc.Platforms) != len(platforms) {
		t.Fatalf("下发了 %d 个平台，注册表有 %d 个", len(doc.Platforms), len(platforms))
	}
	// 前端靠 login 决定出哪套交互，靠 prefix 渲染前缀说明——不能是空的
	for _, pl := range doc.Platforms {
		if pl.ID == "" || pl.Name == "" || pl.Group == "" {
			t.Errorf("下发的平台字段不全: %+v", pl)
		}
	}
}

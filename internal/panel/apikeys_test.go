// apikeys_test.go 密钥接口的两条安全约定（清单 37 的地基）：列表不回明文，
// 删除认 id；完整密钥只在生成那一次的响应里出现。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeKeyStore 一份住在内存里的配置：api_keys 数组 + 走 mutateAPIKeys 的写回路径。
type fakeKeyStore struct {
	cfg map[string]any
}

func (f *fakeKeyStore) LoadConfig() (any, error) { return f.cfg, nil }

func (f *fakeKeyStore) SaveConfig(raw []byte) ([]string, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	f.cfg = m
	return nil, nil
}

func newKeyPanel(t *testing.T, keysJSON string) (*Panel, *fakeKeyStore) {
	t.Helper()
	var arr any
	if err := json.Unmarshal([]byte(keysJSON), &arr); err != nil {
		t.Fatal(err)
	}
	st := &fakeKeyStore{cfg: map[string]any{"listen": ":7863", "api_keys": arr}}
	return New(Config{Version: "test", LoadConfig: st.LoadConfig, SaveConfig: st.SaveConfig}), st
}

func TestAPIKeysListNeverLeaksPlaintext(t *testing.T) {
	p, _ := newKeyPanel(t, `[{"key":"bh-deadbeefcafe1234567890abcdef1234","name":"桌面端","platforms":["qoder"]}]`)
	rec := httptest.NewRecorder()
	p.getAPIKeys(rec, httptest.NewRequest(http.MethodGet, "/panel/api/apikeys", nil))

	body := rec.Body.String()
	if strings.Contains(body, "bh-deadbeefcafe1234567890abcdef1234") {
		t.Fatalf("列表把明文密钥发给了前端：%s", body)
	}
	var got struct {
		Keys []struct {
			ID     string   `json:"id"`
			Name   string   `json:"name"`
			Masked string   `json:"masked"`
			Plat   []string `json:"platforms"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Keys) != 1 || got.Keys[0].ID == "" || !strings.Contains(got.Keys[0].Masked, "…") {
		t.Fatalf("行里没有 id / 掩码：%s", body)
	}
}

func TestAPIKeysDeleteByID(t *testing.T) {
	p, st := newKeyPanel(t, `[{"key":"bh-00112233445566778899aabbccddeeff","name":"要被删掉的"}]`)
	id := keyID("bh-00112233445566778899aabbccddeeff")

	rec := httptest.NewRecorder()
	p.deleteAPIKeys(rec, httptest.NewRequest(http.MethodPost, "/panel/api/apikeys/delete",
		strings.NewReader(`{"id":"`+id+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("按 id 删除失败：%d %s", rec.Code, rec.Body.String())
	}
	raw, _ := json.Marshal(st.cfg["api_keys"])
	if strings.Contains(string(raw), "bh-00112233445566778899") {
		t.Fatalf("配置里那把 Key 没被删掉：%s", raw)
	}

	// 不存在的 id 必须是 404，而不是「静默成功」——用户会以为删了。
	rec2 := httptest.NewRecorder()
	p.deleteAPIKeys(rec2, httptest.NewRequest(http.MethodPost, "/panel/api/apikeys/delete",
		strings.NewReader(`{"id":"ffffffffffff"}`)))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("状态码 %d，期望 404", rec2.Code)
	}
}

func TestAPIKeyGenerateReturnsFullKeyOnce(t *testing.T) {
	p, st := newKeyPanel(t, `[]`)
	rec := httptest.NewRecorder()
	p.postAPIKeys(rec, httptest.NewRequest(http.MethodPost, "/panel/api/apikeys",
		strings.NewReader(`{"name":"新客户端","platforms":["qoder"],"note":"笔记本"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Key struct {
			Key       string   `json:"key"`
			Name      string   `json:"name"`
			Platforms []string `json:"platforms"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// 一次性卡片要能拿到完整密钥，否则「只显示这一次」无从显示
	if !strings.HasPrefix(got.Key.Key, "bh-") || len(got.Key.Key) < 30 {
		t.Fatalf("生成响应没带回完整密钥：%q", got.Key.Key)
	}
	// 而列表接口随后再也给不出它
	raw, _ := json.Marshal(st.cfg["api_keys"])
	if !strings.Contains(string(raw), got.Key.Key) {
		t.Fatalf("配置里没有落盘这把 Key：%s", raw)
	}
	rec2 := httptest.NewRecorder()
	p.getAPIKeys(rec2, httptest.NewRequest(http.MethodGet, "/panel/api/apikeys", nil))
	if strings.Contains(rec2.Body.String(), got.Key.Key) {
		t.Fatalf("列表又回了一次明文：%s", rec2.Body.String())
	}
}

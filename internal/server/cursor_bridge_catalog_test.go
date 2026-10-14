package server

// 模型目录 → 桥接的端到端接线测试（自带假上游，不依赖 cursor_bridge_test.go
// 里的 harness，避免与并行改动撞车）。
//
// 要钉死的是"库完成 ≠ 功能可用"这一层的最后一环：桥接确实拉了目录、
// 确实把**目录解析出的变体 id** 发给了上游，而不是把用户给的名字原样送。
// 验法不靠猜：从 BidiAppend 的上行负载里把 run_request 的 protobuf 还原
// 出来，直接查里面的字符串。
import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/cursor"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/pool"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

// catFakeUpstream 一个同时演 unary 目录、身份、RunSSE、BidiAppend 的假上游。
type catFakeUpstream struct {
	mu sync.Mutex
	// pickerJSON AiService/AvailableModels 的响应体。
	pickerJSON string
	// runFrames RunSSE 要回的帧字节。
	runFrames []byte
	// appendPayloads 收到的每条 BidiAppend 原始负载。
	appendPayloads [][]byte
	// dirCalls 目录端点被问了几次（验 ModelsKeep 缓存）。
	dirCalls int
	runCalls int
}

func (f *catFakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "AvailableModels"):
		f.mu.Lock()
		f.dirCalls++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.pickerJSON))
	case strings.HasSuffix(p, "GetUsableModels"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`)) // 空 = 不做变体裁剪（fork 同款）
	case strings.HasSuffix(p, "GetMe"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"u@example.com"}`))
	case strings.HasSuffix(p, "GetPlanInfo"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"planInfo":{"planName":"Pro"}}`))
	case strings.HasSuffix(p, "RunSSE"):
		f.mu.Lock()
		f.runCalls++
		raw := f.runFrames
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/connect+proto")
		_, _ = w.Write(raw)
	case strings.HasSuffix(p, "BidiAppend"):
		raw := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(raw)
		}
		f.mu.Lock()
		f.appendPayloads = append(f.appendPayloads, raw)
		f.mu.Unlock()
		w.WriteHeader(200)
	default:
		w.WriteHeader(404)
	}
}

// catalogSample 一个带两个 effort 变体的模型（200k 单一上下文尺寸）。
const catalogSample = `{"models":[{
	"name":"claude-opus-5","clientDisplayName":"Claude Opus 5",
	"parameterDefinitions":[
		{"id":"context","parameterType":{"enumParameter":{"values":[{"value":"200k"}]}}},
		{"id":"effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"high"}]}}}
	],
	"variants":[
		{"legacySlug":"claude-opus-5-5-low","isDefaultNonMaxConfig":true,
		 "parameterValues":[{"id":"context","value":"200k"},{"id":"effort","value":"low"}]},
		{"legacySlug":"claude-opus-5-5-high",
		 "parameterValues":[{"id":"context","value":"200k"},{"id":"effort","value":"high"}]}
	]}]}`

func newCatalogHarness(t *testing.T, up *catFakeUpstream, token string) (*Handler, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(srv.Close)

	mgr := extstore.NewManager(t.TempDir())
	raw, _ := json.Marshal(cursor.Credential{AccessToken: token})
	if err := mgr.Add(extstore.PCursor, "cu0", "CU0", raw); err != nil {
		t.Fatalf("add: %v", err)
	}
	prev := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: cursorTransport{target: srv.URL}}
	t.Cleanup(func() { http.DefaultClient = prev })

	h := NewHandler(Config{
		ExtAccounts: mgr.ExtList,
		Pool:        pool.New(""),
		Upstream:    upstream.New(),
	})
	h.ExtSetManager(&fakeExtMgr{})
	return h, srv
}

// runRequestStrings 从 BidiAppend 负载里还原 run_request 的可读文本
// （负载 field1 是帧的 hex 字符串；解出来是一串 protobuf，直接当文本扫
//
//	length-delimited 字段里的字符串即可）。
func runRequestStrings(t *testing.T, payload []byte) string {
	t.Helper()
	fs := cursor.Fields(payload)
	hexStr := cursor.Str(fs, 1)
	if hexStr == "" {
		t.Fatal("BidiAppend 负载里没有 field1 的 hex 消息")
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatalf("hex 解码: %v", err)
	}
	return string(raw)
}

// 请求点名 "claude-opus-5@200k" + reasoning_effort=high：
// 发给上游的必须是目录解析出的 claude-opus-5-5-high，不是用户给的名字。
func TestCursorBridgeSendsResolvedVariantID(t *testing.T) {
	up := &catFakeUpstream{pickerJSON: catalogSample,
		runFrames: cursorTextFrameForCatalogTest("ok")}
	h, _ := newCatalogHarness(t, up, "tok-ok")

	w := postCursor(h, `{"model":"cursor:claude-opus-5@200k","stream":true,
		"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("code = %d body = %s", w.Code, w.Body.String())
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.appendPayloads) == 0 {
		t.Fatal("没抓到上行（BidiAppend 一次都没发）")
	}
	got := runRequestStrings(t, up.appendPayloads[0])
	if !strings.Contains(got, "claude-opus-5-5-high") {
		t.Fatalf("上行里没有目录解析出的变体 id:\n%q", got)
	}
	if strings.Contains(got, "claude-opus-5@200k") {
		t.Fatal("把请求名原样送给了上游（目录解析没生效）")
	}
	// 目录确实被拉了。
	if up.dirCalls == 0 {
		t.Fatal("AvailableModels 一次都没问")
	}
}

// 目录按 token 缓存（ModelsKeep 内不重复拉）：两次请求只问一次目录。
func TestCursorBridgeCachesCatalogPerToken(t *testing.T) {
	up := &catFakeUpstream{pickerJSON: catalogSample,
		runFrames: cursorTextFrameForCatalogTest("ok")}
	h, _ := newCatalogHarness(t, up, "tok-ok")
	for i := 0; i < 2; i++ {
		w := postCursor(h, `{"model":"cursor:claude-opus-5@200k","stream":true,
			"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != 200 {
			t.Fatalf("第 %d 次 code = %d", i+1, w.Code)
		}
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.runCalls != 2 {
		t.Fatalf("RunSSE 次数 = %d, want 2", up.runCalls)
	}
	if up.dirCalls != 1 {
		t.Fatalf("目录被拉了 %d 次, want 1（ModelsKeep 缓存没生效）", up.dirCalls)
	}
}

// 目录拉不到时不阻塞对话：名字原样送（fork 的 try/catch 同款兜底）。
func TestCursorBridgeCatalogFailureFallsBack(t *testing.T) {
	up := &catFakeUpstream{pickerJSON: `{"models":[]}`, // 上游列不出模型
		runFrames: cursorTextFrameForCatalogTest("ok")}
	h, _ := newCatalogHarness(t, up, "tok-ok")
	w := postCursor(h, `{"model":"cursor:some-unknown","stream":true,
		"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != 200 {
		t.Fatalf("目录失败不该挡对话: code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("正文未透出: %s", w.Body.String())
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.appendPayloads) == 0 {
		t.Fatal("兜底路径也该正常建流上行")
	}
	got := runRequestStrings(t, up.appendPayloads[0])
	if !strings.Contains(got, "some-unknown") {
		t.Fatalf("兜底应原样送请求名:\n%q", got)
	}
}

// /v1/models 列出 cursor: 条目，字段来自目录（context_length 取自 @200k
// 那条、档位取自参数定义）——清单与桥接解析走同一条 request() 路径，
// "列得出的名字就发得出对应变体"，这里钉住清单侧真的接上了。
func TestCursorModelsListedInV1Models(t *testing.T) {
	up := &catFakeUpstream{pickerJSON: catalogSample,
		runFrames: cursorTextFrameForCatalogTest("ok")}
	h, _ := newCatalogHarness(t, up, "tok-ok")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	var doc struct {
		Data []struct {
			ID         string   `json:"id"`
			ContextLen int64    `json:"context_length"`
			Efforts    []string `json:"reasoning_supported_efforts"`
		} `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &doc) != nil {
		t.Fatalf("解析 /v1/models: %s", w.Body.String())
	}
	var hit *struct {
		ID         string   `json:"id"`
		ContextLen int64    `json:"context_length"`
		Efforts    []string `json:"reasoning_supported_efforts"`
	}
	for i := range doc.Data {
		if strings.HasPrefix(doc.Data[i].ID, "cursor:") {
			hit = &doc.Data[i]
			break
		}
	}
	if hit == nil {
		t.Fatalf("清单里没有 cursor: 条目: %s", w.Body.String())
	}
	if hit.ID != "cursor:claude-opus-5@200k" {
		t.Fatalf("id = %q（应是目录条目的请求名，与桥接解析同一套名字）", hit.ID)
	}
	if hit.ContextLen != 200_000 {
		t.Fatalf("context_length = %d, want 200000（来自目录 @200k）", hit.ContextLen)
	}
	// 档位按序透出：该模型只有 low/high（none/minimal/xhigh/max 没有）。
	if len(hit.Efforts) != 2 || hit.Efforts[0] != "low" || hit.Efforts[1] != "high" {
		t.Fatalf("reasoning_supported_efforts = %v, want [low high]", hit.Efforts)
	}
}

// 目录拉不到时 /v1/models 整条不受影响：不报错、不卡、不编造条目。
func TestCursorModelsListingSurvivesCatalogFailure(t *testing.T) {
	up := &catFakeUpstream{pickerJSON: `{"models":[]}`, // 上游给不出目录
		runFrames: cursorTextFrameForCatalogTest("ok")}
	h, _ := newCatalogHarness(t, up, "tok-ok")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != 200 {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "cursor:") {
		t.Fatalf("目录不可用时不该编出 cursor 条目: %s", w.Body.String())
	}
}

// cursorTextFrameForCatalogTest 与 cursor_bridge_test.go 的同名构造等价，
// 这里自带一份以免互相依赖（那个文件由并行改动持有）。
func cursorTextFrameForCatalogTest(text string) []byte {
	msg := cursor.PB().Sub(1, cursor.PB().Sub(1, cursor.PB().Str(1, text))).Raw()
	out := cursor.EncodeFrame(msg, 0)
	return append(out, cursor.EncodeFrame([]byte(`{}`), 2)...)
}

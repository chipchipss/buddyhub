package accio

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// 上游风控页样本（截自真实回执：HTTP 200 + text/html + `rgv587_flag:sm`）。
// 网关把它当合法流透传时，客户端只会看到「200 空正文」。
const wafPunishPage = "\r\n\r\n<a id=\"a-link\"\r\n    href=\"https://bixi-intl.alicdn.com/punish/punish:resource:template:ICBUSpace:default_447831.html?qrcode=11e87135|asZbDw\">\r\n</a>\r\n<script>\r\nvar cookie = \"x5secdata=;maxAge=-100\";\r\n</script>\r\n<!--rgv587_flag:sm-->"

func respWith(ct, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{ct}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestInterceptReasonFlagsAntiBotPage(t *testing.T) {
	got := InterceptReason(respWith("text/html;charset=utf-8", wafPunishPage))
	if got == "" {
		t.Fatal("风控页被判成正常流")
	}
	if !strings.Contains(got, "风控") {
		t.Errorf("原因要点明是风控拦截，实际：%s", got)
	}
}

func TestInterceptReasonLeavesRealStreamUntouched(t *testing.T) {
	stream := "data: {\"parts\":[{\"text\":\"hi\"}]}\n\ndata: [DONE]\n\n"
	resp := respWith("text/event-stream", stream)
	if got := InterceptReason(resp); got != "" {
		t.Fatalf("正常流被误判：%s", got)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// Peek 会把字节读进缓冲区：不回填就会丢前 512 字节，正文必须一字节不少。
	if string(raw) != stream {
		t.Errorf("嗅探吃掉了响应体\ngot:  %q\nwant: %q", raw, stream)
	}
}

func TestInterceptReasonFlagsEmptyBody(t *testing.T) {
	if got := InterceptReason(respWith("text/event-stream", "")); got == "" {
		t.Fatal("空响应被判成正常流")
	} else if !strings.Contains(got, "空") {
		t.Errorf("原因要点明空响应，实际：%s", got)
	}
}

func TestInterceptReasonIgnoresUnknownShape(t *testing.T) {
	// 不是 HTML 也不是空（例如 JSON 错误信封）：这里不猜测，交给 Aggregate 的零帧判定。
	if got := InterceptReason(respWith("application/json", `{"success":false,"code":"403","message":"auth failed"}`)); got != "" {
		t.Errorf("非干扰形状被误判：%s", got)
	}
}

func TestAggregateRejectsInterferenceBody(t *testing.T) {
	if _, err := Aggregate(strings.NewReader(wafPunishPage), "c1", 0, "m"); err == nil {
		t.Fatal("风控页被聚合成合法 completion——这正是「200 空正文」的源头")
	} else if !strings.Contains(err.Error(), "不是模型流") {
		t.Errorf("错误要点明形状，实际：%v", err)
	}
}

func TestAggregateRejectsEmptyBody(t *testing.T) {
	if _, err := Aggregate(strings.NewReader(""), "c1", 0, "m"); err == nil {
		t.Fatal("空响应被聚合成合法 completion")
	} else if !strings.Contains(err.Error(), "空") {
		t.Errorf("错误要点明空响应，实际：%v", err)
	}
}

func TestAggregateAcceptsTerminalDoneOnly(t *testing.T) {
	// 只发了 [DONE] 也是合法流，不能误判成干扰。
	if _, err := Aggregate(strings.NewReader("data: [DONE]\n\n"), "c1", 0, "m"); err != nil {
		t.Errorf("[DONE] 流被误判：%v", err)
	}
}

func TestAggregateErrorsWhenNoFrames(t *testing.T) {
	if _, err := Aggregate(strings.NewReader(wafPunishPage), "id", 0, "m"); err == nil {
		t.Fatal("HTML 干扰页被聚合成合法 completion")
	} else if !strings.Contains(err.Error(), "不是模型流") {
		t.Errorf("实际：%v", err)
	}
	if _, err := Aggregate(strings.NewReader(""), "id", 0, "m"); err == nil {
		t.Fatal("空响应被聚合成合法 completion")
	} else if !strings.Contains(err.Error(), "响应体为空") {
		t.Errorf("实际：%v", err)
	}
}

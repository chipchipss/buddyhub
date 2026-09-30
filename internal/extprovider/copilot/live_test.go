package copilot

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveDeviceFlow 对真实 GitHub 跑一次设备码申请（只申请、不轮询、不授权）。
//
// 默认跳过——CI 与离线环境不该依赖外网。需要时显式开启：
//
//	COPILOT_LIVE=1 HTTPS_PROXY=http://127.0.0.1:2080 go test ./internal/extprovider/copilot/ -run Live -v
//
// 注意：github.com（网页域）在国内网络常需代理，而 api.github.com 往往可直连；
// 设备流两个端点都在 github.com 上，因此这条通道通常必须配 HTTPS_PROXY。
func TestLiveDeviceFlow(t *testing.T) {
	if os.Getenv("COPILOT_LIVE") == "" {
		t.Skip("未设置 COPILOT_LIVE=1，跳过真实 GitHub 冒烟")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	f, err := StartDeviceFlow(ctx)
	if err != nil {
		t.Fatalf("真实设备码申请失败（检查网络/代理 HTTPS_PROXY）: %v", err)
	}
	if f.UserCode == "" || f.DeviceCode == "" {
		t.Fatalf("回执不完整: %+v", f)
	}
	if !strings.Contains(f.VerificationURI, "github.com") {
		t.Fatalf("verification_uri 异常: %s", f.VerificationURI)
	}
	t.Logf("设备码 %s → %s（%ds 内有效，轮询间隔 %ds）",
		f.UserCode, f.VerificationURI, f.ExpiresIn, f.Interval)

	// 未授权时轮询必须返回 pending（而不是报错或 Done）——验证状态机语义。
	// 首次轮询实测会命中 incorrect_device_code（设备码尚未生效），Poll 内部容忍之。
	for i := 1; i <= 3; i++ {
		res, err := f.Poll(ctx)
		if err != nil {
			t.Fatalf("第 %d 次轮询失败: %v", i, err)
		}
		t.Logf("轮询 %d: Done=%v Error=%q", i, res.Done, res.Error)
		if res.Done {
			t.Fatalf("未授权却 Done 了: %+v", res)
		}
		if res.Error == "" {
			return // 落到 pending，符合预期
		}
		time.Sleep(time.Duration(f.Interval) * time.Second)
	}
	t.Fatal("连续 3 次轮询都未落到 pending")
}

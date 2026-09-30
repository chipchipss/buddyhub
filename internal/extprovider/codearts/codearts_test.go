package codearts

import (
	"net/http"
	"strings"
	"testing"
)

// 签名头是否带上 x-security-token，直接决定永久 AK/SK 能不能用：
// 华为云永久密钥的请求不带该头，带上空值会让服务端算出的规范请求与客户端
// 不一致（401 verify ak sk signature fail）。
func TestSignRequestSecurityTokenOptional(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"永久 AK/SK：不带该头", "", false},
		{"临时 STS：带上该头", "sts-token-abc", true},
	}
	for _, c := range cases {
		cred := &Credential{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SecurityToken: c.token}
		h, err := signRequestHuawei(cred, http.MethodGet,
			"https://snap-access.cn-north-4.myhuaweicloud.com/snap-manager/v1/statistics/plugin", "")
		if err != nil {
			t.Fatalf("%s: 签名失败: %v", c.name, err)
		}

		_, present := h["x-security-token"]
		if present != c.want {
			t.Fatalf("%s: x-security-token 存在=%v，期望 %v", c.name, present, c.want)
		}
		// 头一旦参与签名就必须出现在 SignedHeaders 里，否则服务端算不出同样的签名
		signed := strings.Contains(h["Authorization"], "x-security-token")
		if signed != c.want {
			t.Fatalf("%s: SignedHeaders 含 x-security-token=%v，期望 %v（Authorization=%s）",
				c.name, signed, c.want, h["Authorization"])
		}
		if h["Authorization"] == "" {
			t.Fatalf("%s: 缺少 Authorization 头", c.name)
		}
	}
}

// 空 security_token 不该影响其它必需头。
func TestSignRequestKeepsRequiredHeaders(t *testing.T) {
	cred := &Credential{AccessKeyID: "AKID", SecretAccessKey: "SECRET"}
	h, err := signRequestHuawei(cred, http.MethodPost,
		"https://snap-access.cn-north-4.myhuaweicloud.com/v1/ops/claim", `{"campaignId":"x"}`)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	for _, k := range []string{"host", "x-sdk-date", "x-sdk-content-sha256", "content-type", "Authorization"} {
		if h[k] == "" {
			t.Errorf("缺少必需头 %s", k)
		}
	}
	// GET 不带 body，按协议不该有 content-type
	hg, _ := signRequestHuawei(cred, http.MethodGet, "https://h.example/a/b", "")
	if _, ok := hg["content-type"]; ok {
		t.Error("GET 请求不该带 content-type")
	}
}

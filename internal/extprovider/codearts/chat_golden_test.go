package codearts

import (
	"strings"
	"testing"
)

// TestSignChatMatchesOfficialVector 对话签名的**黄金测试**。
//
// 输入与期望值逐字取自参考实现 agent2api 的 `codearts/signer_vectors.json`
// 里名为 `reference-vector` 的那一条（该条 note 写着「signer_test.go 对照
// 官方测试向量」）。
//
// 这条测试不能省：CodeArts 对话**没有任何账号可以端到端验证**，而签名算错的
// 唯一表现是上游稳定回 `401 verify ak sk signature fail`——那个错误与
// 「AK/SK 填错」「时钟不同步」「头集合不对」长得一模一样，事后完全无法定位。
// 有了官方向量，至少能证明规范请求串、参与签名的头集合、string_to_sign、
// 最终签名四样东西全对。
func TestSignChatMatchesOfficialVector(t *testing.T) {
	headers := map[string]string{"X-Domain-Id": "dom", "X-Language": "en-us", "X-Sdk-Date": "20260102T030405Z", "X-Security-Token": "tok", "content-type": "application/json", "plugin-name": "snap_vscode"}
	body := []byte("{\"model\":\"x\"}")

	canonical, signedNames, sig := signChatCanonical("POST",
		"/api/v2/chat/completions", "",
		headers, body,
		"20260102T030405Z", "AKIDEXAMPLE", "secret")

	wantCanonical := "POST\n/api/v2/chat/completions/\n\ncontent-type:application/json\nplugin-name:snap_vscode\nx-domain-id:dom\nx-language:en-us\nx-sdk-date:20260102T030405Z\nx-security-token:tok\n\ncontent-type;plugin-name;x-domain-id;x-language;x-sdk-date;x-security-token\n78b0e11c6754fc9671c2034f4a068d3a0e53c1c22a5a33900dd46bc8cd9136f3"
	wantSigned := "content-type;plugin-name;x-domain-id;x-language;x-sdk-date;x-security-token"
	wantSToSign := "SDK-HMAC-SHA256\n20260102T030405Z\n80a347b7e5daed7afe5cfe8f08b8092abcb3a20841d79dc7f22417ec9f192253"
	wantSig := "0093888a0805c8535df7212bd72024fea1570eabb927853f44a4d0827204bac4"

	if signedNames != wantSigned {
		t.Errorf("参与签名的头集合不符\n got: %s\nwant: %s", signedNames, wantSigned)
	}
	if canonical != wantCanonical {
		t.Errorf("规范请求串不符\n got:\n%s\nwant:\n%s", canonical, wantCanonical)
	}
	sts := "SDK-HMAC-SHA256\n20260102T030405Z\n" + sha256Hex(canonical)
	if sts != wantSToSign {
		t.Errorf("string_to_sign 不符\n got: %s\nwant: %s", sts, wantSToSign)
	}
	if sig != wantSig {
		t.Errorf("签名不符\n got: %s\nwant: %s", sig, wantSig)
	}

	// 反向断言：这两条正是「对话」与「区域 API」两套口径的分歧点。
	// 照抄区域 API 那套（signRequestHuawei 恒签 host + x-sdk-content-sha256）
	// 到这条链路上，得到的必然是稳定 401。
	if hasHeader(signedNames, "host") {
		t.Error("对话链路不该把 host 签进去（参考实现 include_host=false）")
	}
	if hasHeader(signedNames, "x-sdk-content-sha256") {
		t.Error("对话链路不该把 x-sdk-content-sha256 签进去")
	}
}

// hasHeader 判断 `;` 连接的头名集合里有没有某个名字（全小写比较）。
func hasHeader(joined, name string) bool {
	for _, p := range strings.Split(joined, ";") {
		if strings.EqualFold(p, name) {
			return true
		}
	}
	return false
}

// TestSignChatAddsDateAndTokenWithoutHost 生产路径的形状检查：
// x-sdk-date 必须自己注入、有 STS token 时带上、且 host 不进签名集合。
func TestSignChatAddsDateAndTokenWithoutHost(t *testing.T) {
	cred := &Credential{AccessKeyID: "AK", SecretAccessKey: "SK", SecurityToken: "sts-token"}
	in := map[string]string{"Content-Type": "application/json", "plugin-name": DefaultPluginName}
	out, err := signChat(cred, "POST", SnapEngineURL+ChatPath, []byte(`{"model":"x"}`), in)
	if err != nil {
		t.Fatalf("signChat: %v", err)
	}
	if out["x-sdk-date"] == "" {
		t.Error("x-sdk-date 没注入")
	}
	if out["x-security-token"] != "sts-token" {
		t.Error("有 STS token 时必须带上并参与签名")
	}
	auth := out["Authorization"]
	if !strings.HasPrefix(auth, "SDK-HMAC-SHA256 Access=AK,SignedHeaders=") {
		t.Errorf("Authorization 形状不对: %s", auth)
	}
	signed := strings.Split(auth, "SignedHeaders=")[1]
	signed = strings.Split(signed, ",")[0]
	if hasHeader(signed, "host") {
		t.Errorf("host 不该被签进去: %s", signed)
	}
	// 三个模型头的等价物：Content-Type 与 plugin-name 必须在集合里
	for _, want := range []string{"content-type", "plugin-name", "x-sdk-date", "x-security-token"} {
		if !hasHeader(signed, want) {
			t.Errorf("签名集合缺 %s: %s", want, signed)
		}
	}
	_ = in
}

// TestSignChatAgentChannelNoModelHeaders 两条通道的签名头集合必须**不同**：
// agent 通道（老套餐模型）不含 model-id 三头，benefit 通道（免费模型）必须含。
// 这是「踩空后表现很像没权限」的坑——漏三头上游报 "model is not registered"，
// 老套餐模型带三头反而 "unsupported model"（参考项目两条通道严格区分的实证）。
func TestSignChatAgentChannelNoModelHeaders(t *testing.T) {
	agent := DefaultChatProfile().headers("openpangu-2.0-pro", false)
	for _, k := range []string{"model-id", "model-name", "x-model-id"} {
		if _, ok := agent[k]; ok {
			t.Errorf("agent 通道签名头集合不该含 %s（会触发 unsupported model）: %+v", k, agent)
		}
	}
	benefit := DefaultChatProfile().headers("deepseek-v4-flash-0731", true)
	for _, k := range []string{"model-id", "model-name", "x-model-id"} {
		if v, ok := benefit[k]; !ok || v != "deepseek-v4-flash-0731" {
			t.Errorf("benefit 通道必须把 %s 签进去且值等于模型名，实际: %+v", k, benefit)
		}
	}
}

// TestIsBenefitModelRouting 兜底目录里的免费模型判 benefit，老套餐判 agent；
// 未知新模型名保守按 benefit（与参考项目 AGENT_MODELS 判据一致）。
func TestIsBenefitModelRouting(t *testing.T) {
	cases := map[string]bool{
		"openpangu-2.0-pro":    false,
		"openpangu-2.0-flash":  false,
		"GLM-5.2":              false,
		"deepseek-v4-flash-0731": true,
		"deepseek-v4-pro-0813": true,
		"glm-5.3-flash":        true,
		"never-seen-model-x":   true,
	}
	for name, want := range cases {
		if got := IsBenefitModel(name); got != want {
			t.Errorf("IsBenefitModel(%q) = %v, want %v", name, got, want)
		}
	}
}

package copilot

import "testing"

// TestChatUAFollowsPluginVersion chatUA 由插件版本推导，两条都要钉住：
// 默认档必须逐字等于改成可配之前写死的 "GitHubCopilotChat/0.26.7"（否则线上
// UA 悄悄变了没人察觉），覆盖插件版本时 UA 必须同步——两个头自相矛盾比旧版本
// 更容易被上游判成异常客户端。
func TestChatUAFollowsPluginVersion(t *testing.T) {
	defer SetFingerprints("", "")

	if got := chatUA(); got != "GitHubCopilotChat/0.26.7" {
		t.Errorf("默认 UA=%q，与改造前写死值不一致", got)
	}
	if _, plugin := SetFingerprints("", "copilot-chat/0.28.0"); plugin != "copilot-chat/0.28.0" {
		t.Fatalf("插件版本未生效: %s", plugin)
	}
	if got := chatUA(); got != "GitHubCopilotChat/0.28.0" {
		t.Errorf("UA 未跟随插件版本: %s", got)
	}
	if ed, _ := SetFingerprints("vscode/1.90.0", ""); ed != "vscode/1.90.0" {
		t.Errorf("编辑器版本未生效: %s", ed)
	}
	// 空值 = 回落内置默认，不是保留上一次的覆盖。
	SetFingerprints("", "")
	if chatEditorVer != "vscode/1.99.3" || chatPluginVer != "copilot-chat/0.26.7" {
		t.Errorf("空值未回落默认: %s / %s", chatEditorVer, chatPluginVer)
	}
}

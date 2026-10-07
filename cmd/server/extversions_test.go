package main

import (
	"strings"
	"testing"

	"github.com/chipchipss/buddyhub/internal/extprovider/accio"
	"github.com/chipchipss/buddyhub/internal/extprovider/autoclaw"
	"github.com/chipchipss/buddyhub/internal/extprovider/cline"
	"github.com/chipchipss/buddyhub/internal/extprovider/codearts"
	"github.com/chipchipss/buddyhub/internal/extprovider/loomy"
	"github.com/chipchipss/buddyhub/internal/extprovider/qclaw"
	"github.com/chipchipss/buddyhub/internal/extprovider/raccoon"
	"github.com/chipchipss/buddyhub/internal/extprovider/trae"
	"github.com/chipchipss/buddyhub/internal/extprovider/traework"
)

// defaultExtFingerprintLines 空 config 下应打印的生效指纹 = 各通道内置默认档。
// 抬默认版本时这张表会红，逼着改动被看见（而不是悄悄换掉上游看到的客户端形态）。
var defaultExtFingerprintLines = []string{
	"accio.app_version=0.32.6",
	"autoclaw.client_version=1.18.5",
	"cline.client_version=3.0.62",
	"codearts.plugin_version=26.9.101",
	"copilot.chat_editor_version=vscode/1.99.3",
	"copilot.chat_plugin_version=copilot-chat/0.26.7",
	"loomy.client_version=1.0.0",
	"marvis.client_version=1.0.0.10371",
	"marvis.client_platform_version=1.0.0.10634",
	"qclaw.web_version=1.4.0",
	"qclaw.client_version=0.2.36.629",
	"raccoon.client_version=v1.0.35",
	"trae.ide_version=0.1.61",
	"trae.ide_version_code=20260820",
	"traework.ide_version=0.1.56",
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("生效指纹不符\n got: %v\nwant: %v", got, want)
	}
}

func TestApplyExtVersionsBuiltInDefaults(t *testing.T) {
	assertLines(t, applyExtVersions(&Config{}), defaultExtFingerprintLines)
	// 返回值之外，落到通道全局的必须是同一个值（出站头就读这些变量）。
	if trae.IDEVersion != "0.1.61" || autoclaw.ClientVersion != "1.18.5" {
		t.Errorf("通道全局未回落默认: trae=%s autoclaw=%s", trae.IDEVersion, autoclaw.ClientVersion)
	}
}

// TestApplyExtVersionsOverrides config 逐项覆盖后，生效清单与通道全局同步改写。
func TestApplyExtVersionsOverrides(t *testing.T) {
	defer applyExtVersions(&Config{}) // 复位，避免污染同包其它测试

	cfg := &Config{}
	cfg.ExtVersions.AccioAppVersion = "0.99.1"
	cfg.ExtVersions.AutoClawClientVersion = "99.0.0"
	cfg.ExtVersions.ClineClientVersion = "9.9.9"
	cfg.ExtVersions.CodeArtsPluginVersion = "99.99.99"
	cfg.ExtVersions.CopilotChatEditor = "vscode/1.0.0"
	cfg.ExtVersions.CopilotChatPlugin = "copilot-chat/1.0.0"
	cfg.ExtVersions.LoomyClientVersion = "9.9.9"
	cfg.ExtVersions.MarvisClientVersion = "9.0.0.0"
	cfg.ExtVersions.MarvisPlatformVersion = "9.0.0.1"
	cfg.ExtVersions.QClawWebVersion = "9.9.9"
	cfg.ExtVersions.QClawClientVersion = "9.9.9.9"
	cfg.ExtVersions.RaccoonClientVersion = "v9.9.9"
	cfg.ExtVersions.TraeIDEVersion = "9.9.9"
	cfg.ExtVersions.TraeIDEVersionCode = "99999999"
	cfg.ExtVersions.TraeWorkIDEVersion = "9.9.9"

	assertLines(t, applyExtVersions(cfg), []string{
		"accio.app_version=0.99.1",
		"autoclaw.client_version=99.0.0",
		"cline.client_version=9.9.9",
		"codearts.plugin_version=99.99.99",
		"copilot.chat_editor_version=vscode/1.0.0",
		"copilot.chat_plugin_version=copilot-chat/1.0.0",
		"loomy.client_version=9.9.9",
		"marvis.client_version=9.0.0.0",
		"marvis.client_platform_version=9.0.0.1",
		"qclaw.web_version=9.9.9",
		"qclaw.client_version=9.9.9.9",
		"raccoon.client_version=v9.9.9",
		"trae.ide_version=9.9.9",
		"trae.ide_version_code=99999999",
		"traework.ide_version=9.9.9",
	})
	for _, c := range []struct {
		label    string
		got, exp string
	}{
		{"accio.app_version", accio.AppVersion, "0.99.1"},
		{"autoclaw.client_version", autoclaw.ClientVersion, "99.0.0"},
		{"cline.client_version", cline.ClientVersion, "9.9.9"},
		{"codearts.plugin_version", codearts.DefaultPluginVersion, "99.99.99"},
		{"loomy.client_version", loomy.ClientVersion, "9.9.9"},
		{"qclaw.web_version", qclaw.WebVersion, "9.9.9"},
		{"qclaw.client_version", qclaw.ClientVersion, "9.9.9.9"},
		{"raccoon.client_version", raccoon.ClientVersion, "v9.9.9"},
		{"trae.ide_version", trae.IDEVersion, "9.9.9"},
		{"trae.ide_version_code", trae.IDEVersionCode, "99999999"},
		{"traework.ide_version", traework.IDEVersion, "9.9.9"},
	} {
		if c.got != c.exp {
			t.Errorf("%s=%q want %q", c.label, c.got, c.exp)
		}
	}
}

// TestApplyExtVersionsBlankKeepsDefault 空串/纯空白 = 逐项回落内置默认，
// 这样 config 里留着空键也不会把某个通道的指纹清成空头发给上游。
func TestApplyExtVersionsBlankKeepsDefault(t *testing.T) {
	defer applyExtVersions(&Config{})

	cfg := &Config{}
	cfg.ExtVersions.TraeIDEVersion = "   "
	cfg.ExtVersions.AutoClawClientVersion = ""
	assertLines(t, applyExtVersions(cfg), defaultExtFingerprintLines)
}

// TestExtVersionsJSONTags config 里的键名必须真的落到结构体字段上：json 标签写错
// 不会报错，只会静默用回内置默认——"改了没生效" 正是这次改动要消灭的东西，所以
// 15 个键逐个钉住。
func TestExtVersionsJSONTags(t *testing.T) {
	defer applyExtVersions(&Config{})

	raw := []byte(`{"ext_versions":{
		"accio_app_version":"a1","autoclaw_client_version":"a2","cline_client_version":"a3",
		"codearts_plugin_version":"a4","copilot_chat_editor_version":"a5","copilot_chat_plugin_version":"a6",
		"loomy_client_version":"a7","marvis_client_version":"a8","marvis_client_platform_version":"a9",
		"qclaw_client_version":"a10","qclaw_web_version":"a11","raccoon_client_version":"a12",
		"trae_ide_version":"a13","trae_ide_version_code":"a14","traework_ide_version":"a15"}}`)
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	lines := strings.Join(applyExtVersions(cfg), "\n")
	for _, want := range []string{
		"accio.app_version=a1",
		"autoclaw.client_version=a2",
		"cline.client_version=a3",
		"codearts.plugin_version=a4",
		"copilot.chat_editor_version=a5",
		"copilot.chat_plugin_version=a6",
		"loomy.client_version=a7",
		"marvis.client_version=a8",
		"marvis.client_platform_version=a9",
		"qclaw.web_version=a11",
		"qclaw.client_version=a10",
		"raccoon.client_version=a12",
		"trae.ide_version=a13",
		"trae.ide_version_code=a14",
		"traework.ide_version=a15",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("config 键未生效: 缺 %q\n生效清单:\n%s", want, lines)
		}
	}
}

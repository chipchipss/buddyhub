// zai.go 装配 Z.AI / ZCode 账号池。
//
// 组成：存储（data/zai-accounts.json）→ 旧配置 key 导入 → 选号池 → 验证码预解池
// → 请求编排器（Plan JWT 优先，API Key 回退）。
//
// 旧配置兼容：`schedule.zai.zai_keys` / `bigmodel_keys` 在**账号池为空时**导入为
// 账号（各带独立设备指纹）。导入后以账号池为准——面板是唯一管理入口，配置里的
// key 数组不再被读取（避免两处真相打架）。
package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/zai"
)

// buildZaiStack 装配账号池；返回 (编排器, 验证码池)。未配置任何账号时返回 nil, nil
// （zai: 通道保持禁用，模型列表不列、请求返回明确提示）。
func buildZaiStack(cfg *Config) (*zai.Client, *zai.CaptchaManager) {
	zc := cfg.Schedule.Zai

	accountsPath := strings.TrimSpace(zc.AccountsFile)
	if accountsPath == "" {
		accountsPath = stateSibling(cfg.StateFile, "zai-accounts.json")
	}
	store, err := zai.NewStore(accountsPath)
	if err != nil {
		log.Printf("[zai] 账号池读取失败（%s）：%v —— zai: 通道禁用", accountsPath, err)
		return nil, nil
	}

	imported := importLegacyZaiKeys(store, zc.ZaiKeys, zc.BigModelKeys)
	if imported > 0 {
		log.Printf("[zai] 已从 config 导入 %d 个旧版 Key 为账号（此后以账号池为准：%s）", imported, accountsPath)
	}

	accounts := store.List()
	if len(accounts) == 0 {
		// 池为空也要把编排器建起来：面板要能添加第一个账号（否则先有鸡还是先有蛋）。
		// zai: 通道对外是否可用由 HasSelectable 决定（模型列表与请求路径各自判定）。
		log.Printf("[zai] 账号池为空 —— 可在面板「自动化 → Z.AI」添加 JWT 或 API Key（%s）", accountsPath)
	}

	maxConc := zc.MaxConcurrency
	if maxConc <= 0 {
		maxConc = 2 // 官方客户端单账号并发默认 2
	}
	pool := zai.NewPool(store, maxConc)

	// 验证码预解池：只有存在可走 Plan 通道的账号时才预热（避免空转产生上游流量）
	gate := func() bool {
		for _, a := range store.List() {
			if a.HasJWTPath() && a.Selectable(time.Now()) {
				return true
			}
		}
		return false
	}
	captcha := zai.NewCaptchaManager(zai.SolverConfig{
		Command: zc.CaptchaCommand,
		Script:  zc.CaptchaSolver,
		Timeout: secondsToDuration(zc.CaptchaTimeoutSec),
		PoolMin: zc.CaptchaPoolMin,
		PoolMax: zc.CaptchaPoolMax,
	}, gate)
	captcha.Start()

	blocks := zai.LoadSystemBlocks(zc.SystemFile)
	// 身份块是「走 Plan 通道的 JWT 账号」的前提，所以只在真有这样账号时才警告。
	// 原先无条件打这条：没配 zai 的部署每次启动多两行废话（这条 + 就绪:0 个账号），
	// 而 0 账号时 3012 这个后果根本不可能发生——警告的适用条件不成立。
	if len(blocks) == 0 && gate() {
		log.Printf("[zai] 未配置 Plan 通道身份块（schedule.zai.system_file）：JWT 账号可能被上游拒为 3012")
	}

	client := zai.NewClient(pool, captcha, blocks)

	// 后台额度轮询（错峰）：只刷 JWT 账号，默认 5 分钟一轮；0 = 关闭。
	// 上游 WAF 对 billing 族连续查询敏感，间隔别设太密。
	interval := zc.QuotaRefreshMinutes
	if interval == 0 {
		interval = 5
	}
	if interval > 0 {
		stopQuota := client.StartQuotaLoop(time.Duration(interval) * time.Minute)
		_ = stopQuota // 进程退出即结束，无需显式停止
	}

	// 后台套餐领取轮：每轮 激活上报 → preview → 逐个领取全部可领套餐。
	// 1005 名额用完按上游 next_at 退避；默认 10 分钟一轮，0 = 关闭。
	claimMin := zc.ClaimRoundMinutes
	if claimMin == 0 {
		claimMin = 10
	}
	if claimMin > 0 {
		stopClaim := client.StartClaimLoop(time.Duration(claimMin) * time.Minute)
		_ = stopClaim
	}

	// 空池时上面那条「账号池为空」已经说完了（含往哪加账号），这条纯重复；
	// 打出来只会让没启用 zai 的部署每次启动多一行 0 个账号。
	if len(accounts) > 0 {
		log.Printf("[zai] 账号池就绪：%d 个账号（%s）· 单账号并发 %d · 验证码求解器 %s · 额度轮询 %d 分钟 · 领取轮 %d 分钟",
			len(accounts), accountsPath, maxConc, enabledWord(captcha.Enabled()), interval, claimMin)
	}
	return client, captcha
}

// importLegacyZaiKeys 把旧配置里的 key 数组导入账号池（幂等：同名账号已存在则跳过）。
// 返回新导入的数量。
func importLegacyZaiKeys(store *zai.Store, zaiKeys, bigModelKeys []string) int {
	existing := map[string]bool{}
	for _, a := range store.List() {
		existing[a.Name] = true
	}
	n := 0
	add := func(name, key, provider string) {
		key = strings.TrimSpace(key)
		if key == "" || existing[name] {
			return
		}
		a := zai.NewAccount(name, key)
		a.Provider = provider
		if provider == zai.ProviderBigModel {
			// 智谱开放平台只有 API Key，不参与 Plan 通道
			a.Mode = zai.ModeAPIKey
			a.JWT = ""
			a.APIKey = key
		}
		if err := store.Add(a); err == nil {
			existing[name] = true
			n++
		}
	}
	for i, k := range zaiKeys {
		add(fmt.Sprintf("zai-key-%d", i+1), k, zai.ProviderZai)
	}
	for i, k := range bigModelKeys {
		add(fmt.Sprintf("bigmodel-key-%d", i+1), k, zai.ProviderBigModel)
	}
	return n
}

func enabledWord(on bool) string {
	if on {
		return "已启用"
	}
	return "未配置（Plan 通道不可用，仅走 API Key 回退）"
}

// secondsToDuration 秒 → Duration（<=0 表示用缺省值）。
func secondsToDuration(sec int) time.Duration {
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

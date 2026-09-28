// Package panel: loomy_renew.go — Loomy 无人值守续期循环。
//
// 语义（对齐 loomy2api 的 session_renew_before_days=3）：session 14 天、
// 无刷新 token。每 6h 扫一遍外部池的 loomy-cli 账号：
//   - 有存密密码 且 （剩余 <3 天 或 登录时间未知）→ 用存量密码静默重登，
//     新 session 写回外部池（replaceCred）并双写 data/loomy-session.json
//     （FindLoomySession 第 0 优先级来源，Loomy 页任务/签到依赖）。
//   - 无密码账号跳过（过期需手动重登，面板可见）。
//
// 重登失败不中断循环：记日志，下轮再试；401 触发的即时重登由 loomy 桥负责。
package panel

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/loomy"
	"github.com/chipchipss/buddyhub/internal/extstore"
	"github.com/chipchipss/buddyhub/internal/upstream"
)

const loomyRenewInterval = 6 * time.Hour

var loomyRenewOnce sync.Once

// StartLoomyRenewLoop 启动后台续期（进程级单例；main 装配时调用一次）。
func StartLoomyRenewLoop(p *Panel) {
	loomyRenewOnce.Do(func() {
		go func() {
			// 启动后 5 分钟先跑一轮（新装场景尽快校验存量凭据），
			// 之后固定周期。
			t := time.NewTimer(5 * time.Minute)
			for range t.C {
				LoomyRenewPass(p)
				t.Reset(loomyRenewInterval)
			}
		}()
	})
}

// LoomyRenewPass 单轮维护：扫描、按需重登、双写。暴露为可测函数。
func LoomyRenewPass(p *Panel) {
	mgr := p.extManager()
	if mgr == nil {
		return
	}
	cli := loomy.NewClient()
	for _, a := range mgr.List() {
		if a.Provider != extstore.PLoomyCLI || a.Disabled {
			continue
		}
		var cred upstream.LoomyCred
		if json.Unmarshal(a.Cred, &cred) != nil {
			continue
		}
		if !cred.NeedsRenew() {
			continue
		}
		plain, err := upstream.UnprotectPassword(cred.Password)
		if err != nil || plain == "" {
			log.Printf("loomy-renew: %s 密码不可用（%v），跳过自动续期", a.ID, err)
			continue
		}
		res, err := cli.LoginByPassword(cred.Phone, plain, nil)
		if err != nil {
			log.Printf("loomy-renew: %s 自动重登失败: %v", a.ID, err)
			continue
		}
		fresh := upstream.LoomyCred{
			Session:  res.Session,
			UserID:   res.UserID,
			Phone:    res.Phone,
			Password: cred.Password, // 存量密文原样保留（DPAPI 密文与账号无关）
			LoginAt:  time.Now().Unix(),
		}
		raw, _ := json.Marshal(fresh)
		mgr.ReplaceCred(a.Provider, a.ID, raw)
		// 双写 session 文件（该文件是单号文件：多号时写最后一个成功的，
		// 与面板登录行为一致——外部池始终是权威全量）。
		if err := upstream.SaveLoomySession(&upstream.LoomySession{
			Session: res.Session,
			UserID:  res.UserID,
			Phone:   res.Phone,
		}); err != nil {
			log.Printf("loomy-renew: %s session 文件写入失败: %v", a.ID, err)
		}
		log.Printf("loomy-renew: %s 已自动续期（新 session 14 天）", a.ID)
	}
}

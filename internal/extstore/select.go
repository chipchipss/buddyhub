package extstore

// select.go 外部平台账号的**选择与健康**。
//
// ── 为什么不能照搬腾讯 pool 那套 ────────────────────────────────────
// `internal/pool` 是一整套状态机：健康/软冷却/熔断/连败降权 + 在途租约 +
// 三因子加权 + 6004 模型级独立冷却 + 成本档位探索 + state.json + Redis 镜像。
// 外部平台一条都不适用：
//   - 每平台通常只有 1–3 个号，且**同构**（同一订阅档），加权没有可分化的依据；
//   - 没有 6004/11102 这类按模型的上游语义，模型级冷却无从谈起；
//   - 签到按天幂等，"成本档探索"这类为计费优化的机制没有意义；
//   - 上游 QPS 极低（面板操作 + 偶发对话），在途租约防的是高并发抢号。
// 硬套只会引入一堆**无法验证**的复杂度（没有账号可以压测）。
//
// ── 真正要解决的两件事 ─────────────────────────────────────────────
//  1. **流量全落在第一个号上**：桥接都是 `for _, a := range accounts` 且**成功即停**，
//     而 ExtList 按 provider+label 排序——固定顺序。有 3 个号时 100% 打第一个。
//  2. **挂掉的号每次请求都被重试**：失败即 continue 下一个，但**没有失败记忆**，
//     下一次请求还是从它开始，每次都白打一发上游。
//
// 对应解法：
//   - 轮转序（rotation counter）→ 负载自然摊开
//   - 失败冷却（指数退避）→ 挂掉的号自动退到队尾，冷却期内不再被首选
//   - 冷却中的号**仍然排在后面而不是剔除**（兜底）：只有它们可用时仍能试一把，
//     与腾讯池「全冷却兜底取最早到期者」同语义
//
// 状态是**进程内**的（重启清零）。可接受：冷却是分钟级的，而重启本身会让
// 所有号重新探活一次；为此把状态并进 ext-accounts.json 反而会让「账号表」
// 同时承担路由与健康两份职责，出错时更难排查。
import (
	"sort"
	"sync"
	"time"
)

// 冷却退避参数。30s → 2m → 8m → 30m：既能快速绕开一次抖动，
// 又不至于让一个持续失败的号被反复撞；30 分钟封顶保证它不会被永久遗忘
// （真正的永久问题是凭据失效，那要人去重新登录，不是冷却能解决的）。
const (
	coolBase = 30 * time.Second
	coolMax  = 30 * time.Minute
)

// health 单个外部账号的运行时健康。
type health struct {
	fails int       // 连续失败次数（驱动退避档位）
	until time.Time // 冷却截止；零值 = 不在冷却
}

// selectState 整个 Manager 的选择状态。
type selectState struct {
	mu  sync.Mutex
	rr  map[string]uint64 // provider → 轮转计数
	acc map[string]health // "provider/id" → 健康
}

func (m *Manager) selState() *selectState {
	m.selOnce.Do(func() { m.sel = &selectState{rr: map[string]uint64{}, acc: map[string]health{}} })
	return m.sel
}

// NoteChatResult 记一次对话结果。
//
// `err == nil` 记成功（清退避）；非 nil 记失败并按指数退避进入冷却。
// 调用方是各桥接的账号循环：失败那一支与成功那一支都要调。
//
// **为什么失败要立刻冷却**：桥接是「失败就 continue 换下一个」，没有失败记忆
// 的话下一次请求又会先撞同一个号——每发请求白烧一次上游往返。
func (m *Manager) NoteChatResult(provider, id string, err error) {
	if provider == "" || id == "" {
		return
	}
	s := m.selState()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + "/" + id
	h := s.acc[key]
	if err == nil {
		h.fails = 0
		h.until = time.Time{}
		s.acc[key] = h
		return
	}
	h.fails++
	if h.fails > 16 {
		h.fails = 16 // 只是防 1<<n 溢出：退避已封顶
	}
	// 退避 = coolBase << (fails-1)：30s → 1m → 2m → 4m → 8m → 16m → 30m（封顶）。
	// 2 的幂次便于一眼看出档位，封顶保证**被持续失败的号不会被永久遗忘**——
	// 真正的永久问题是凭据失效，那要人去重新登录，冷却解决不了。
	d := coolBase << (h.fails - 1)
	if d > coolMax || d <= 0 {
		d = coolMax
	}
	h.until = time.Now().Add(d)
	s.acc[key] = h
}

// SelectChatOrder 把账号按**首选顺序**排好返回。
//
// 顺序：健康号（稳定排序后按轮转位偏移）→ 冷却中的号（按到期时间升序，
// 即最早解禁的排最前）。Disabled 不在这层过滤——桥接各自判断（那是用户显式
// 停用，与健康无关，语义不同）。
//
// ── 为什么是「偏移」而不是按使用时间排序 ──────────────────────────────
// 两个更朴素的写法都不成立：
//
//	· 按 `used` 做 LRU —— 这一层拿不到"谁真正被用了"（桥接成功即停，
//	  没用到的号也在这次调用里被标了 used），等于所有号同一时刻，白排；
//	· 按 `hash(id) + rr % n` 轮转 —— 散列撞 mod n 时**三个号会永远同序**，
//	  轮转静默失效（这种失效没有任何报错，只能靠测试抓）。
//
// 显式偏移 `rr % len` 是教科书写法：稳定排序 + 转位，必然每轮换一个起始号，
// 没有碰撞分支。
//
// **有副作用**：调用即推进轮转计数，所以一次请求只该调一次（桥接在按
// provider 过滤前调即可）。
func (m *Manager) SelectChatOrder(all []*ExtAccount) []*ExtAccount {
	if len(all) == 0 {
		return all
	}
	now := time.Now()
	s := m.selState()
	s.mu.Lock()
	defer s.mu.Unlock()

	// 按 provider 分组，各组独立轮转——一个平台挂了不该让别的平台跟着错位。
	byProvider := map[string][]*ExtAccount{}
	var order []string
	for _, a := range all {
		if a == nil {
			continue
		}
		if _, ok := byProvider[a.Provider]; !ok {
			order = append(order, a.Provider)
		}
		byProvider[a.Provider] = append(byProvider[a.Provider], a)
	}
	sort.Strings(order) // 组间顺序固定，避免 provider 次序每次请求都跳

	var out []*ExtAccount
	for _, prov := range order {
		group := byProvider[prov]
		type item struct {
			a    *ExtAccount
			key  string
			h    health
			cool bool
		}
		items := make([]item, 0, len(group))
		for _, a := range group {
			key := prov + "/" + a.ID
			h := s.acc[key]
			items = append(items, item{a: a, key: key, h: h,
				cool: !h.until.IsZero() && h.until.After(now)})
		}
		var healthy, cooling []item
		for _, it := range items {
			if it.cool {
				cooling = append(cooling, it)
			} else {
				healthy = append(healthy, it)
			}
		}
		// 健康号：按 key 稳定排序 → 按轮转位转位。
		sort.Slice(healthy, func(i, j int) bool { return healthy[i].key < healthy[j].key })
		if n := len(healthy); n > 1 {
			start := int(s.rr[prov] % uint64(n))
			healthy = append(healthy[start:], healthy[:start]...)
		}
		// 冷却号：最早解禁的排最前（兜底时优先用"差一点就恢复"的）
		sort.Slice(cooling, func(i, j int) bool {
			return cooling[i].h.until.Before(cooling[j].h.until)
		})

		s.rr[prov]++
		for _, it := range healthy {
			out = append(out, it.a)
		}
		for _, it := range cooling {
			out = append(out, it.a)
		}
	}
	return out
}

// HealthSnapshot 单个账号的健康摘要（供面板 /status 或排障展示）。
type HealthSnapshot struct {
	Provider    string `json:"provider"`
	ID          string `json:"id"`
	FailStreak  int    `json:"fail_streak"`
	CooldownSec int    `json:"cooldown_sec,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// ChatHealth 该平台账号的健康快照（按 id 排序；只列有记录的）。
func (m *Manager) ChatHealth(provider string) []HealthSnapshot {
	s := m.selState()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var out []HealthSnapshot
	for key, h := range s.acc {
		if splitProvider(key) != provider {
			continue
		}
		snap := HealthSnapshot{
			Provider:   provider,
			ID:         key[len(provider)+1:],
			FailStreak: h.fails,
		}
		if h.until.After(now) {
			snap.CooldownSec = int(h.until.Sub(now).Seconds())
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func splitProvider(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i]
		}
	}
	return ""
}

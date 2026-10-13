// 清单 38：逐 API Key 调用量（最近使用 / 共几次），供面板判断「这把删了安不安全」。
// 这些测试钉住四条口径：累计与单调、落盘恢复、排序稳定、旧文件缺 keys 字段仍能加载。
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAddKeyCountsAndMonotonicLast(t *testing.T) {
	r := New("")
	now := time.Now()
	r.AddKey("k1", true, now)
	r.AddKey("k1", false, now.Add(time.Second))
	// 乱序晚到的旧时间戳不能把「最近使用」拨回去——一把刚被调用的 Key 显示
	// 「三天没动」，用户就会把它删掉。
	r.AddKey("k1", true, now.Add(-time.Hour))
	// 空 id（未配置鉴权）必须空操作，不产生幽灵行。
	r.AddKey("", true, now)

	s := r.Snapshot(24, nil)
	if len(s.ByKey) != 1 {
		t.Fatalf("by_key = %+v, want 仅 k1 一条（空 id 不计数）", s.ByKey)
	}
	k := s.ByKey[0]
	if k.ID != "k1" || k.Calls != 3 || k.Errors != 1 {
		t.Fatalf("k1 = %+v, want calls 3 / errors 1", k)
	}
	if k.Last != now.Add(time.Second).UnixMilli() {
		t.Fatalf("last = %d, want %d（单调：旧时间戳不得回拨）", k.Last, now.Add(time.Second).UnixMilli())
	}
}

// 落盘→新实例恢复：per-Key 计数重启不丢（和桶同一条 flush/load 路径）。
func TestAddKeyFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	last := time.Now().Add(-5 * time.Minute)
	r1 := New(path)
	r1.AddKey("ka", true, last)
	r1.AddKey("ka", false, last)
	r1.AddKey("kb", true, last)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(0, nil)
	if len(s.ByKey) != 2 {
		t.Fatalf("恢复后 by_key = %+v, want 2 条", s.ByKey)
	}
	for _, k := range s.ByKey {
		switch k.ID {
		case "ka":
			if k.Calls != 2 || k.Errors != 1 || k.Last != last.UnixMilli() {
				t.Fatalf("ka 恢复错位: %+v", k)
			}
		case "kb":
			if k.Calls != 1 || k.Errors != 0 {
				t.Fatalf("kb 恢复错位: %+v", k)
			}
		default:
			t.Fatalf("恢复出未知 id %q（明文/脏数据混进来了？）", k.ID)
		}
	}
	// usage.json 里只允许出现 id 摘要，任何原始 Key 形态都不该落盘。
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Keys) != 2 {
		t.Fatalf("落盘 keys 异常: err=%v keys=%+v", err, f.Keys)
	}
}

// 排序：调用量降序 → 同量按最近使用降序 → 同值按 id 升序（前端 diff 不抖）。
func TestSnapshotByKeySorted(t *testing.T) {
	r := New("")
	now := time.Now()
	r.AddKey("few", true, now)
	r.AddKey("many", true, now)
	r.AddKey("many", true, now)
	// idle：两次调用都在一小时前——同量但更久没用，应排在 many 之后。
	r.AddKey("idle", true, now.Add(-2*time.Hour))
	r.AddKey("idle", true, now.Add(-time.Hour))

	s := r.Snapshot(0, nil)
	want := []string{"many", "idle", "few"}
	if len(s.ByKey) != 3 {
		t.Fatalf("by_key = %+v, want 3 条", s.ByKey)
	}
	for i, id := range want {
		if s.ByKey[i].ID != id {
			t.Fatalf("排序 = %+v, want many→idle→few（calls desc, last desc）", s.ByKey)
		}
	}
}

// 旧版本 usage.json 没有 keys 字段：必须照常加载桶、by_key 为空——
// 升级第一启如果报错清零历史用量，等于把成本台账烧了。
func TestLoadLegacyFileWithoutKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	scope := "h:" + time.Now().Format(hourLayout)
	legacy := `{"version":1,"saved":"2026-01-01T00:00:00+08:00","buckets":[` +
		`{"s":"` + scope + `","r":"cn","u":"u1","m":"glm","q":7,"e":1,"p":10,"c":5,"t":15}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	s := r.Snapshot(0, nil)
	if s.Totals.Requests != 7 || s.Totals.TotalTokens != 15 {
		t.Fatalf("旧桶未恢复: %+v", s.Totals)
	}
	if len(s.ByKey) != 0 {
		t.Fatalf("by_key = %+v, want 空（旧文件无 keys 字段）", s.ByKey)
	}
	// 恢复后新记一把也能带着旧桶一起落盘。
	r.AddKey("knew", true, time.Now())
	r.Save()
	r2 := New(path)
	s2 := r2.Snapshot(0, nil)
	if len(s2.ByKey) != 1 || s2.ByKey[0].ID != "knew" || s2.Totals.Requests != 7 {
		t.Fatalf("混合格式往返异常: by_key=%+v totals=%+v", s2.ByKey, s2.Totals)
	}
}

// ── fork keyOwner 维度：按 Key 的小时桶 token 用量 ──────────────────────

// AddKeyUsage 同时喂 KeyStat（终身）与 keyBuckets（小时片），且 token 口径
// 与主桶一致：HasTotal 缺失时按 pt+ct 兜底，Has* 缺失不加。
func TestAddKeyUsageBucketsTokens(t *testing.T) {
	r := New("")
	now := time.Now()
	r.AddKeyUsage("k1", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true}, true, now)
	// 上游没给 total：兜底 pt+ct（与主桶同一口径）。
	r.AddKeyUsage("k1", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, false, now)
	// 失败且无 usage：只计请求数与失败数。
	r.AddKeyUsage("k1", Delta{}, false, now)
	// 空 id 空操作。
	r.AddKeyUsage("", Delta{PromptTokens: 9, HasPromptTokens: true}, true, now)

	s := r.Snapshot(0, nil)
	if len(s.ByKey) != 1 {
		t.Fatalf("by_key = %+v, want 仅 k1", s.ByKey)
	}
	k := s.ByKey[0]
	if k.Calls != 3 || k.Errors != 2 {
		t.Fatalf("k1 = %+v, want calls 3 / errors 2", k)
	}
	if k.PromptTokens != 110 || k.CompletionTokens != 55 || k.TotalTokens != 165 {
		t.Fatalf("k1 tokens = %d/%d/%d, want 110/55/165（total 按 pt+ct 兜底）",
			k.PromptTokens, k.CompletionTokens, k.TotalTokens)
	}

	// 小时片与 KeyStat 同源：当小时点应含同样的 token。
	pts := r.KeySeries("k1", 24)
	if len(pts) != 1 {
		t.Fatalf("series = %+v, want 1 个小时点", pts)
	}
	if pts[0].PromptTokens != 110 || pts[0].CompletionTok != 55 || pts[0].Requests != 3 || pts[0].Errors != 2 {
		t.Fatalf("小时点 = %+v, want pt110/ct55/req3/err2", pts[0])
	}
}

// KeySeries 窗口口径与 Snapshot 一致：窗口外小时点不进，全部历史含折叠日点。
func TestKeySeriesWindowAndAllHistory(t *testing.T) {
	r := New("")
	now := time.Now()
	r.AddKeyUsage("k1", Delta{PromptTokens: 5, HasPromptTokens: true}, true, now.Add(-48*time.Hour)) // 窗口(24h)外
	r.AddKeyUsage("k1", Delta{PromptTokens: 3, HasPromptTokens: true}, true, now)                    // 窗口内
	// 造一个 100 天前的折叠日桶（Rollup 产物形态）。
	dayOld := "d:" + now.AddDate(0, 0, -100).Format(dayLayout)
	r.keyBuckets["k1|"+dayOld] = &keyBucket{KeyID: "k1", Scope: dayOld, Req: 4, PT: 40, TT: 40}

	s := r.KeySeries("k1", 24)
	if len(s) != 1 || s[0].PromptTokens != 3 {
		t.Fatalf("24h 窗口 series = %+v, want 仅当前小时 pt3", s)
	}

	all := r.KeySeries("k1", 0)
	// 全部历史 = 折叠日点（100 天前）+ 两个小时点（48h 前与当前，按时间升序）。
	if len(all) != 3 || all[0].Scope != "day" {
		t.Fatalf("全部历史 series = %+v, want 日点在前 + 小时点在后（48h 前的点属于全部历史）", all)
	}
	if all[0].Requests != 4 || all[1].Requests != 1 || all[1].PromptTokens != 5 || all[2].Requests != 1 || all[2].PromptTokens != 3 {
		t.Fatalf("series 计数 = %+v, want 日4/小时(48h前)1pt5/小时(当前)1pt3", all)
	}

	// 窗口上限夹到 60 天；空 id / nil recorder 返回 nil。
	if got := r.KeySeries("k1", 24*60*10); len(got) == 0 {
		t.Fatal("超长窗口不应丢数据（夹到 60 天而非报错）")
	}
	if got := r.KeySeries("", 24); got != nil {
		t.Fatalf("空 id 应返回 nil, got %+v", got)
	}
	var nilR *Recorder
	if got := nilR.KeySeries("k1", 24); got != nil {
		t.Fatalf("nil recorder 应返回 nil, got %+v", got)
	}
}

// Rollup 对 key 桶同规则折叠且幂等（总量不变、小时桶消失、日桶出现）。
func TestKeyRollupFoldsKeyBuckets(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100)
	r.AddKeyUsage("k1", Delta{PromptTokens: 7, HasPromptTokens: true}, true, old)
	r.AddKeyUsage("k1", Delta{PromptTokens: 7, HasPromptTokens: true}, true, old)
	r.AddKeyUsage("k1", Delta{PromptTokens: 1, HasPromptTokens: true}, true, time.Now())

	before := r.KeySeries("k1", 0)
	r.Rollup(time.Now())
	after := r.KeySeries("k1", 0)

	// 折叠后：日点 1 个（两天前合计）+ 小时点 1 个（当前小时）。
	if len(after) != 2 || after[0].Scope != "day" || after[0].Requests != 2 || after[0].PromptTokens != 14 {
		t.Fatalf("折叠后 series = %+v, want 日点 req2/pt14 + 小时点", after)
	}
	// 幂等：总量不变（对比折叠前后全部历史的合计）。
	var sumReq func(pts []Point) int64
	sumReq = func(pts []Point) (n int64) {
		for _, p := range pts {
			n += p.Requests
		}
		return
	}
	if sumReq(before) != sumReq(after) || sumReq(after) != 3 {
		t.Fatalf("总量漂移: before=%d after=%d, want 3/3", sumReq(before), sumReq(after))
	}

	// 二次折叠幂等（总量与点数都不变）。
	r.Rollup(time.Now())
	again := r.KeySeries("k1", 0)
	if sumReq(again) != 3 || len(again) != 2 {
		t.Fatalf("二次折叠破坏幂等: %+v", again)
	}
}

// 落盘→恢复：key 小时桶重启不丢；旧文件缺 key_buckets 字段照常加载。
func TestKeyBucketFlushRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.AddKeyUsage("k1", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true, time.Now())
	r1.Save()

	r2 := New(path)
	pts := r2.KeySeries("k1", 24)
	if len(pts) != 1 || pts[0].PromptTokens != 42 {
		t.Fatalf("恢复后 key series = %+v, want pt42", pts)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || len(f.KeyBuckets) != 1 {
		t.Fatalf("落盘 key_buckets 异常: err=%v kb=%+v", err, f.KeyBuckets)
	}

	// 旧版文件（无 key_buckets 字段）加载后新记一把，混合格式往返正常。
	legacy := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"version":1,"saved":"x","buckets":[],"keys":[{"id":"old","calls":1,"errors":0,"last":1}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r3 := New(legacy)
	if s := r3.Snapshot(0, nil); len(s.ByKey) != 1 || s.ByKey[0].ID != "old" {
		t.Fatalf("旧文件 keys 未恢复: %+v", s.ByKey)
	}
	r3.AddKeyUsage("new", Delta{PromptTokens: 1, HasPromptTokens: true}, true, time.Now())
	r3.Save()
	r4 := New(legacy)
	if got := r4.KeySeries("new", 24); len(got) != 1 || got[0].PromptTokens != 1 {
		t.Fatalf("混合格式 key 桶往返异常: %+v", got)
	}
}

// 恒等式：KeyStat 的终身 token == 该 Key 所有小时/日桶之和。
// 两处同源更新，任何一处漏加都会让面板「累计」与「时序」对不上账。
func TestKeyStatTokensMatchBucketSums(t *testing.T) {
	r := New("")
	now := time.Now()
	for i := 0; i < 5; i++ {
		r.AddKeyUsage("k1", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 4, HasCompletion: true}, i%2 == 0, now.Add(time.Duration(i)*time.Minute))
	}
	r.AddKeyUsage("k2", Delta{TotalTokens: 7, HasTotal: true}, true, now)

	for _, id := range []string{"k1", "k2"} {
		var pt, ct, tt int64
		for _, b := range r.keyBuckets {
			if b.KeyID != id {
				continue
			}
			pt += b.PT
			ct += b.CT
			tt += b.TT
		}
		k := r.keys[id]
		if k == nil {
			t.Fatalf("%s: KeyStat 缺失", id)
		}
		if k.PromptTokens != pt || k.CompletionTokens != ct || k.TotalTokens != tt {
			t.Fatalf("%s: KeyStat %d/%d/%d != 桶和 %d/%d/%d（两处不同源）",
				id, k.PromptTokens, k.CompletionTokens, k.TotalTokens, pt, ct, tt)
		}
	}
}

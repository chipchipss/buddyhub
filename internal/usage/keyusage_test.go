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

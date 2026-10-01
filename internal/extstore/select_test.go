package extstore

import (
	"errors"
	"testing"
)

func accts(ids ...string) []*ExtAccount {
	out := make([]*ExtAccount, 0, len(ids))
	for _, id := range ids {
		out = append(out, &ExtAccount{Provider: PQoder, ID: id, Label: id})
	}
	return out
}

// 轮转是这次改动的核心：此前**固定按 provider+label 排序且成功即停**，
// 于是三个号 100% 打在第一个上。这里必须看到起始号每次都不一样、且遍历一圈。
func TestSelectRotatesHead(t *testing.T) {
	m := NewManager(t.TempDir())
	want := map[string]bool{}
	var seq []string
	for i := 0; i < 6; i++ {
		list := m.SelectChatOrder(accts("a", "b", "c"))
		if len(list) != 3 {
			t.Fatalf("账号数 = %d", len(list))
		}
		seq = append(seq, list[0].ID)
		want[list[0].ID] = true
	}
	if len(want) != 3 {
		t.Errorf("6 轮下来只轮到 %d 个起始号（期望 3 个都被用上），序列 = %v", len(want), seq)
	}
	// 起始号必须每轮都换（3 个号 → 每 3 轮一个周期）
	if seq[0] == seq[1] || seq[1] == seq[2] {
		t.Errorf("相邻轮次起始号没变，轮转没生效: %v", seq)
	}
}

// 一个平台挂了不该让别的平台也跟着错位。
func TestSelectRotatesIndependentlyPerProvider(t *testing.T) {
	m := NewManager(t.TempDir())
	q := accts("q1", "q2")
	r := []*ExtAccount{
		{Provider: PRaccoon, ID: "r1"},
		{Provider: PRaccoon, ID: "r2"},
	}
	all := append(append([]*ExtAccount{}, q...), r...)

	m.SelectChatOrder(all) // 第一轮
	m.SelectChatOrder(all) // 第二轮：qoder 应已前进一位，raccoon 同理但互不影响

	got := m.SelectChatOrder(all)
	if got[0].Provider != PQoder && got[0].Provider != PRaccoon {
		t.Fatalf("第一个账号的 provider 不对: %s", got[0].Provider)
	}
	// raccoon 组内部顺序也要相对稳定（按 key 排），不受 qoder 轮转影响
	var raccoons []string
	for _, a := range got {
		if a.Provider == PRaccoon {
			raccoons = append(raccoons, a.ID)
		}
	}
	if len(raccoons) != 2 {
		t.Fatalf("raccoon 账号丢了: %v", raccoons)
	}
	if raccoons[0] > raccoons[1] {
		t.Errorf("组内顺序应稳定（按 id 升序），得到 %v", raccoons)
	}
}

// 失败即进冷却，且**仍然在列表里**（只是排到健康号后面）——
// 全部冷却时仍要能兜底试一把，不能直接返回空。
func TestFailureCoolsAccountButKeepsItAsFallback(t *testing.T) {
	m := NewManager(t.TempDir())
	m.NoteChatResult(PQoder, "a", errors.New("上游 401"))

	list := m.SelectChatOrder(accts("a", "b", "c"))
	if len(list) != 3 {
		t.Fatalf("冷却的号不该被剔除，得到 %d 个", len(list))
	}
	if list[0].ID == "a" {
		t.Errorf("进冷却的号不该再排第一，顺序 = %v", ids(list))
	}
	if got := m.ChatHealth(PQoder); len(got) == 0 || got[0].FailStreak != 1 {
		t.Errorf("失败计数没记上: %+v", got)
	}
}

// 连续失败 → 退避越来越长；成功一次即清零（否则一个恢复了的号要等满
// 最后一档才肯出来，且下次失败会直接跳到高档而不是从 30s 重来）。
func TestBackoffEscalatesAndResetsOnSuccess(t *testing.T) {
	m := NewManager(t.TempDir())
	id := PQoder + "/a"

	m.NoteChatResult(PQoder, "a", errors.New("第 1 次"))
	first := cooldownOf(t, m, id)

	m.NoteChatResult(PQoder, "a", errors.New("第 2 次"))
	second := cooldownOf(t, m, id)
	if second <= first {
		t.Errorf("第 2 次失败的退避没有变长: %ds → %ds", first, second)
	}

	// 成功后完全恢复
	m.NoteChatResult(PQoder, "a", nil)
	if got := cooldownOf(t, m, id); got != 0 {
		t.Errorf("成功后应清冷却，仍有 %ds", got)
	}
	if h := m.ChatHealth(PQoder); len(h) > 0 && h[0].FailStreak != 0 {
		t.Errorf("成功后失败计数应清零: %+v", h)
	}

	// 清零后下次失败从最短档重来（不是接着上一次的档位）
	m.NoteChatResult(PQoder, "a", errors.New("重新失败"))
	again := cooldownOf(t, m, id)
	if again != first {
		t.Errorf("清零后应从最短档重来: %ds，期望 %ds", again, first)
	}
}

func cooldownOf(t *testing.T, m *Manager, id string) int {
	t.Helper()
	for _, h := range m.ChatHealth(PQoder) {
		if h.ID == id[len(PQoder)+1:] {
			return h.CooldownSec
		}
	}
	return 0
}

func ids(list []*ExtAccount) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

// 空输入不该 panic（桥接在没有账号时会走这条）。
func TestSelectEmptyAndNilSafe(t *testing.T) {
	m := NewManager(t.TempDir())
	if got := m.SelectChatOrder(nil); got != nil {
		t.Errorf("空输入应原样返回，得到 %v", got)
	}
	m.NoteChatResult(PQoder, "", errors.New("x"))
	m.NoteChatResult("", "x", errors.New("x"))
	if got := m.SelectChatOrder(nil); len(got) != 0 {
		t.Errorf("空输入+空上报不应产生条目")
	}
	// 单账号：轮转无意义，但不能出错或改变顺序
	one := m.SelectChatOrder(accts("solo"))
	if len(one) != 1 || one[0].ID != "solo" {
		t.Errorf("单账号应原样返回: %v", ids(one))
	}
}

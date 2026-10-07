package zai

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSetEnabledRevivesDisabledAndInvalid(t *testing.T) {
	st, err := NewStore(filepath.Join(t.TempDir(), "acc.json"))
	if err != nil {
		t.Fatal(err)
	}

	// 风控禁用：人工 toggle 必须复活
	banned := NewAccount("封禁号", "header."+b64(`{"sub":"b"}`)+".sig")
	_ = st.Add(banned)
	_ = st.Update(banned.ID, func(a *Account) { a.BanForRisk("3012") })
	if st.Get(banned.ID).Status != StatusDisabled {
		t.Fatal("前置条件：应处于禁用")
	}
	if err := st.SetEnabled(banned.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := st.Get(banned.ID); got.Status != StatusActive || got.LastError != "" {
		t.Fatalf("人工启用应复活禁用号并清错误，got status=%s err=%q", got.Status, got.LastError)
	}

	// 凭证失效：人工 toggle 同样给恢复入口（WAF/轮询误判后的人工复核）
	dead := NewAccount("误杀号", "header."+b64(`{"sub":"d"}`)+".sig")
	_ = st.Add(dead)
	_ = st.Update(dead.ID, func(a *Account) { a.Invalidate("额度查询被拒：凭证失效") })
	if st.Get(dead.ID).Status != StatusInvalid {
		t.Fatal("前置条件：应处于失效")
	}
	if err := st.SetEnabled(dead.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := st.Get(dead.ID); got.Status != StatusActive || got.LastError != "" {
		t.Fatalf("人工启用应复活失效号，got status=%s err=%q", got.Status, got.LastError)
	}

	// 冷却：人工 toggle 不得强解时间驱动状态
	cooling := NewAccount("冷却号", "header."+b64(`{"sub":"c"}`)+".sig")
	_ = st.Add(cooling)
	_ = st.Update(cooling.ID, func(a *Account) { a.Cool(300 * time.Second) })
	if err := st.SetEnabled(cooling.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := st.Get(cooling.ID).Status; got != StatusCooling {
		t.Fatalf("冷却不应被人工启用强解，got %s", got)
	}
}

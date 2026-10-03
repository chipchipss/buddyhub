package ima

import (
	"strings"
	"testing"
	"time"
)

// replaceCookieToken 必须只换 IMA-TOKEN 的值，且不误伤其它 cookie（尤其
// IMA-REFRESH-TOKEN —— 它的值里不含 "IMA-TOKEN=" 前缀，但正则若写错就会连带替换）。
func TestReplaceCookieTokenKeepsOthers(t *testing.T) {
	cookie := "PLATFORM=H5; IMA-UID=u123; IMA-TOKEN=oldtoken==; IMA-REFRESH-TOKEN=refABC; TOKEN-TYPE=14"
	out := replaceCookieToken(cookie, "newtoken==")
	if !strings.Contains(out, "IMA-TOKEN=newtoken==") {
		t.Fatalf("IMA-TOKEN 未被替换: %s", out)
	}
	if strings.Contains(out, "IMA-TOKEN=oldtoken") {
		t.Errorf("旧 IMA-TOKEN 仍在: %s", out)
	}
	if !strings.Contains(out, "IMA-REFRESH-TOKEN=refABC") {
		t.Errorf("refresh token 被误伤: %s", out)
	}
	if !strings.Contains(out, "IMA-UID=u123") || !strings.Contains(out, "TOKEN-TYPE=14") {
		t.Errorf("其它 cookie 字段丢失: %s", out)
	}
}

// 没有 IMA-TOKEN 时追加（而非静默丢弃整个 cookie）。
func TestReplaceCookieTokenAppendsWhenAbsent(t *testing.T) {
	out := replaceCookieToken("IMA-UID=u1", "tok")
	if !strings.Contains(out, "IMA-TOKEN=tok") {
		t.Errorf("缺 IMA-TOKEN 时应追加: %s", out)
	}
}

// NeedsRefresh：无 refresh_token 一律不续（否则每个请求都白打一次注定失败的接口）。
func TestNeedsRefreshSkipsWithoutRefreshToken(t *testing.T) {
	c := &Credential{Cookie: "IMA-TOKEN=t", LastRefresh: time.Now().Add(-10 * 3600).Unix()}
	if c.NeedsRefresh() {
		t.Error("无 refresh_token 时不应续期")
	}
}

// NeedsRefresh：从没续期过（LastRefresh=0）不续——刚添加的 cookie 通常还有效。
func TestNeedsRefreshSkipsWhenNeverRefreshed(t *testing.T) {
	c := &Credential{Cookie: "IMA-TOKEN=t; IMA-REFRESH-TOKEN=r"}
	if c.NeedsRefresh() {
		t.Error("从未续期过不应触发续期")
	}
}

// NeedsRefresh：续期后临近过期才续，且 10 分钟余量内就续。
func TestNeedsRefreshRespectsExpiryAndSkew(t *testing.T) {
	now := time.Now().Unix()
	// 刚续期（有效期 7200）→ 不续
	fresh := &Credential{Cookie: "IMA-TOKEN=t; IMA-REFRESH-TOKEN=r", LastRefresh: now, TokenValidTime: 7200}
	if fresh.NeedsRefresh() {
		t.Error("刚续期不该续")
	}
	// 已过有效期 → 续
	stale := &Credential{Cookie: "IMA-TOKEN=t; IMA-REFRESH-TOKEN=r",
		LastRefresh: now - 7200 - 60, TokenValidTime: 7200}
	if !stale.NeedsRefresh() {
		t.Error("过期后应续期")
	}
	// 落在余量窗口内（距到期 5 分钟）→ 续
	skew := &Credential{Cookie: "IMA-TOKEN=t; IMA-REFRESH-TOKEN=r",
		LastRefresh: now - (7200 - 300), TokenValidTime: 7200}
	if !skew.NeedsRefresh() {
		t.Error("余量窗口内应续期")
	}
	// 未提供 token_valid_time 时按缺省 7200 算
	noTTL := &Credential{Cookie: "IMA-TOKEN=t; IMA-REFRESH-TOKEN=r",
		LastRefresh: now - 7300}
	if !noTTL.NeedsRefresh() {
		t.Error("缺省有效期下过期应续期")
	}
}

// bkn 必须逐字对齐参考实现（djb2 变体 h = h + (h<<5) + ch）——算错会被上游判成
// 非法 cookie。向量手算：5381 → 'a'(97) 177670 → 'b'(98) 5863208 → 'c'(99) 193485963。
func TestBknMatchesReferenceVector(t *testing.T) {
	if got := bkn("abc"); got != "193485963" {
		t.Errorf("bkn(\"abc\") = %s, 期望 193485963", got)
	}
	if got := bkn(""); got != "5381" {
		t.Errorf("bkn(\"\") = %s, 期望 5381", got)
	}
}
package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/authcore"
)

/* ── 阶段 4b：生命周期语义（jobToken/deviceChat 登出谓词、CN 特例）── */

func newLifecycleClient(handler http.HandlerFunc) (*Client, Site) {
	srv := httptest.NewServer(handler)
	t := &testing.T{}
	t.Cleanup(srv.Close)
	global, cn := SiteGlobal, SiteCN
	global.OpenAPI, cn.OpenAPI = srv.URL, srv.URL
	return &Client{HTTP: srv.Client(), SpentCaches: map[string]*authcore.SpentCache{
		"global": authcore.NewSpentCache(time.Hour),
		"cn":     authcore.NewSpentCache(time.Hour),
	}}, global
}

// jrt jobToken refresh 401/403 → SignInExpiredError；其他状态保留登录态。

func TestJobRefreshRefusalMarksExpired(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		c, global := newLifecycleClient(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":"invalid_token"}`))
		})
		cred := &Credential{AccessToken: "jt", RefreshToken: "jrt-1"}
		_, err := c.RefreshForSite(context.Background(), global, cred)
		var expired *SignInExpiredError
		if !errors.As(err, &expired) {
			t.Fatalf("status %d: want SignInExpiredError, got %v", status, err)
		}
	}
}

func TestJobRefreshTransientKeepsSignIn(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		c, global := newLifecycleClient(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})
		cred := &Credential{AccessToken: "jt", RefreshToken: "jrt-1"}
		_, err := c.RefreshForSite(context.Background(), global, cred)
		var expired *SignInExpiredError
		if errors.As(err, &expired) {
			t.Fatalf("status %d must NOT mark expired (transient)", status)
		}
		if err == nil {
			t.Fatalf("status %d must still error", status)
		}
	}
}

// 成功刷新必须轮换 refresh token（一次性 token 语义），并回写新 pair。

func TestJobRefreshRotatesPair(t *testing.T) {
	c, global := newLifecycleClient(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"token":"jt-2","refresh_token":"jrt-2","expires_at":1790835411267}`))
	})
	cred := &Credential{AccessToken: "jt-1", RefreshToken: "jrt-1"}
	fresh, err := c.RefreshForSite(context.Background(), global, cred)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if fresh.RefreshToken != "jrt-2" || fresh.AccessToken != "jt-2" {
		t.Fatalf("fresh = %+v", fresh)
	}
	if fresh.ExpireTime < 1e12 {
		t.Fatalf("expiry not ms: %d", fresh.ExpireTime)
	}
}

// CN deviceChat：device token 即 chat token；401/403 → 登录失效。

func TestDeviceChatRefreshRefusalMarksExpired(t *testing.T) {
	c, cn := newLifecycleClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	cred := &Credential{AccessToken: "dt-cn", RefreshToken: "drt-cn", DeviceChat: true}
	_, err := c.RefreshForSite(context.Background(), cn, cred)
	var expired *SignInExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("deviceChat refusal must mark expired, got %v", err)
	}
}

func TestDeviceChatRefreshRotatesPair(t *testing.T) {
	c, cn := newLifecycleClient(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, refreshPathDT) {
			t.Errorf("device refresh must hit %s, got %s", refreshPathDT, r.URL.Path)
		}
		w.Write([]byte(`{"token":"dt-cn-2","refresh_token":"drt-cn-2","expires_in":7200}`))
	})
	cred := &Credential{AccessToken: "dt-cn", RefreshToken: "drt-cn", DeviceChat: true}
	fresh, err := c.refreshDeviceChat(context.Background(), cn, cred)
	if err != nil {
		t.Fatalf("device refresh: %v", err)
	}
	if fresh.DeviceToken != "dt-cn-2" || fresh.DeviceRefreshToken != "drt-cn-2" || fresh.AccessToken != "dt-cn-2" {
		t.Fatalf("fresh = %+v", fresh)
	}
}

// 一次性 refresh token：成功但缺新 pair 必须报错（绝不沿用已用旧 token）。

func TestJobRefreshMissingPairErrors(t *testing.T) {
	c, global := newLifecycleClient(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"token":"jt-2"}`)) // 缺 refresh_token
	})
	cred := &Credential{AccessToken: "jt-1", RefreshToken: "jrt-1"}
	if _, err := c.RefreshForSite(context.Background(), global, cred); err == nil {
		t.Fatal("missing rotated pair must error")
	}
}

// SpentCache：同一旧 refresh token 的并发刷新，第二个直接拿缓存结果
//（不对一次性 token 再打上游）。

func TestRefreshSpentCacheHit(t *testing.T) {
	calls := 0
	c, global := newLifecycleClient(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"token":"jt-2","refresh_token":"jrt-2"}`))
	})
	cred := &Credential{AccessToken: "jt-1", RefreshToken: "jrt-1"}
	first, err := c.RefreshForSite(context.Background(), global, cred)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := c.RefreshForSite(context.Background(), SiteGlobal, &Credential{AccessToken: "jt-1", RefreshToken: "jrt-1"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if calls != 1 {
		t.Fatalf("upstream called %d times, want 1 (cache hit)", calls)
	}
	if first.AccessToken != second.AccessToken {
		t.Fatalf("cache mismatch: %q vs %q", first.AccessToken, second.AccessToken)
	}
}

// ExchangeDeviceToken：CN 4xx 回退 deviceChat，Global 4xx 报错。

func TestExchangeDeviceTokenCNFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"client not allowed"}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	site := SiteCN
	site.OpenAPI = srv.URL
	device := &Credential{AccessToken: "dt-cn", RefreshToken: "drt-cn", ExpireTime: time.Now().Add(time.Hour).UnixMilli()}
	out, err := c.ExchangeDeviceToken(context.Background(), site, device)
	if err != nil {
		t.Fatalf("CN fallback: %v", err)
	}
	if !out.DeviceChat || out.DeviceToken != "dt-cn" || out.SiteID != "cn" {
		t.Fatalf("fallback cred = %+v", out)
	}
}

func TestExchangeDeviceTokenGlobal4xxIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	site := SiteGlobal
	site.OpenAPI = srv.URL
	device := &Credential{AccessToken: "dt", RefreshToken: "drt", ExpireTime: time.Now().Add(time.Hour).UnixMilli()}
	_, err := c.ExchangeDeviceToken(context.Background(), site, device)
	var expired *SignInExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("global 4xx must be SignInExpiredError, got %v", err)
	}
}

func TestExchangeDeviceTokenSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["clientId"] == "" {
			t.Errorf("clientId missing")
		}
		w.Write([]byte(`{"token":"jt","refresh_token":"jrt","expires_in":3600}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client()}
	site := SiteGlobal
	site.OpenAPI = srv.URL
	device := &Credential{AccessToken: "dt", RefreshToken: "drt", ExpireTime: time.Now().Add(time.Hour).UnixMilli()}
	out, err := c.ExchangeDeviceToken(context.Background(), site, device)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if out.AccessToken != "jt" || out.RefreshToken != "jrt" || out.DeviceChat {
		t.Fatalf("out = %+v", out)
	}
}

func TestSiteByID(t *testing.T) {
	if SiteByID("").ID != SiteGlobal.ID {
		t.Fatal("empty defaults global")
	}
	if SiteByID("cn").ID != SiteCN.ID {
		t.Fatal("cn resolution failed")
	}
	if SiteByID("global").ID != SiteGlobal.ID {
		t.Fatal("global resolution failed")
	}
}

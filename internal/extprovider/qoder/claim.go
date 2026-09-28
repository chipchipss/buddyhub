// Package qoder: claim.go — COSY 活动领取引擎（对齐 d4ncboz/qoder-workflow claimer）。
//
// 与 campaign.go 的 /sash/ 路线互补的第二条领奖通道：
//
//	GET  {openapi}/api/v2/activity/claim/eligibility   扫描可领活动
//	POST {openapi}/api/v2/activity/claim?activityId=x  领取
//	GET  {openapi}/api/v2/quota/usage                  配额查询
//
// 签名（动态 GMT MD5，来源 qoder-workflow 实测）：
//
//	raw = "cosy&war, war never changes&<RFC1123 GMT>"
//	signature = md5hex(raw)
//	头：date / signature / appcode: cosy + machine 头族（可选，缺省空值）
package qoder

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const claimAppCode = "cosy"
const claimSecret = "cosy&war, war never changes"

// claimSignature 生成 (date, signature)。
func claimSignature() (string, string) {
	now := time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	sum := md5.Sum([]byte(claimSecret + "&" + now))
	return now, hex.EncodeToString(sum[:])
}

// claimHeaders COSY 活动请求头（machine 头族可选：缺失时服务端按无设备身份处理）。
func (c *Client) claimHeaders(cred *Credential, machine *MachineIdentity) (http.Header, error) {
	date, sig := claimSignature()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+cred.Bearer())
	h.Set("Cosy-Version", "1.1.5")
	h.Set("Cosy-ClientType", SashClientType)
	h.Set("Cosy-MachineOS", "x86_64_win32")
	h.Set("Cosy-MachineId", cred.MachineID)
	h.Set("appcode", claimAppCode)
	h.Set("login-version", "v2")
	h.Set("date", date)
	h.Set("signature", sig)
	h.Set("User-Agent", "Go-http-client/2.0")
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	if machine == nil {
		machine = FindMachineIdentity()
	}
	if machine != nil {
		h.Set("Cosy-MachineToken", machine.Token)
		h.Set("Cosy-MachineType", machine.Type)
	}
	return h, nil
}

// ClaimActivity 活动领取结果。
type ClaimActivity struct {
	Eligible []map[string]any `json:"eligible"`
	Claimed  []string         `json:"claimed"`
	Failed   []string         `json:"failed"`
	Note     string           `json:"note"`
}

// ClaimActivities 扫描可领活动并逐个领取（一次调用完成全部可领项）。
// 单项失败不影响其余；全部不可领时 Eligible 为空。
func (c *Client) ClaimActivities(ctx context.Context, cred *Credential) (*ClaimActivity, error) {
	out := &ClaimActivity{}

	// 1) 扫描资格
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenAPIBase+"/api/v2/activity/claim/eligibility", nil)
	if err != nil {
		return nil, err
	}
	hdr, err := c.claimHeaders(cred, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("eligibility HTTP %d: %s", resp.StatusCode, truncate(raw, 160))
	}
	var elig struct {
		Code      int              `json:"code"`
		Activites []map[string]any `json:"activities"`
		Data      []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &elig); err != nil {
		return nil, fmt.Errorf("eligibility 解析失败: %w", err)
	}
	items := elig.Data
	if len(items) == 0 {
		items = elig.Activites
	}
	// 2) 逐个领取 id 类字段（id / activityId / campaignId 兼容）
	for _, item := range items {
		id := anyID(item, []string{"id", "activityId", "campaignId"})
		if id == "" {
			continue
		}
		out.Eligible = append(out.Eligible, item)
		claimURL := fmt.Sprintf("%s/api/v2/activity/claim?activityId=%s", OpenAPIBase, urlPathEscape(id))
		req2, err := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, nil)
		if err != nil {
			continue
		}
		hdr2, err := c.claimHeaders(cred, nil)
		if err != nil {
			continue
		}
		for k, vs := range hdr2 {
			for _, v := range vs {
				req2.Header.Add(k, v)
			}
		}
		resp2, err := c.HTTP.Do(req2)
		if err != nil {
			out.Failed = append(out.Failed, id)
			continue
		}
		body2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		if resp2.StatusCode == http.StatusOK {
			var r2 struct {
				Code int `json:"code"`
			}
			_ = json.Unmarshal(body2, &r2)
			if r2.Code == 0 {
				out.Claimed = append(out.Claimed, id)
			} else {
				out.Failed = append(out.Failed, id)
			}
		} else {
			out.Failed = append(out.Failed, id)
		}
	}
	if len(out.Eligible) == 0 {
		out.Note = "当前无可领取活动"
	}
	return out, nil
}

// QuotaUsage 配额查询（原样 JSON 透出，面板展示用）。
func (c *Client) QuotaUsage(ctx context.Context, cred *Credential) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenAPIBase+"/api/v2/quota/usage", nil)
	if err != nil {
		return nil, err
	}
	hdr, err := c.claimHeaders(cred, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(raw, 160))
	}
	return json.RawMessage(raw), nil
}

// anyID 从 map 按候选键序取第一个非空字符串（兼容数字）。
func anyID(m map[string]any, keys []string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			if v == float64(int64(v)) {
				return fmt.Sprintf("%d", int64(v))
			}
			return fmt.Sprintf("%v", v)
		}
	}
	return ""
}

// urlPathEscape 最小路径转义（标准 url.PathEscape 的等价实现，免 import）。
func urlPathEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

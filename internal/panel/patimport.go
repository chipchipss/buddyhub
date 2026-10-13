package panel

// patimport.go Qoder PAT 批量导入（fork qoder-farm 的 inject_qoder_pat.py，
// MIT）——账号池扩容的现成弹药。
//
// fork 做的事：读 pats_final.json（[{"email","pat"}，pat 形如 pt-...）→
// 校验 pt- 前缀 → 幂等 upsert 进 9router SQLite 的 providerConnections
// （provider=qoder, authType=apikey, name=email，已存在则更新 data）。
// buddyhub 侧等价物是 extstore.Manager.Add（同 provider+同 ID 幂等覆盖），
// 凭据形状走 qoder.Credential 的 pat 字段。
//
// 边界（与原脚本一致，如实保留）：
//   - 只做「注入」：PAT 从哪来不管（farm 产物、手工导出、其他渠道）；
//   - 不注册、不养号、不碰验证码——那部分明确不进 buddyhub 主体；
//   - 幂等：同 email 重复导入是更新不是报错（脚本的 SELECT-then-UPSERT 语义）；
//   - 逐条失败不阻断：坏行记 errors，好行照常入库。
//
// 端点：POST /panel/api/import/qoder-pat（JSON body，与 cockpit 导入同族）

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// patEntry 导入数组的单个元素（fork pats_final.json 形状）。
type patEntry struct {
	Email string `json:"email"`
	PAT   string `json:"pat"`
}

// patImportBody 请求体：账号数组。
type patImportBody struct {
	Accounts []patEntry `json:"accounts"`
}

// patValid 校验 PAT 形态（fork L38-41：必须 pt- 开头）。
// 只做形态校验，不联网验证——有效性由账号池的签到/对话路径自然发现。
func patValid(pat string) bool { return strings.HasPrefix(pat, "pt-") }

// patEmailIDValid 校验 email 能否作为账号 id 落盘（fork 的幂等键就是
// email 本身，所以口径为 email 设计，不能复用 validImportUID——那个只放行
// [a-zA-Z0-9-_]，会把 @ 和 . 全毙掉）。
//
// 要求：恰好一个 @，本地段非空，域名段含至少一个点且不含空白与控制字符。
// 长度上限 254（RFC 5321），防御性截断滥用。
func patEmailIDValid(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at != strings.LastIndexByte(email, '@') {
		return false // 无 @ / 多个 @ / @ 开头
	}
	local, domain := email[:at], email[at+1:]
	if local == "" || domain == "" {
		return false
	}
	if !strings.Contains(domain, ".") {
		return false
	}
	for _, c := range email {
		if c <= ' ' || c == 0x7f || c == '/' || c == '\\' {
			return false // 空白/控制字符/路径分隔符
		}
	}
	return true
}

// importQoderPAT POST /panel/api/import/qoder-pat
//
// body: {"accounts":[{"email":"a@b.c","pat":"pt-..."}, ...]}
// 返回 {ok, total, imported, skipped, errors}（与 cockpit 导入同形状）。
//
// 落库：provider=qoder，id=email（同 fork 的 name=email 幂等键），
// label 缺省用 email；cred = {"pat":"pt-..."}（qoder.Credential 字段）。
func (p *Panel) importQoderPAT(w http.ResponseWriter, r *http.Request) {
	var body patImportBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(body.Accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var imported, skipped int
	var errs []string
	for _, acc := range body.Accounts {
		email := strings.TrimSpace(acc.Email)
		pat := strings.TrimSpace(acc.PAT)
		if email == "" {
			skipped++
			errs = append(errs, "missing email")
			continue
		}
		// 账号 id 落盘要做文件名/路径安全（extstore 的 id 参与存储键）——
		// 口径按 email 设计（fork 的幂等键就是 email）：@/. 合法，
		// 空白/控制字符/路径分隔符拒绝。
		if !patEmailIDValid(email) {
			skipped++
			errs = append(errs, fmt.Sprintf("invalid email id: %q", email))
			continue
		}
		if !patValid(pat) {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: bad PAT（须 pt- 开头）", email))
			continue
		}
		cred, err := json.Marshal(map[string]string{"pat": pat})
		if err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: cred marshal: %v", email, err))
			continue
		}
		label := email
		if err := p.extManager().Add("qoder", email, label, cred); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: add: %v", email, err))
			continue
		}
		imported++
	}

	log.Printf("panel: qoder PAT import finished total=%d imported=%d skipped=%d",
		len(body.Accounts), imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    len(body.Accounts),
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

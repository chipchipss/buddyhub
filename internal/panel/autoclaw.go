package panel

// autoclaw.go AutoClaw（智谱 autoglm）面板接线：手机短信登录入池。
//
// 与 extlogin.go 里的设备码/扫码流程不同，短信登录是**两段同步调用**
// （发码 → 校验），不需要会话与轮询：
//
//	POST /panel/api/ext/autoclaw/send_code  {phone, region}            → {device_id, phone_tail}
//	POST /panel/api/ext/autoclaw/login      {phone, code, device_id, region} → {account}
//
// device_id 必须由前端透传回来：上游把设备与登录会话绑定，两步用不同的
// device_id 会登录失败。

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/autoclaw"
	"github.com/chipchipss/buddyhub/internal/extstore"
)

// extAutoClawSendCode POST /panel/api/ext/autoclaw/send_code
func (p *Panel) extAutoClawSendCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone  string `json:"phone"`
		Region string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json")
		return
	}
	phone := strings.TrimSpace(body.Phone)
	if phone == "" {
		writeErr(w, http.StatusBadRequest, "请填写手机号")
		return
	}
	region := autoclaw.ParseRegion(body.Region)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	deviceID, err := autoclaw.SendCode(ctx, region, phone)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: AutoClaw 验证码已发送（%s · %s）", autoclaw.MaskPhone(phone), region.Label())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"device_id":  deviceID,
		"phone_tail": autoclaw.MaskPhone(phone),
		"region":     string(region),
	})
}

// extAutoClawLogin POST /panel/api/ext/autoclaw/login
func (p *Panel) extAutoClawLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		Code     string `json:"code"`
		DeviceID string `json:"device_id"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request json")
		return
	}
	phone := strings.TrimSpace(body.Phone)
	if phone == "" {
		writeErr(w, http.StatusBadRequest, "请填写手机号")
		return
	}
	region := autoclaw.ParseRegion(body.Region)

	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	cred, err := autoclaw.LoginWithCode(ctx, region, phone, body.Code, body.DeviceID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	raw, err := json.Marshal(cred)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 账号 ID：优先上游 userId，其次手机号（两者都稳定，重复登录幂等覆盖）
	id := cred.UserID
	if id == "" {
		id = "autoclaw-" + strings.TrimPrefix(autoclaw.MaskPhone(phone), "+")
	}
	label := "AutoClaw " + region.Label()
	if cred.PhoneTail != "" {
		label += " · " + cred.PhoneTail
	}
	if err := p.extManager().Add(extstore.PAutoClaw, id, label, raw); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: AutoClaw 账号已通过短信登录入池 %s（%s）", id, region.Label())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"account": map[string]any{
			"id": id, "label": label, "provider": extstore.PAutoClaw,
		},
	})
}

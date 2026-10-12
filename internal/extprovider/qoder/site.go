package qoder

import "strings"

// Site describes the Qoder deployment used for login and token operations.
// Lineage: magpie-plugins packages/qoder/index.mjs L1299-1324.
type Site struct {
	ID         string
	Name       string
	Web        string
	OpenAPI    string
	Gateway    string
	ClientID   string
	Redirect   string
	DeviceChat bool // Qoder CN: use device token directly when jobToken is refused
}

var (
	SiteGlobal = Site{ID: "global", Name: "Qoder", Web: "https://qoder.com", OpenAPI: OpenAPIBase, Gateway: InferBase, ClientID: ClientID, Redirect: "qoder-app://"}
	SiteCN     = Site{ID: "cn", Name: "Qoder CN", Web: "https://qoder.cn", OpenAPI: "https://openapi.qoder.com.cn", Gateway: "https://gateway.qoder.com.cn", ClientID: ClientID, DeviceChat: true}
)

// SiteByID returns a registered site; unknown/empty defaults to Global.
func SiteByID(id string) Site {
	if strings.EqualFold(strings.TrimSpace(id), SiteCN.ID) {
		return SiteCN
	}
	return SiteGlobal
}

// SiteForLoginName normalizes the extstore provider/name hint to a Qoder site.
// CN account labels/provider aliases are accepted; unknown values default global.
func SiteForLoginName(name string) Site {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.Contains(n, "cn") || strings.Contains(n, "中国") || strings.Contains(n, "国内") {
		return SiteCN
	}
	return SiteGlobal
}

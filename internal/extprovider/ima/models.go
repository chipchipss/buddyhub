package ima

// models.go ima 的模型清单（(type, id) 二元组，不是名字字符串）。
//
// 清单来自 ima2api 的 config.example.json（官方 id，实测可用）。
// 上游还有 GET /cgi-bin/model_manage/get_models 动态目录，这里先用静态表
// ——与 qoder 同一取舍：id 是官方写死的（official_N），漂移概率低。

// Model ima 的一个模型（type/id 是上游的二元标识）。
type Model struct {
	Name string `json:"name"` // 我们对外的模型名（ima:<name>）
	Type int    `json:"type"` // model_type
	ID   string `json:"id"`   // model_id（official_N）
	Desc string `json:"desc,omitempty"`
}

// Models 全部已知模型。
func Models() []Model {
	return []Model{
		{Name: "hy3-preview", Type: 0, ID: "official_0", Desc: "腾讯混元 Hy3 preview"},
		{Name: "hy3-preview-think", Type: 2, ID: "official_2", Desc: "混元 Hy3 preview（思考）"},
		{Name: "deepseek-v4-flash", Type: 3, ID: "official_3", Desc: "DeepSeek V4-Flash"},
		{Name: "deepseek-v4-flash-think", Type: 1, ID: "official_1", Desc: "DeepSeek V4-Flash（思考）"},
		{Name: "glm-5.2", Type: 3000, ID: "official_3000", Desc: "GLM-5.2"},
		{Name: "glm-5.2-think", Type: 3001, ID: "official_3001", Desc: "GLM-5.2（思考）"},
	}
}

// Lookup 按我们的模型名查（找不到返回 nil，调用方回落默认）。
func Lookup(name string) *Model {
	for i := range Models() {
		if Models()[i].Name == name {
			m := Models()[i]
			return &m
		}
	}
	return nil
}

// Default 默认模型（hy3-preview，与 ima2api 的 default_model 一致）。
func Default() Model {
	return Models()[0]
}

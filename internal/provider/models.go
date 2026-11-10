// 模型清单（PRD F2）：预检时从上游拉取并随 providers.json 落盘，
// 供 /v1/models、模型选择器与目录同步（W3+）使用。
package provider

// Model 是模型清单的一行。
type Model struct {
	ID            string `json:"id"`
	ContextWindow int    `json:"context_window,omitempty"`
}

// ModelIDs 返回清单中的模型 id 列表。
func (p *Provider) ModelIDs() []string {
	out := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		out = append(out, m.ID)
	}
	return out
}

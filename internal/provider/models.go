// 模型清单（PRD F2）：预检时从上游拉取并随 providers.json 落盘，
// 供 /v1/models、模型选择器与目录同步（W3+）使用。
package provider

// Model 是模型清单的一行。
type Model struct {
	ID            string `json:"id"`
	ContextWindow int    `json:"context_window,omitempty"`
	// Unavailable 是组员体检实测「确定性不可用」标记（2026-11-20）：
	// 清单拉到 ≠ 套餐能用（如智谱 1311 套餐未开放，发生在对话阶段）。
	// 置位后控制台清单行打「不可用」徽标，意图链席位池与智能代理候选
	// 同步剔除该模型；再次体检实测通过即清除（AuditGroups 反向清除，
	// 也是唯一清除入口——预检/编辑写回均按 ID 继承标记，不因普通操作
	// 抹掉）。随 providers.json 落盘。
	Unavailable   bool   `json:"unavailable,omitempty"`
	UnavailReason string `json:"unavail_reason,omitempty"` // 实测判定原因（悬浮展示）
}

// ModelIDs 返回清单中的模型 id 列表。
func (p *Provider) ModelIDs() []string {
	out := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		out = append(out, m.ID)
	}
	return out
}

// 策略 agent（2026-11-13）：Agent 插件按请求语境自主选模型——
// Plan 内 smart 基线 + 转发前 agentPickReorder 重排（选中者置首）；
// 插件不可用/非法输出回落基线，绝不因分配而失败。
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"yardmaster/internal/provider"
)

func agentPickServer(t *testing.T, picks *[]int, calls *int) *Server {
	prov := provider.Provider{ID: "prov", Name: "prov", Protocol: provider.ProtocolChat,
		BaseURL:  "http://x/v1",
		Models:   []provider.Model{{ID: "m1"}, {ID: "m2"}},
		Accounts: []provider.Account{{Name: "a1"}, {Name: "a2"}}}
	group := provider.Group{ID: "g1", Strategy: provider.StrategyAgent,
		Members: []string{"prov/m1@a1", "prov/m2@a2"}}
	s := &Server{Planner: NewPlanner([]provider.Provider{prov}, []provider.Group{group}), Version: "t"}
	if picks != nil {
		s.AgentPick = func(ctx context.Context, payload map[string]any) (json.RawMessage, error) {
			*calls++
			// payload 应带用户消息与候选清单（含 index/member/status）；
			// 候选可能为强类型切片（单测直调）——经 JSON 归一后断言。
			b, err := json.Marshal(payload["candidates"])
			if err != nil {
				t.Fatalf("候选清单序列化失败: %v", err)
			}
			var cs []map[string]any
			if err := json.Unmarshal(b, &cs); err != nil || len(cs) != 2 {
				t.Fatalf("候选清单应含 2 项: %s", b)
			}
			if _, ok := payload["message"].(string); !ok {
				t.Fatalf("payload 应带 message: %v", payload)
			}
			return json.Marshal(map[string]int{"pick": (*picks)[0]})
		}
	}
	return s
}

func TestAgentPickReorderMovesPickedToFront(t *testing.T) {
	picks := []int{1}
	calls := 0
	s := agentPickServer(t, &picks, &calls)
	cands, err := s.Planner.Plan("group/g1", "sticky-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("应有 2 个候选: %d", len(cands))
	}
	out := s.agentPickReorder(cands, "group/g1", "qa", "一段无特征话术")
	if out[0].UpstreamModel != "m2" {
		t.Fatalf("Agent 选中候选应提到队首: %+v", out[0])
	}
	if out[1].UpstreamModel != cands[0].UpstreamModel {
		t.Fatalf("其余候选保持基线顺序作后备: %+v vs %+v", out[1], cands[0])
	}
	if calls != 1 {
		t.Fatalf("应调用一次 AgentPick: %d", calls)
	}
}

func TestAgentPickIllegalFallsBackToBaseline(t *testing.T) {
	picks := []int{9} // 序号越界
	calls := 0
	s := agentPickServer(t, &picks, &calls)
	cands, err := s.Planner.Plan("group/g1", "sticky-b")
	if err != nil {
		t.Fatal(err)
	}
	out := s.agentPickReorder(cands, "group/g1", "qa", "一段无特征话术")
	for i := range cands {
		if out[i].UpstreamModel != cands[i].UpstreamModel || out[i].Key() != cands[i].Key() {
			t.Fatalf("非法输出应原样回落基线: %+v", out)
		}
	}
	if calls != 1 {
		t.Fatalf("仍应尝试调用一次: %d", calls)
	}
}

func TestAgentPickSkippedWhenNotApplicable(t *testing.T) {
	picks := []int{0}
	calls := 0
	s := agentPickServer(t, &picks, &calls)
	// 非组目标（直连模型）不触发
	cands, err := s.Planner.Plan("prov/m1@a1", "sticky-c")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.agentPickReorder(cands, "prov/m1@a1", "qa", "话术")
	if calls != 0 {
		t.Fatalf("直连目标不应调用 AgentPick: %d", calls)
	}
	// 非 agent 策略组不触发
	s2 := agentPickServer(t, nil, &calls)
	group := provider.Group{ID: "g2", Strategy: provider.StrategySmart,
		Members: []string{"prov/m1@a1"}}
	s2.Planner.Reload(s2.Planner.Providers(), []provider.Group{group})
	_ = s2.agentPickReorder(cands, "group/g2", "qa", "话术")
	if calls != 0 {
		t.Fatalf("非 agent 策略不应调用 AgentPick: %d", calls)
	}
	// 空消息不触发
	cands, err = s.Planner.Plan("group/g1", "sticky-d")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.agentPickReorder(cands, "group/g1", "", "")
	if calls != 0 {
		t.Fatalf("空消息不应调用 AgentPick: %d", calls)
	}
}

var _ = http.StatusOK // 保持 import（防未来用例增删时误删）

// M9：GET /api/provider-presets 行为测试——前端「添加 Provider」表单
// 的预填数据源与 CLI presets 消费同一 provider.Presets；还原旧代码
// （端点缺失/形状漂移）即失败。
package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

func TestProviderPresetsEndpoint(t *testing.T) {
	gw, _ := newAdminGateway(t, store.ProvidersFile{})
	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/api/provider-presets", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/provider-presets 应 200，得到 %d", resp.StatusCode)
	}
	var got []provider.Preset
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("响应应为 Preset 数组: %v", err)
	}
	if len(got) != len(provider.Presets) {
		t.Fatalf("预设数量应与内置表一致: got %d want %d", len(got), len(provider.Presets))
	}
	byID := map[string]provider.Preset{}
	for _, p := range got {
		byID[p.ID] = p
	}
	for _, want := range provider.Presets {
		gotP, ok := byID[want.ID]
		if !ok {
			t.Fatalf("预设 %q 缺失", want.ID)
		}
		if gotP.Protocol != want.Protocol || gotP.BaseURL != want.BaseURL {
			t.Fatalf("预设 %q 形状漂移: got %+v want %+v", want.ID, gotP, want)
		}
	}
}

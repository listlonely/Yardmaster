package edit

import (
	"strings"
	"testing"
)

const sampleTOML = `# 用户注释：不要动
model = "gpt-5.2"

[profiles.work]
key = "user-key" # 行尾注释

[model_providers.openai]
base_url = "https://api.openai.com/v1"
`

func TestSetTOMLReplacesExistingKeyKeepsComment(t *testing.T) {
	out, err := SetTOML([]byte(sampleTOML), nil, "model", "group/claude-max")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `model = "group/claude-max"`) {
		t.Fatalf("model 未替换\n%s", s)
	}
	if !strings.Contains(s, "# 用户注释：不要动") {
		t.Fatalf("注释丢失\n%s", s)
	}
	// 其他表不受影响
	if !strings.Contains(s, `key = "user-key" # 行尾注释`) {
		t.Fatalf("兄弟表被破坏\n%s", s)
	}
}

func TestSetTOMLNewKeyInExistingTable(t *testing.T) {
	out, err := SetTOML([]byte(sampleTOML), []string{"model_providers", "openai"}, "api_key", "sk-x")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `api_key = "sk-x"`) {
		t.Fatalf("新键未插入\n%s", s)
	}
	if !strings.Contains(s, `base_url = "https://api.openai.com/v1"`) {
		t.Fatalf("既有键丢失\n%s", s)
	}
	// 新键必须位于 [model_providers.openai] 表头之后（跟在表内最后键后）
	if strings.Index(s, `api_key = "sk-x"`) < strings.Index(s, "[model_providers.openai]") {
		t.Fatalf("新键落到了错误的表\n%s", s)
	}
}

func TestSetTOMLMissingTableAppended(t *testing.T) {
	out, err := SetTOML([]byte(sampleTOML), []string{"model_providers", "yardmaster"}, "base_url", "http://127.0.0.1:8317/v1")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "[model_providers.yardmaster]") ||
		!strings.Contains(s, `base_url = "http://127.0.0.1:8317/v1"`) {
		t.Fatalf("缺表未补建\n%s", s)
	}
	if strings.Contains(s, "[model_providers.yardmaster]") && strings.Index(s, "[model_providers.yardmaster]") < strings.Index(s, "[profiles.work]") {
		t.Fatalf("新表应追加在文件末尾\n%s", s)
	}
}

func TestUnsetTOMLIdempotentAndScoped(t *testing.T) {
	out, err := UnsetTOML([]byte(sampleTOML), nil, "model")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `model = "gpt-5.2"`) {
		t.Fatalf("model 未删除\n%s", out)
	}
	if !strings.Contains(string(out), "[profiles.work]") {
		t.Fatalf("其他表丢失\n%s", out)
	}
	// 幂等
	out2, err := UnsetTOML(out, nil, "model")
	if err != nil || string(out2) != string(out) {
		t.Fatalf("幂等性破坏: %v", err)
	}
	// 删除不存在的键：原样
	out3, _ := UnsetTOML([]byte(sampleTOML), []string{"profiles", "work"}, "nope")
	if string(out3) != sampleTOML {
		t.Fatalf("不存在键的删除改动了字节")
	}
}

func TestGetTOMLRoundtrip(t *testing.T) {
	data := []byte(sampleTOML)
	if v, ok := GetTOML(data, nil, "model"); !ok || v != "gpt-5.2" {
		t.Fatalf("model = %v %v", v, ok)
	}
	if v, ok := GetTOML(data, []string{"profiles", "work"}, "key"); !ok || v != "user-key" {
		t.Fatalf("profiles.work.key = %v %v", v, ok)
	}
	if _, ok := GetTOML(data, nil, "nope"); ok {
		t.Fatal("不存在的键应返回 false")
	}
}

func TestSetTOMLThenGetRoundtrip(t *testing.T) {
	out, err := SetTOML([]byte(sampleTOML), []string{"model_providers", "yardmaster"}, "api_key", "gw-codex")
	if err != nil {
		t.Fatal(err)
	}
	v, ok := GetTOML(out, []string{"model_providers", "yardmaster"}, "api_key")
	if !ok || v != "gw-codex" {
		t.Fatalf("roundtrip = %v %v", v, ok)
	}
}

func TestTOMLCRLFPreserved(t *testing.T) {
	in := "a = 1\r\n[t]\r\nb = 2\r\n"
	out, err := SetTOML([]byte(in), []string{"t"}, "c", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Count(s, "\r\n") != 4 { // 原有 3 行 + 新增 1 行
		t.Fatalf("CRLF 数量异常: %q", s)
	}
	if !strings.Contains(s, "c = 3\r\n") {
		t.Fatalf("新行应为 CRLF: %q", s)
	}
}

func TestTOMLTopLevelKeyNotConfusedWithTableKeys(t *testing.T) {
	// 顶层 model 与 [x] 内的 model 不得混淆
	in := "[x]\nmodel = \"inner\"\n"
	out, err := SetTOML([]byte(in), nil, "model", "outer")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "model = \"outer\"\n") {
		t.Fatalf("顶层键应插在文件最前: %q", s)
	}
	if !strings.Contains(s, "model = \"inner\"") {
		t.Fatalf("表内键被误改: %q", s)
	}
}

package edit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const sampleJSONC = `{
  // 用户注释：不要动
  "model": "group/claude-max",
  "env": {
    /* 块注释 */
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8317",
    "ANTHROPIC_AUTH_TOKEN": "gw-claude-code" // 行尾注释
  },
  "tiers": {"haiku": "glm-air"}
}`

func TestSetReplacesValueAndKeepsComments(t *testing.T) {
	out, err := SetJSONC([]byte(sampleJSONC), []string{"model"}, "deepseek/deepseek-chat")
	if err != nil {
		t.Fatalf("SetJSONC: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"deepseek/deepseek-chat",
		"// 用户注释：不要动",
		"/* 块注释 */",
		"// 行尾注释",
		`"tiers": {"haiku": "glm-air"}`,
		"gw-claude-code",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("result missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "group/claude-max") {
		t.Fatalf("old value must be replaced\n---\n%s", s)
	}
}

func TestSetCreatesMissingParents(t *testing.T) {
	out, err := SetJSONC([]byte(sampleJSONC), []string{"features", "async", "enabled"}, true)
	if err != nil {
		t.Fatalf("SetJSONC: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"features"`) || !strings.Contains(s, `"async"`) || !strings.Contains(s, `"enabled": true`) {
		t.Fatalf("nested parents not created\n---\n%s", s)
	}
	// 既有内容与注释不受影响
	if !strings.Contains(s, "/* 块注释 */") || !strings.Contains(s, "glm-air") {
		t.Fatalf("existing content damaged\n---\n%s", s)
	}
}

func TestSetKeepsTwoSpaceIndentStyle(t *testing.T) {
	out, err := SetJSONC([]byte("{\n  \"a\": 1\n}"), []string{"b"}, map[string]any{"c": 2})
	if err != nil {
		t.Fatalf("SetJSONC: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "\n  \"b\": {\n    \"c\": 2\n  }") {
		t.Fatalf("indent style not followed\n---\n%s", s)
	}
}

func TestUnsetMiddleKeepsSiblings(t *testing.T) {
	in := "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}"
	out, err := UnsetJSONC([]byte(in), []string{"b"})
	if err != nil {
		t.Fatalf("UnsetJSONC: %v", err)
	}
	if got := string(out); got != "{\n  \"a\": 1,\n  \"c\": 3\n}" {
		t.Fatalf("got\n%q\nwant\n%q", got, "{\n  \"a\": 1,\n  \"c\": 3\n}")
	}
}

func TestUnsetLastRemovesPrecedingComma(t *testing.T) {
	in := "{\n  \"a\": 1,\n  \"b\": 2\n}"
	out, err := UnsetJSONC([]byte(in), []string{"b"})
	if err != nil {
		t.Fatalf("UnsetJSONC: %v", err)
	}
	if got := string(out); got != "{\n  \"a\": 1\n}" {
		t.Fatalf("got\n%q", got)
	}
	// 严格 JSON 必须可通过（无尾逗号）
	if err := json.Unmarshal(out, &map[string]any{}); err != nil {
		t.Fatalf("strict json invalid: %v\n%s", err, out)
	}
}

func TestUnsetOnlyMemberCollapsesToEmptyObject(t *testing.T) {
	out, err := UnsetJSONC([]byte("{\n  \"a\": 1\n}"), []string{"a"})
	if err != nil {
		t.Fatalf("UnsetJSONC: %v", err)
	}
	if err := json.Unmarshal(out, &map[string]any{}); err != nil {
		t.Fatalf("strict json invalid: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "\"a\"") {
		t.Fatalf("key not removed\n---\n%s", out)
	}
}

func TestUnsetMissingIsIdempotent(t *testing.T) {
	in := []byte(sampleJSONC)
	out, err := UnsetJSONC(in, []string{"nope"})
	if err != nil {
		t.Fatalf("UnsetJSONC: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("idempotent unset changed bytes")
	}
	out2, err := UnsetJSONC(in, []string{"env", "nope"})
	if err != nil || !bytes.Equal(out2, in) {
		t.Fatalf("idempotent nested unset changed bytes: %v", err)
	}
}

func TestSetThenUnsetRoundTripIsIdempotent(t *testing.T) {
	in := []byte("{\n  \"a\": 1\n}")
	out, err := SetJSONC(in, []string{"yardmaster-temp"}, "x")
	if err != nil {
		t.Fatalf("SetJSONC: %v", err)
	}
	out2, err := UnsetJSONC(out, []string{"yardmaster-temp"})
	if err != nil {
		t.Fatalf("UnsetJSONC: %v", err)
	}
	if err := json.Unmarshal(out2, &map[string]any{}); err != nil {
		t.Fatalf("strict json invalid: %v\n%s", err, out2)
	}
	if strings.Contains(string(out2), "yardmaster-temp") {
		t.Fatalf("key not removed\n---\n%s", out2)
	}
}

func TestBOMAndCRLFPreserved(t *testing.T) {
	in := append([]byte{0xEF, 0xBB, 0xBF}, []byte("{\r\n  \"a\": 1,\r\n  \"b\": 2,\r\n  \"c\": 3\r\n}\r\n")...)
	out, err := UnsetJSONC(in, []string{"b"})
	if err != nil {
		t.Fatalf("UnsetJSONC: %v", err)
	}
	if !bytes.HasPrefix(out, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("BOM lost")
	}
	body := string(out[3:])
	if !strings.Contains(body, "\"a\": 1,\r\n") {
		t.Fatalf("CRLF style broken (a line)\n---\n%q", body)
	}
	if !strings.Contains(body, "\r\n  \"c\": 3") {
		t.Fatalf("CRLF style broken (c line)\n---\n%q", body)
	}
	if strings.Contains(body, "\r\r") {
		t.Fatalf("orphan \\r artifact\n---\n%q", body)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("strict json invalid: %v\n%q", err, body)
	}
	if _, ok := m["b"]; ok {
		t.Fatal("key b not removed")
	}
}

func TestUnsetKeepsParentObject(t *testing.T) {
	// F4：只删名下键；父级变空也保留（不级联删除）。
	out, err := UnsetJSONC([]byte(sampleJSONC), []string{"env", "ANTHROPIC_BASE_URL"})
	if err != nil {
		t.Fatalf("UnsetJSONC: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"env"`) {
		t.Fatalf("parent env must survive\n---\n%s", s)
	}
	if strings.Contains(s, "ANTHROPIC_BASE_URL") {
		t.Fatal("key not removed")
	}
	if !strings.Contains(s, "/* 块注释 */") {
		t.Fatalf("comment inside env damaged\n---\n%s", s)
	}
}

func TestSetOnScalarPathErrors(t *testing.T) {
	if _, err := SetJSONC([]byte(sampleJSONC), []string{"model", "sub"}, 1); err == nil {
		t.Fatal("want error when intermediate is scalar")
	}
}

func TestPlainJSONIsSupported(t *testing.T) {
	out, err := SetJSONC([]byte(`{"a":1,"b":[1,2]}`), []string{"c"}, []any{1, 2})
	if err != nil {
		t.Fatalf("SetJSONC: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("json invalid: %v\n%s", err, out)
	}
	if len(m) != 3 {
		t.Fatalf("want 3 keys, got %v", m)
	}
}

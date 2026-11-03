// env 编辑器测试：键级替换、追加、删除幂等、注释/空行/用户键保留、
// BOM 与 CRLF 保留（M9-W2 gemini-cli .env 载体）。
package edit

import (
	"strings"
	"testing"
)

const userEnv = `# 用户注释
A=1
CUSTOM=keep
# 尾注释
`

func TestSetEnvReplacesExistingKeepsRest(t *testing.T) {
	out := string(SetEnv([]byte(userEnv), "A", "http://127.0.0.1:8317"))
	for _, want := range []string{
		"# 用户注释",
		"A=http://127.0.0.1:8317",
		"CUSTOM=keep",
		"# 尾注释",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "A=1") {
		t.Fatalf("旧值未替换\n%s", out)
	}
}

func TestSetEnvAppendsMissing(t *testing.T) {
	out := string(SetEnv([]byte(userEnv), "NEW_KEY", "v"))
	if !strings.Contains(out, "NEW_KEY=v") || !strings.Contains(out, "CUSTOM=keep") {
		t.Fatalf("追加失败\n%s", out)
	}
}

func TestUnsetEnvIdempotentScoped(t *testing.T) {
	out := string(UnsetEnv([]byte(userEnv), "A"))
	if strings.Contains(out, "A=1") || !strings.Contains(out, "CUSTOM=keep") || !strings.Contains(out, "# 用户注释") {
		t.Fatalf("删除范围异常\n%s", out)
	}
	if again := string(UnsetEnv([]byte(userEnv), "MISSING")); again != userEnv {
		t.Fatalf("缺失键应幂等原样返回\n%s", again)
	}
}

func TestGetEnv(t *testing.T) {
	if v, ok := GetEnv([]byte(userEnv), "CUSTOM"); !ok || v != "keep" {
		t.Fatalf("GetEnv = %q %v", v, ok)
	}
	if _, ok := GetEnv([]byte(userEnv), "MISSING"); ok {
		t.Fatal("缺失键不应命中")
	}
}

func TestSetEnvRoundTripRestore(t *testing.T) {
	// 追加的键：set+unset 逐字节还原
	out := UnsetEnv(SetEnv([]byte(userEnv), "NEW_KEY", "x"), "NEW_KEY")
	if string(out) != userEnv {
		t.Fatalf("追加键 set+unset 应逐字节还原\n---got---\n%s\n---want---\n%s", out, userEnv)
	}
	// 已有键：set 原位替换，unset 删除该行（行级语义；接线场景用户
	// 文件走 stash 逐字节还原，unset 仅作用于软件创建的文件）
	out = UnsetEnv(SetEnv([]byte(userEnv), "A", "x"), "A")
	if strings.Contains(string(out), "A=") || !strings.Contains(string(out), "CUSTOM=keep") {
		t.Fatalf("已有键 set+unset 应删除该行且不动邻居\n%s", out)
	}
}

func TestEnvBOMAndCRLFPreserved(t *testing.T) {
	data := []byte("# 注释\r\nA=1\r\nB=2\r\n")
	out := SetEnv(withBOM(data), "A", "x")
	if out[0] != 0xEF || out[1] != 0xBB || out[2] != 0xBF {
		t.Fatal("BOM 应保留")
	}
	s := string(out)
	if !strings.Contains(s, "A=x") || !strings.Contains(s, "B=2\r\n") {
		t.Fatalf("CRLF 应保留\n%q", s)
	}
}

func TestIsEmptyEnv(t *testing.T) {
	if !IsEmptyEnv([]byte("# only comment\n\n")) {
		t.Fatal("纯注释应视为空")
	}
	if IsEmptyEnv([]byte("# comment\nA=1\n")) {
		t.Fatal("有键不算空")
	}
}

func withBOM(data []byte) []byte {
	return append([]byte{0xEF, 0xBB, 0xBF}, data...)
}

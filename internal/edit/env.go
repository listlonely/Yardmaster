// env 文件键级编辑（Gemini CLI 的 ~/.gemini/.env 等由 Agent 自行加载的
// KEY=VALUE 载体）：行级拼接，只改动名下键所在行，注释、空行、顺序、
// 其余内容逐字节保留；键不存在时追加到文件尾。
//
// 与 jsonc/toml 同纪律：读不懂不动（本实现按行匹配 `KEY=VALUE`，
// 读不懂的行原样保留），BOM 与 CRLF 保留。
package edit

import "strings"

// GetEnv 读取 KEY 的值（去除值两侧空白；不解析引号——env 值按 dotenv
// 惯例原样生效，写入方也不加引号）。
func GetEnv(data []byte, key string) (string, bool) {
	for _, ln := range splitLines(data) {
		if k, v, ok := splitEnvLine(ln); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// SetEnv 设置 KEY=value：已有行原位替换（保留行尾注释与缩进），
// 不存在时追加到文件尾（前置空行衔接）。
func SetEnv(data []byte, key, value string) []byte {
	bom, body := splitBOM(data)
	lines := splitLines(body)
	eol := detectEOL(body)
	for i, ln := range lines {
		if k, _, ok := splitEnvLine(ln); ok && k == key {
			indent, comment := envIndentComment(ln)
			lines[i] = indent + key + "=" + value + comment
			return append(bom, joinLines(lines, eol)...)
		}
	}
	if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
		lines[n-1] += eol // 残余行无换行：补 EOL 后再追加（不加空行，保 unset 逐字节还原）
	}
	lines = append(lines, key+"="+value+eol)
	return append(bom, joinLines(lines, eol)...)
}

// UnsetEnv 删除 KEY 所在行；不存在时幂等返回原数据。空注释行保留
// （只删名下键）。
func UnsetEnv(data []byte, key string) []byte {
	bom, body := splitBOM(data)
	lines := splitLines(body)
	eol := detectEOL(body)
	for i, ln := range lines {
		if k, _, ok := splitEnvLine(ln); ok && k == key {
			return append(bom, joinLines(append(lines[:i:i], lines[i+1:]...), eol)...)
		}
	}
	return data
}

// IsEmptyEnv 判断 env 文件是否只剩注释/空白（Unwire 级联清理条件）。
func IsEmptyEnv(data []byte) bool {
	for _, ln := range splitLines(data) {
		t := strings.TrimSpace(ln)
		if t != "" && !strings.HasPrefix(t, "#") {
			return false
		}
	}
	return true
}

// splitEnvLine 拆出 `KEY=VALUE` 行的键与值；注释行/空行/无等号行不匹配。
// 键名两侧空白容忍（dotenv 惯例）；值原样保留（不去引号）。
func splitEnvLine(ln string) (key, value string, ok bool) {
	t := strings.TrimSpace(ln)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "export ") {
		return "", "", false
	}
	eq := strings.Index(t, "=")
	if eq <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(t[:eq])
	if key == "" || strings.ContainsAny(key, " \t\"'") {
		return "", "", false
	}
	return key, strings.TrimSpace(t[eq+1:]), true
}

// envIndentComment 拆出行的缩进与行尾注释（# 起，env 值不含 # 时安全；
// 含 # 的值写入时不保留原注释——原样替换整行值部分）。
func envIndentComment(ln string) (indent, comment string) {
	i := 0
	for i < len(ln) && (ln[i] == ' ' || ln[i] == '\t') {
		i++
	}
	indent = ln[:i]
	if j := strings.Index(ln[i:], " #"); j >= 0 {
		comment = ln[i+j:]
	}
	return indent, comment
}

// splitBOM 拆出 UTF-8 BOM 前缀（保留字节原样回写）。
func splitBOM(data []byte) (bom, body []byte) {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[:3], data[3:]
	}
	return nil, data
}

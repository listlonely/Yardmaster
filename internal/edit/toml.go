// TOML 键级编辑（Codex 等使用 TOML 配置的 Agent）：行级拼接，
// 只改动目标键/表所在行，注释、顺序、其余内容逐字节保留。
//
// 已知边界（见 docs/subsystems/edit.md）：不支持多行数组/内嵌表的
// 行内匹配（目标键均为单行标量，覆盖 MVP Agent 配置需求）。
package edit

import (
	"fmt"
	"strconv"
	"strings"
)

// SetTOML 在 table（表路径，nil/空=顶层；缺失时在文件末尾补建，
// TOML 允许后置定义父表）下设置 key=value，保留既有键的行尾注释。
func SetTOML(data []byte, table []string, key string, value any) ([]byte, error) {
	lines := splitLines(data)
	eol := detectEOL(data)
	sec := findSection(lines, table)
	val := tomlValue(value)

	if sec.found {
		for i := sec.bodyStart; i < sec.bodyEnd; i++ {
			if k, _, isKey := splitTOMLKey(lines[i]); isKey && k == key {
				indent, comment := lineIndentOf(lines[i])
				lines[i] = indent + key + " = " + val + comment
				return joinLines(lines, eol), nil
			}
		}
		// 新键插在表内最后一个键之后（空表则紧跟表头；顶层为文件最前）
		insertAt := sec.lastKeyIdx + 1
		lines = insertLine(lines, insertAt, key+" = "+val, eol)
		return joinLines(lines, eol), nil
	}
	// 表缺失：文件末尾补建
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
		lines = append(lines, "")
	}
	lines = append(lines, "["+strings.Join(table, ".")+"]", key+" = "+val)
	return joinLines(lines, eol), nil
}

// UnsetTOML 删除 table 下的 key；不存在时幂等空操作。空表头保留
// （只删名下键，§4.3）。
func UnsetTOML(data []byte, table []string, key string) ([]byte, error) {
	lines := splitLines(data)
	eol := detectEOL(data)
	sec := findSection(lines, table)
	if !sec.found {
		return data, nil
	}
	for i := sec.bodyStart; i < sec.bodyEnd; i++ {
		if k, _, isKey := splitTOMLKey(lines[i]); isKey && k == key {
			lines = append(lines[:i], lines[i+1:]...)
			return joinLines(lines, eol), nil
		}
	}
	return data, nil
}

// GetTOML 读取 table 下 key 的值（string/bool/int64/float64/原始串）。
func GetTOML(data []byte, table []string, key string) (any, bool) {
	lines := splitLines(data)
	sec := findSection(lines, table)
	if !sec.found {
		return nil, false
	}
	for i := sec.bodyStart; i < sec.bodyEnd; i++ {
		if k, rhs, isKey := splitTOMLKey(lines[i]); isKey && k == key {
			return parseTOMLValue(strings.TrimSpace(rhs)), true
		}
	}
	return nil, false
}

// ---------- 内部 ----------

type tomlSection struct {
	found      bool
	headerIdx  int // 表头行；顶层为 -1
	bodyStart  int // 键区起始（顶层 0，有表头则 headerIdx+1）
	bodyEnd    int // 键区结束（下一表头行或文件尾）
	lastKeyIdx int
}

// findSection 定位表区域。所有表头先收集，键区边界 = 本表头+1 到
// 下一表头；顶层区域 = 文件首到第一个表头。
func findSection(lines []string, table []string) tomlSection {
	type header struct {
		path []string
		idx  int
	}
	var headers []header
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") && !strings.HasPrefix(t, "[[") {
			headers = append(headers, header{parseTablePath(t), i})
		}
	}
	if len(table) == 0 {
		s := tomlSection{found: true, headerIdx: -1, bodyStart: 0, lastKeyIdx: -1}
		s.bodyEnd = len(lines)
		if len(headers) > 0 {
			s.bodyEnd = headers[0].idx
		}
		for i := s.bodyStart; i < s.bodyEnd; i++ {
			if isTOMLKeyLine(lines[i]) {
				s.lastKeyIdx = i
			}
		}
		return s
	}
	want := strings.Join(table, ".")
	for hi, h := range headers {
		if strings.Join(h.path, ".") != want {
			continue
		}
		s := tomlSection{found: true, headerIdx: h.idx, bodyStart: h.idx + 1, lastKeyIdx: h.idx}
		s.bodyEnd = len(lines)
		if hi+1 < len(headers) {
			s.bodyEnd = headers[hi+1].idx
		}
		for i := s.bodyStart; i < s.bodyEnd; i++ {
			if isTOMLKeyLine(lines[i]) {
				s.lastKeyIdx = i
			}
		}
		return s
	}
	return tomlSection{found: false}
}

func parseTablePath(header string) []string {
	inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(header), "["), "]")
	return strings.Split(inner, ".")
}

func isTOMLKeyLine(ln string) bool {
	t := strings.TrimSpace(ln)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[") {
		return false
	}
	_, _, ok := splitTOMLKey(ln)
	return ok
}

// splitTOMLKey 拆出键名与值部分；值部分剥离引号外的行尾注释。
func splitTOMLKey(ln string) (key, rhs string, ok bool) {
	t := strings.TrimSpace(ln)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	eq := strings.Index(t, "=")
	if eq <= 0 {
		return "", "", false
	}
	rawKey := strings.TrimSpace(t[:eq])
	if strings.HasPrefix(rawKey, "\"") && strings.HasSuffix(rawKey, "\"") && len(rawKey) >= 2 {
		if unquoted, err := strconv.Unquote(rawKey); err == nil {
			key = unquoted
		} else {
			key = strings.Trim(rawKey, "\"")
		}
	} else {
		key = rawKey
	}
	return key, stripComment(t[eq+1:]), true
}

// stripComment 去掉值部分的行尾注释（引号外的 # 起）。
func stripComment(s string) string {
	inStr := false
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return strings.TrimSpace(s[:j])
			}
		}
	}
	return strings.TrimSpace(s)
}

// lineIndentOf 拆出行首缩进与行尾注释（仅当存在引号外的 #）。
func lineIndentOf(ln string) (indent, comment string) {
	i := 0
	for i < len(ln) && (ln[i] == ' ' || ln[i] == '\t') {
		i++
	}
	indent = ln[:i]
	inStr := false
	for j := i; j < len(ln); j++ {
		switch ln[j] {
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return indent, " " + ln[j:]
			}
		}
	}
	return indent, ""
}

// tomlValue 序列化标量/标量数组为 TOML 值；复合结构不支持（用表表达）。
func tomlValue(v any) string {
	switch t := v.(type) {
	case string:
		return strconv.Quote(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, tomlValue(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case nil:
		return "\"\""
	default:
		return strconv.Quote(fmt.Sprint(v))
	}
}

func parseTOMLValue(s string) any {
	if s == "" {
		return ""
	}
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	if strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") && len(s) >= 2 {
		if v, err := strconv.Unquote(s); err == nil {
			return v
		}
		return strings.Trim(s, "\"")
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// ---------- 行工具（保留 CRLF/LF） ----------

// splitLines 按换行拆分，每行保留自己的 EOL；末尾无换行的残余行也保留。
func splitLines(data []byte) []string {
	s := string(data)
	if s == "" {
		return nil
	}
	var out []string
	for {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			out = append(out, s)
			return out
		}
		out = append(out, s[:idx+1])
		s = s[idx+1:]
		if s == "" {
			return out
		}
	}
}

func joinLines(lines []string, eol string) []byte {
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln)
		if !strings.HasSuffix(ln, "\n") {
			b.WriteString(eol)
		}
	}
	return []byte(b.String())
}

func detectEOL(data []byte) string {
	if strings.Contains(string(data), "\r\n") {
		return "\r\n"
	}
	return "\n"
}

func insertLine(lines []string, at int, line, eol string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:at]...)
	out = append(out, line+eol)
	out = append(out, lines[at:]...)
	return out
}

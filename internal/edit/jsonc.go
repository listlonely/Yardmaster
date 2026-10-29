// JSONC 键级编辑：只改动目标键的原始字节范围，其余内容（注释、顺序、
// 空行、缩进、BOM、CRLF）逐字节保留——配置改写铁律（开发文档 §4.3）的
// 核心实现。普通 JSON 是 JSONC 的子集，因此同一实现覆盖两者。
//
// 实现模型：解析出每个成员在源文中的原始区间 [keyStart, commaEnd)，
// Set 即替换值区间，Unset 即删除成员区间并修复相邻逗号；注释位于
// 成员之间的间隙中，天然不受拼接影响。
package edit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// JSONCDoc 是一份可编辑的 JSONC 文档。
type JSONCDoc struct {
	src    []byte // 不含 BOM
	hasBOM bool
	root   *jsoncNode
	stale  bool // 拼接后树失效，下次操作前重解析
}

type jsoncNode struct {
	kind    byte // 'o' 对象 / 'a' 数组 / 's' 标量
	start   int
	end     int // 值结束位置（闭括号之后）
	members []jsoncMember
}

type jsoncMember struct {
	key      string // 数组元素为 ""
	keyStart int
	valStart int
	valEnd   int
	commaEnd int // 尾逗号之后的位置；-1 表示无
	node     *jsoncNode
}

// ParseJSONC 解析 JSONC 内容；带 BOM 时记录并在 Bytes 中还原。
func ParseJSONC(data []byte) (*JSONCDoc, error) {
	d := &JSONCDoc{}
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		d.hasBOM = true
		data = data[3:]
	}
	d.src = data
	if err := d.reparse(); err != nil {
		return nil, err
	}
	return d, nil
}

// Set 写入 path 指向的键；缺失的中间对象自动创建；其余字节保持原样。
func (d *JSONCDoc) Set(path []string, value any) error {
	if len(path) == 0 {
		return errors.New("edit: 空路径")
	}
	if err := d.ensureFresh(); err != nil {
		return err
	}
	if err := d.set(path, value); err != nil {
		return err
	}
	d.stale = true
	return nil
}

// Unset 删除 path 指向的键（仅该键，父级对象保留）；键不存在时为幂等
// 空操作。删除最后一个成员时同步移除前一成员的尾逗号。
func (d *JSONCDoc) Unset(path []string) error {
	if len(path) == 0 {
		return errors.New("edit: 空路径")
	}
	if err := d.ensureFresh(); err != nil {
		return err
	}
	changed, err := d.unset(path)
	if err != nil || !changed {
		return err
	}
	d.stale = true
	return nil
}

// Bytes 返回编辑后的完整文件内容（保留原有 BOM）。
func (d *JSONCDoc) Bytes() []byte {
	if d.hasBOM {
		return append([]byte{0xEF, 0xBB, 0xBF}, d.src...)
	}
	return d.src
}

// SetJSONC 一次性助手：在 data 中设置键并返回新内容。
func SetJSONC(data []byte, path []string, value any) ([]byte, error) {
	d, err := ParseJSONC(data)
	if err != nil {
		return nil, err
	}
	if err := d.Set(path, value); err != nil {
		return nil, err
	}
	return d.Bytes(), nil
}

// UnsetJSONC 一次性助手：从 data 中删除键并返回新内容；键不存在时
// 原样返回（幂等）。
func UnsetJSONC(data []byte, path []string) ([]byte, error) {
	d, err := ParseJSONC(data)
	if err != nil {
		return nil, err
	}
	if err := d.Unset(path); err != nil {
		return nil, err
	}
	return d.Bytes(), nil
}

// ---------- 内部：解析 ----------

func (d *JSONCDoc) reparse() error {
	p := &jsoncParser{src: d.src}
	n, err := p.parseValue()
	if err != nil {
		return err
	}
	p.skipTrivia()
	if p.pos != len(p.src) {
		return fmt.Errorf("edit: 位置 %d 起存在多余内容", p.pos)
	}
	d.root = n
	d.stale = false
	return nil
}

func (d *JSONCDoc) ensureFresh() error {
	if d.stale {
		return d.reparse()
	}
	return nil
}

type jsoncParser struct {
	src []byte
	pos int
}

func (p *jsoncParser) skipTrivia() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			p.pos++
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '/':
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		case c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '*':
			p.pos += 2
			for p.pos+1 < len(p.src) && !(p.src[p.pos] == '*' && p.src[p.pos+1] == '/') {
				p.pos++
			}
			if p.pos+1 < len(p.src) {
				p.pos += 2
			}
		default:
			return
		}
	}
}

func (p *jsoncParser) parseValue() (*jsoncNode, error) {
	p.skipTrivia()
	start := p.pos
	if p.pos >= len(p.src) {
		return nil, errors.New("edit: 意外的 JSONC 结尾")
	}
	switch p.src[p.pos] {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '"':
		end, err := p.scanString()
		if err != nil {
			return nil, err
		}
		return &jsoncNode{kind: 's', start: start, end: end}, nil
	default:
		end, err := p.scanScalar()
		if err != nil {
			return nil, err
		}
		return &jsoncNode{kind: 's', start: start, end: end}, nil
	}
}

// scanString 读取一个带引号的字符串 token，返回结束位置（引号之后）。
func (p *jsoncParser) scanString() (int, error) {
	i := p.pos + 1
	for i < len(p.src) {
		switch p.src[i] {
		case '\\':
			i += 2
		case '"':
			p.pos = i + 1
			return i + 1, nil
		default:
			i++
		}
	}
	return 0, errors.New("edit: 字符串未闭合")
}

// scanScalar 读取数字/true/false/null 标量，遇到分隔符即止。
func (p *jsoncParser) scanScalar() (int, error) {
	i := p.pos
	for i < len(p.src) {
		c := p.src[i]
		if c == ',' || c == '}' || c == ']' || c == '\n' || c == '\r' || c == ' ' || c == '\t' || c == '/' {
			break
		}
		i++
	}
	if i == p.pos {
		return 0, fmt.Errorf("edit: 位置 %d 处不是合法的 JSONC 值", p.pos)
	}
	p.pos = i
	return i, nil
}

func (p *jsoncParser) parseObject() (*jsoncNode, error) {
	open := p.pos
	p.pos++ // '{'
	n := &jsoncNode{kind: 'o', start: open}
	p.skipTrivia()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		n.end = p.pos
		return n, nil
	}
	for {
		p.skipTrivia()
		if p.pos >= len(p.src) {
			return nil, errors.New("edit: JSONC 对象未闭合")
		}
		m := jsoncMember{commaEnd: -1}
		m.keyStart = p.pos
		if p.src[p.pos] != '"' {
			return nil, fmt.Errorf("edit: 位置 %d 处对象键必须是字符串", p.pos)
		}
		keyEnd, err := p.scanString()
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(p.src[m.keyStart:keyEnd], &m.key); err != nil {
			return nil, fmt.Errorf("edit: 解析对象键失败: %w", err)
		}
		p.skipTrivia()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, fmt.Errorf("edit: 位置 %d 附近缺少 ':'", p.pos)
		}
		p.pos++
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		m.valStart, m.valEnd, m.node = val.start, val.end, val
		p.skipTrivia()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			m.commaEnd = p.pos
		}
		n.members = append(n.members, m)
		p.skipTrivia()
		if p.pos >= len(p.src) {
			return nil, errors.New("edit: JSONC 对象未闭合")
		}
		if p.src[p.pos] == '}' {
			p.pos++
			n.end = p.pos
			return n, nil
		}
	}
}

func (p *jsoncParser) parseArray() (*jsoncNode, error) {
	open := p.pos
	p.pos++ // '['
	n := &jsoncNode{kind: 'a', start: open}
	p.skipTrivia()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		n.end = p.pos
		return n, nil
	}
	for {
		p.skipTrivia()
		if p.pos >= len(p.src) {
			return nil, errors.New("edit: JSONC 数组未闭合")
		}
		m := jsoncMember{commaEnd: -1}
		m.keyStart = p.pos
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		m.valStart, m.valEnd, m.node = val.start, val.end, val
		p.skipTrivia()
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			m.commaEnd = p.pos
		}
		n.members = append(n.members, m)
		p.skipTrivia()
		if p.pos >= len(p.src) {
			return nil, errors.New("edit: JSONC 数组未闭合")
		}
		if p.src[p.pos] == ']' {
			p.pos++
			n.end = p.pos
			return n, nil
		}
	}
}

// ---------- 内部：编辑 ----------

func (d *JSONCDoc) set(path []string, value any) error {
	cur := d.root
	if cur.kind != 'o' {
		return errors.New("edit: 文档根不是对象")
	}
	for i, k := range path {
		last := i == len(path)-1
		idx := findMember(cur, k)
		if last {
			if idx >= 0 {
				m := cur.members[idx]
				ind := lineIndent(d.src, m.keyStart)
				repl := formatValue(value, ind, detectIndent(d.src))
				d.src = spliceBytes(d.src, m.valStart, m.valEnd, repl)
				return nil
			}
			return d.insertMember(cur, k, value)
		}
		if idx < 0 {
			// 中间层缺失：先建空对象，重解析后重走（递归深度=路径长度）
			if err := d.insertMember(cur, k, map[string]any{}); err != nil {
				return err
			}
			if err := d.reparse(); err != nil {
				return err
			}
			return d.set(path, value)
		}
		m := cur.members[idx]
		if m.node.kind != 'o' {
			return fmt.Errorf("edit: %q 已存在且不是对象，无法在其下创建 %q", k, path[i+1])
		}
		cur = m.node
	}
	return nil
}

func (d *JSONCDoc) insertMember(parent *jsoncNode, key string, value any) error {
	if parent.kind != 'o' {
		return errors.New("edit: 只能在对象中插入键")
	}
	unit := detectIndent(d.src)
	kb, _ := json.Marshal(key) // 键的转义形式
	if len(parent.members) == 0 {
		ind := lineIndent(d.src, parent.start) + unit
		val := formatValue(value, ind, unit)
		var text []byte
		text = append(text, kb...)
		text = append(text, []byte(": ")...)
		text = append(text, val...)
		if bytes.ContainsRune(d.src[parent.start+1:parent.end-1], '\n') {
			text = append([]byte("\n"+ind), text...)
		}
		d.src = spliceBytes(d.src, parent.end-1, parent.end-1, text)
		return nil
	}
	last := parent.members[len(parent.members)-1]
	ind := lineIndent(d.src, last.keyStart)
	var text []byte
	text = append(text, kb...)
	text = append(text, []byte(": ")...)
	text = append(text, formatValue(value, ind, unit)...)
	if last.commaEnd >= 0 {
		// 已有尾逗号：在新行插入成员（行尾注释随原位置保留）
		d.src = spliceBytes(d.src, last.commaEnd, last.commaEnd, append([]byte("\n"+ind), text...))
		return nil
	}
	// 补逗号 + 新成员；插在值之后、行尾注释之前（若有）
	d.src = spliceBytes(d.src, last.valEnd, last.valEnd, append([]byte(",\n"+ind), text...))
	return nil
}

func (d *JSONCDoc) unset(path []string) (bool, error) {
	cur := d.root
	if cur.kind != 'o' {
		return false, errors.New("edit: 文档根不是对象")
	}
	for i, k := range path {
		last := i == len(path)-1
		idx := findMember(cur, k)
		if idx < 0 {
			return false, nil // 键或父级不存在：幂等空操作
		}
		m := cur.members[idx]
		if !last {
			if m.node.kind != 'o' {
				return false, nil // 中间层不是对象：目标不存在，幂等
			}
			cur = m.node
			continue
		}
		start := m.keyStart
		// 回溯吞并本行前导空白与前一行换行（含配对 \r），整行干净移除。
		// 循环停在跨行边界前的最后一个空白上：LF 下 \n 在 j-1；CRLF 下
		// \n 同样在 j-1（\r 属于本行前导空白被吞并，\r\n 的 \r 由下步配对）。
		j := start
		for j > 0 && (d.src[j-1] == ' ' || d.src[j-1] == '\t' || d.src[j-1] == '\r') {
			j--
		}
		if j > 0 && d.src[j-1] == '\n' {
			start = j - 1
			if start > 0 && d.src[start-1] == '\r' {
				start--
			}
		}
		end := m.valEnd
		if m.commaEnd >= 0 {
			end = m.commaEnd
		} else if idx > 0 {
			// 最后一个成员：一并移除前一成员的尾逗号，避免残留尾逗号。
			// 回溯集合含 \n：跨过行终止符找前一行行尾的逗号。
			p := start
			for p > 0 && (d.src[p-1] == ' ' || d.src[p-1] == '\t' || d.src[p-1] == '\r' || d.src[p-1] == '\n') {
				p--
			}
			if p > 0 && d.src[p-1] == ',' {
				start = p - 1
			}
		}
		d.src = spliceBytes(d.src, start, end, nil)
		return true, nil
	}
	return false, nil
}

func findMember(n *jsoncNode, key string) int {
	for i := range n.members {
		if n.members[i].key == key {
			return i
		}
	}
	return -1
}

// ---------- 内部：格式化与工具 ----------

// formatValue 把 value 序列化为 JSONC 片段：对象/数组多行缩进，标量单行。
// baseIndent 为成员键所在行的缩进，unit 为每层缩进单位。
func formatValue(v any, baseIndent, unit string) []byte {
	var b bytes.Buffer
	writeJSONCValue(&b, v, baseIndent, unit)
	return b.Bytes()
}

func writeJSONCValue(b *bytes.Buffer, v any, indent, unit string) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			b.WriteString("{}")
			return
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("{\n")
		for i, k := range keys {
			b.WriteString(indent + unit)
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteString(": ")
			writeJSONCValue(b, t[k], indent+unit, unit)
			if i < len(keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range t {
			b.WriteString(indent + unit)
			writeJSONCValue(b, e, indent+unit, unit)
			if i < len(t)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	default:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			// 不可编码值兜底为字符串，避免静默丢失
			s, _ := json.Marshal(fmt.Sprint(v))
			b.Write(s)
			return
		}
		b.Write(bytes.TrimRight(buf.Bytes(), "\n"))
	}
}

// detectIndent 探测文件缩进单位（首个带缩进行的空白前缀），默认两个空格。
func detectIndent(src []byte) string {
	for i := 0; i < len(src); i++ {
		if src[i] != '\n' {
			continue
		}
		j := i + 1
		for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
			j++
		}
		if j > i+1 && j < len(src) && src[j] != '\r' && src[j] != '\n' {
			return string(src[i+1 : j])
		}
	}
	return "  "
}

// lineIndent 返回 pos 所在行的行首缩进。
func lineIndent(src []byte, pos int) string {
	i := pos
	for i > 0 && src[i-1] != '\n' {
		i--
	}
	j := i
	for j < pos && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	return string(src[i:j])
}

func spliceBytes(src []byte, start, end int, repl []byte) []byte {
	out := make([]byte, 0, len(src)-(end-start)+len(repl))
	out = append(out, src[:start]...)
	out = append(out, repl...)
	out = append(out, src[end:]...)
	return out
}

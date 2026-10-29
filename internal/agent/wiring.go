// 接线引擎（PRD F4/F5）：预览 diff → 接线（备份栈+原子写）→ 断开还原
// → 漂移检测。铁律见开发文档 §4.3：只写名下键、原值进 stash、
// 断开逐字节还原、用户值优先。
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yardmaster/internal/edit"
	"yardmaster/internal/store"
)

// Manager 是接线引擎；Addr 为网关地址（含 scheme）。
type Manager struct {
	Paths store.Paths
	Addr  string
}

// State 是接线状态（PRD 规则页「Agent 接线状态」）。
type State string

const (
	StateMissing State = "missing" // 未安装
	StateUnwired State = "unwired" // 未接入
	StateWired   State = "wired"   // 已接入
	StateDrift   State = "drift"   // 漂移
	StateSpecTBD State = "spec-tbd"
)

// DriftRow 是一个键的漂移比对结果。
type DriftRow struct {
	Desc     string `json:"desc"`
	Expected string `json:"expected"`
	Current  string `json:"current"`
	Drift    bool   `json:"drift"`
}

type appliedFile struct {
	Original string `json:"original"`
	Stash    string `json:"stash,omitempty"` // 空=文件由本软件创建
}

// WiringRecord 是一次接入的审计记录（F10 改写审计）。
type WiringRecord struct {
	Agent   string        `json:"agent"`
	WiredAt string        `json:"wired_at"`
	Addr    string        `json:"addr"`
	Key     string        `json:"key"`
	Model   string        `json:"model"`
	Files   []appliedFile `json:"files"`
	Ops     []Op          `json:"ops"`
}

type appliedDoc struct {
	Wirings map[string]WiringRecord `json:"wirings"`
}

func (m *Manager) loadApplied() (appliedDoc, error) {
	var doc appliedDoc
	if err := store.LoadJSON(m.Paths.AppliedPath(), &doc); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return appliedDoc{Wirings: map[string]WiringRecord{}}, nil
		}
		return doc, err
	}
	if doc.Wirings == nil {
		doc.Wirings = map[string]WiringRecord{}
	}
	return doc, nil
}

func (m *Manager) saveApplied(doc appliedDoc) error {
	return store.SaveJSON(m.Paths.AppliedPath(), doc)
}

// targetPath 解析 Agent 的配置文件路径：优先已存在的候选，否则第一个。
func targetPath(def Def) (string, error) {
	for _, rel := range def.RelPaths {
		abs, err := ExpandHome(rel)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err == nil {
			return abs, nil
		}
	}
	abs, err := ExpandHome(def.RelPaths[0])
	if err != nil {
		return "", err
	}
	return abs, nil
}

// StateOf 返回 Agent 当前接线状态（含漂移判定）。
func (m *Manager) StateOf(def Def) (State, error) {
	if def.SpecNote != "" {
		return StateSpecTBD, nil
	}
	applied, err := m.loadApplied()
	if err != nil {
		return "", err
	}
	rec, wired := applied.Wirings[def.ID]
	path, err := targetPath(def)
	if err != nil {
		return "", err
	}
	_, statErr := os.Stat(path)
	if !wired {
		if statErr == nil {
			return StateUnwired, nil
		}
		return StateMissing, nil
	}
	rows, err := m.driftFor(def, rec)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Drift {
			return StateDrift, nil
		}
	}
	return StateWired, nil
}

// Preview 返回接线将产生的变更（diff 行）；不改任何文件（F4 预览）。
func (m *Manager) Preview(def Def, model string) ([]string, error) {
	if def.SpecNote != "" {
		return nil, fmt.Errorf("%w: %s", ErrSpecTBD, def.SpecNote)
	}
	path, err := targetPath(def)
	if err != nil {
		return nil, err
	}
	data, _ := os.ReadFile(path)
	key := "gw-" + def.ID
	var lines []string
	for _, op := range def.Ops(m.Addr, key, model) {
		old := "(未设置)"
		if v, ok := getValue(data, def.Format, op); ok {
			old = fmt.Sprint(v)
		}
		lines = append(lines,
			fmt.Sprintf("- %s: %s", op.Desc, old),
			fmt.Sprintf("+ %s: %s", op.Desc, fmt.Sprint(op.Value)),
		)
	}
	lines = append(lines, fmt.Sprintf("@ 文件: %s", path))
	return lines, nil
}

// Wire 执行接线：备份原文件 → 写入 → 记录审计。已接线时幂等更新。
func (m *Manager) Wire(def Def, model string) (WiringRecord, error) {
	if def.SpecNote != "" {
		return WiringRecord{}, fmt.Errorf("%w: %s", ErrSpecTBD, def.SpecNote)
	}
	path, err := targetPath(def)
	if err != nil {
		return WiringRecord{}, err
	}
	data, _ := os.ReadFile(path)
	key := "gw-" + def.ID
	ops := def.Ops(m.Addr, key, model)

	// 备份栈：首次接线保存原始字节；已有备份不得覆盖（二次接线时
	// 首次原始值优先，§4.3）
	stashPath := ""
	if data != nil {
		stashPath = filepath.Join(m.Paths.StashDir(), def.ID, "0")
		if err := os.MkdirAll(filepath.Dir(stashPath), 0o700); err != nil {
			return WiringRecord{}, err
		}
		if _, statErr := os.Stat(stashPath); os.IsNotExist(statErr) {
			if err := os.WriteFile(stashPath, data, 0o600); err != nil {
				return WiringRecord{}, fmt.Errorf("备份原配置: %w", err)
			}
		}
	}

	// 应用
	var out []byte
	switch def.Format {
	case FormatJSONC:
		out = []byte("{}")
		if data != nil {
			out = data
		}
		doc, perr := edit.ParseJSONC(out)
		if perr != nil {
			return WiringRecord{}, fmt.Errorf("解析 %s: %w", path, perr)
		}
		for _, op := range ops {
			if err := doc.Set(op.JSONPath, op.Value); err != nil {
				return WiringRecord{}, fmt.Errorf("%s: %w", op.Desc, err)
			}
		}
		out = doc.Bytes()
	case FormatTOML:
		out = data
		for _, op := range ops {
			var err error
			out, err = edit.SetTOML(out, op.TOMLTable, op.TOMLKey, op.Value)
			if err != nil {
				return WiringRecord{}, fmt.Errorf("%s: %w", op.Desc, err)
			}
		}
	default:
		return WiringRecord{}, fmt.Errorf("不支持的格式 %q", def.Format)
	}
	if err := edit.WriteAtomic(path, out); err != nil {
		return WiringRecord{}, err
	}

	rec := WiringRecord{
		Agent: def.ID, WiredAt: time.Now().Format(time.RFC3339),
		Addr: m.Addr, Key: key, Model: model,
		Files: []appliedFile{{Original: path, Stash: stashPath}},
		Ops:   ops,
	}
	applied, err := m.loadApplied()
	if err != nil {
		return WiringRecord{}, err
	}
	applied.Wirings[def.ID] = rec
	if err := m.saveApplied(applied); err != nil {
		return WiringRecord{}, err
	}
	return rec, nil
}

// Unwire 断开：有备份逐字节还原；软件创建的文件按逆操作删除名下键
// （删除后为空对象则移除文件，F4）。
func (m *Manager) Unwire(def Def) error {
	applied, err := m.loadApplied()
	if err != nil {
		return err
	}
	rec, ok := applied.Wirings[def.ID]
	if !ok {
		return fmt.Errorf("%s 未接线", def.ID)
	}
	for _, f := range rec.Files {
		if f.Stash != "" {
			orig, err := os.ReadFile(f.Stash)
			if err != nil {
				return fmt.Errorf("读取备份 %s: %w", f.Stash, err)
			}
			if err := edit.WriteAtomic(f.Original, orig); err != nil {
				return err
			}
			continue
		}
		// 软件创建的文件：逆操作删除名下键，并级联清理因此变空的
		// 父对象（整个文件都是本软件创建的，空容器也属于名下，F4）
		data, _ := os.ReadFile(f.Original)
		if data == nil {
			continue
		}
		switch def.Format {
		case FormatJSONC:
			doc, perr := edit.ParseJSONC(data)
			if perr != nil {
				return perr
			}
			for _, op := range rec.Ops {
				_ = doc.Unset(op.JSONPath)
			}
			data = pruneJSONDoc(doc.Bytes())
		case FormatTOML:
			for _, op := range rec.Ops {
				var err error
				data, err = edit.UnsetTOML(data, op.TOMLTable, op.TOMLKey)
				if err != nil {
					return err
				}
			}
		}
		if isEmptyConfig(data) {
			_ = os.Remove(f.Original)
			continue
		}
		if err := edit.WriteAtomic(f.Original, data); err != nil {
			return err
		}
	}
	delete(applied.Wirings, def.ID)
	return m.saveApplied(applied)
}

// Drift 检测名下键是否被外部改动（F5）。
func (m *Manager) Drift(def Def) ([]DriftRow, error) {
	applied, err := m.loadApplied()
	if err != nil {
		return nil, err
	}
	rec, ok := applied.Wirings[def.ID]
	if !ok {
		return nil, fmt.Errorf("%s 未接线", def.ID)
	}
	return m.driftFor(def, rec)
}

func (m *Manager) driftFor(def Def, rec WiringRecord) ([]DriftRow, error) {
	var rows []DriftRow
	for _, f := range rec.Files {
		data, err := os.ReadFile(f.Original)
		if err != nil {
			rows = append(rows, DriftRow{Desc: "配置文件", Expected: "存在", Current: "不可读: " + err.Error(), Drift: true})
			continue
		}
		for _, op := range rec.Ops {
			cur := "(键已消失)"
			if v, ok := getValue(data, def.Format, op); ok {
				cur = fmt.Sprint(v)
			}
			want := fmt.Sprint(op.Value)
			rows = append(rows, DriftRow{
				Desc: op.Desc, Expected: want, Current: cur,
				Drift: cur != want,
			})
		}
	}
	return rows, nil
}

// getValue 按格式读取键当前值。
func getValue(data []byte, format Format, op Op) (any, bool) {
	if data == nil {
		return nil, false
	}
	switch format {
	case FormatJSONC:
		v, ok, err := edit.GetJSONC(data, op.JSONPath)
		if err != nil {
			return nil, false
		}
		return v, ok
	case FormatTOML:
		return edit.GetTOML(data, op.TOMLTable, op.TOMLKey)
	}
	return nil, false
}

// isEmptyConfig 判断配置是否只剩空对象/空白（F4：新建文件删除条件）。
func isEmptyConfig(data []byte) bool {
	return len(strings.TrimSpace(string(data))) == 0 ||
		strings.TrimSpace(string(data)) == "{}"
}

// pruneJSONDoc 递归删除变为空对象的容器（仅用于本软件创建的文件——
// Unset 后 provider.yardmaster.options 这类空父级一并清除）。UseNumber
// 保证大整数不失真。
func pruneJSONDoc(data []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return data
	}
	pruned := pruneJSONValue(m)
	out, err := json.Marshal(pruned)
	if err != nil {
		return data
	}
	return out
}

func pruneJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			pv := pruneJSONValue(e)
			if pv == nil {
				delete(t, k) // 子容器已空：连键一起移除
				continue
			}
			t[k] = pv
		}
		if len(t) == 0 {
			return nil
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = pruneJSONValue(e)
		}
		return t
	default:
		return v
	}
}

// 会话持久化（M6-W2）：派发会话终态追加落盘 dispatch-sessions.jsonl
// （与决策 trace 同目录、同 30 天留存策略），serve 启动恢复最近 N 条
// 进内存环形——控制台「派发」页历史跨重启可见。留存语义：仅派发审计
// 所需字段（任务摘要 ≤512 字 + 输出尾部 ≤64KB + 结局/耗时），不含
// Agent 环境变量与完整对话（§4.4 纪律）。
package dispatch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// sessionsKeepDays 是会话留存天数（与 trace 同策略，F9）。
const sessionsKeepDays = 30

// SessionStore 是会话终态的 JSONL 追加落盘器（并发安全）。运行期只追加；
// 启动构造时 compact 一次（过滤超期行并重写）。落盘失败只降级不阻断
// （内存环形与 trace 观测不受影响）。
type SessionStore struct {
	mu       sync.Mutex
	path     string
	keepDays int
}

// NewSessionStore 构造存储并执行一次超期清理（compact）。
func NewSessionStore(path string) *SessionStore {
	s := &SessionStore{path: path, keepDays: sessionsKeepDays}
	s.compact()
	return s
}

// NewSessionStoreWithKeep 显式注入留存天数（测试用）。
func NewSessionStoreWithKeep(path string, keepDays int) *SessionStore {
	s := &SessionStore{path: path, keepDays: keepDays}
	s.compact()
	return s
}

// Append 追加一条终态会话（JSONL 一行）。调用方传锁内快照值；失败记
// 日志不返回错误（持久化是尽力而为的观测增强，不是请求路径）。
func (s *SessionStore) Append(sess Session) {
	b, err := json.Marshal(sess)
	if err != nil {
		slog.Warn("会话序列化失败（跳过落盘）", "session", sess.ID, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		slog.Warn("会话落盘目录创建失败", "err", err)
		return
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("会话落盘失败（不影响派发）", "err", err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		slog.Warn("会话写入失败", "err", err)
	}
}

// Recent 返回留存期内的最近 n 条会话（旧→新序；恢复进内存环形用）。
// 解析失败的行跳过（读不懂=未知）。
func (s *SessionStore) Recent(n int) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		return nil
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// readAll 读取全部可解析会话（旧→新）。调用方须持有 s.mu。
func (s *SessionStore) readAll() ([]Session, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("打开会话文件: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []Session
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20) // Result.Output 可达 64KB
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var sess Session
		if json.Unmarshal(line, &sess) != nil {
			continue // 读不懂=未知：跳过坏行（不影响其余记录）
		}
		out = append(out, sess)
	}
	return out, sc.Err()
}

// compact 启动清理：过滤 keepDays 前的会话并重写文件（坏行顺带剔除）。
func (s *SessionStore) compact() {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		slog.Warn("会话文件读取失败（保留原样，不影响启动）", "path", s.path, "err", err)
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.keepDays)
	kept := make([]Session, 0, len(all))
	for _, sess := range all {
		started, err := time.Parse(time.RFC3339, sess.StartedAt)
		if err != nil || started.Before(cutoff) {
			continue // 时间读不懂=未知：不留（留存纪律宁紧勿松）
		}
		kept = append(kept, sess)
	}
	if len(kept) == len(all) {
		return
	}
	if err := s.rewrite(kept); err != nil {
		slog.Warn("会话文件清理失败（保留原样）", "err", err)
	}
}

// rewrite 用留存会话重写文件（compact 专用）。调用方须持有 s.mu。
func (s *SessionStore) rewrite(kept []Session) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, sess := range kept {
		b, err := json.Marshal(sess)
		if err != nil {
			continue
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.path) // 同目录原子替换
}

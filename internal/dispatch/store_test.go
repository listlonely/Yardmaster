// M6-W2 会话持久化测试：终态追加落盘、30 天 compact、启动恢复进环形
//（含 Output 回退与 running 标失败语义）。
package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustStore(t *testing.T, keepDays int) (*SessionStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace", "dispatch-sessions.jsonl")
	if keepDays < 0 {
		return NewSessionStore(path), path
	}
	return NewSessionStoreWithKeep(path, keepDays), path
}

func doneSession(id, task, output string, started time.Time) Session {
	return Session{
		ID: id, Agent: "a", Task: task, Status: "done",
		StartedAt: started.UTC().Format(time.RFC3339),
		EndedAt:   started.Add(time.Second).UTC().Format(time.RFC3339),
		Result:    &Result{Agent: "a", Outcome: OutcomeOK, Output: output, DurationMS: 1000},
	}
}

// TestSessionStoreAppendRestore：终态落盘 → 新存储恢复，序与内容一致。
func TestSessionStoreAppendRestore(t *testing.T) {
	s, path := mustStore(t, -1)
	now := time.Now()
	s.Append(doneSession("d-1", "任务一", "输出一", now.Add(-2*time.Second)))
	s.Append(doneSession("d-2", "任务二", "输出二", now))

	// 重启语义：全新存储读同一文件
	s2 := &SessionStore{path: path, keepDays: sessionsKeepDays}
	got := s2.Recent(100)
	if len(got) != 2 {
		t.Fatalf("恢复条数 = %d, want 2", len(got))
	}
	if got[0].ID != "d-1" || got[1].ID != "d-2" {
		t.Fatalf("恢复序应旧→新: %s, %s", got[0].ID, got[1].ID)
	}
	if got[1].Result == nil || got[1].Result.Output != "输出二" {
		t.Fatalf("Result.Output 丢失: %+v", got[1].Result)
	}
	// 恢复会话的 Output 回退语义
	if out := got[1].Output(); out != "输出二" {
		t.Fatalf("恢复会话 Output = %q, want Result.Output", out)
	}
}

// TestSessionStoreCompact：超过留存期的会话在构造时被清理重写。
func TestSessionStoreCompact(t *testing.T) {
	s, path := mustStore(t, 7)
	now := time.Now()
	s.Append(doneSession("d-old", "过期任务", "", now.AddDate(0, 0, -40)))
	s.Append(doneSession("d-new", "新任务", "", now))

	// 重启：keepDays=7 → 只剩 d-new
	s2 := NewSessionStoreWithKeep(path, 7)
	got := s2.Recent(100)
	if len(got) != 1 || got[0].ID != "d-new" {
		t.Fatalf("compact 后应只剩 d-new: %+v", got)
	}
	// 文件物理重写（旧行清除）
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "d-old") || len(strings.Split(strings.TrimSpace(string(b)), "\n")) != 1 {
		t.Fatalf("文件应只剩 1 行且不含过期记录: %q", b)
	}
}

// TestRestoreSessionsRunningMarkedFailed：重启时落盘里的 running 会话
// 标失败（进程被杀，不猜结果），且 Wait 立即返回、Snapshot 链路完整。
func TestRestoreSessionsRunningMarkedFailed(t *testing.T) {
	e := NewEngine(nil)
	running := doneSession("d-run", "任务", "输出", time.Now())
	running.Status = "running"
	running.Result = nil
	e.RestoreSessions([]Session{running})

	sessions := e.Sessions()
	if len(sessions) != 1 || sessions[0].Status != "failed" {
		t.Fatalf("恢复 running 应标 failed: %+v", sessions)
	}
	// Wait 不死锁（finished 已关闭）
	sessions[0].Wait()
	// Snapshot 可取（锁内拷贝链路完整）
	if sv, ok := e.Snapshot("d-run"); !ok || sv.ID != "d-run" {
		t.Fatalf("Snapshot 失效: %+v, %v", sv, ok)
	}
}

// TestEnginePersistRestore：引擎挂载存储后 Submit 终态落盘；新引擎
// 恢复后 Sessions/Output 可见（helper 进程真实跑一轮）。
func TestEnginePersistRestore(t *testing.T) {
	s, _ := mustStore(t, -1)
	spec := helperAgent(t, "ok")
	e := mustSpec(t, DispatchFile{Agents: []AgentSpec{{ID: "fake", Command: spec.Command, Env: spec.Env}}})
	e.AttachStore(s)
	sess, err := e.Submit(context.Background(), e.Agents()[0], "持久化任务", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.Wait()
	if sess.Status != "done" {
		t.Fatalf("status = %s", sess.Status)
	}

	// 重启语义：新引擎从文件恢复（无 store——恢复与落盘互不依赖）
	e2 := NewEngine(nil)
	e2.RestoreSessions(s.Recent(100))
	got := e2.Sessions()
	if len(got) != 1 || got[0].ID != sess.ID {
		t.Fatalf("恢复会话不符: %+v", got)
	}
	if got[0].Status != "done" {
		t.Fatalf("恢复终态 = %s", got[0].Status)
	}
	if !strings.Contains(got[0].Output(), "42") {
		t.Fatalf("恢复会话 Output 缺失: %q", got[0].Output())
	}
}

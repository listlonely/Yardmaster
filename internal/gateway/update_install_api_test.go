// M11-W3 管理端点测试：POST /api/update/install——引擎未装配 503、
// 装配成功 200、钩子失败 409。安装引擎本体在 internal/app（依赖方向
// 单向），此处只验网关侧路由与状态码语义。
package gateway

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/store"
)

func doPost(t *testing.T, url, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestUpdateInstallEndpoint(t *testing.T) {
	s, srv := newConfigServer(t, nil)
	st, body := doPost(t, srv.URL, "/api/update/install")
	if st != http.StatusServiceUnavailable {
		t.Fatalf("未装配引擎应 503，得到 HTTP %d: %s", st, body)
	}

	called := 0
	s.OnUpdateInstall = func() error { called++; return nil }
	st, body = doPost(t, srv.URL, "/api/update/install")
	if st != http.StatusOK || !strings.Contains(string(body), "installed") {
		t.Fatalf("装配后应 200 + installed，得到 HTTP %d: %s", st, body)
	}
	if called != 1 {
		t.Fatalf("钩子应恰好调用一次，实际 %d", called)
	}

	s.OnUpdateInstall = func() error { return errors.New("单飞冲突") }
	st, body = doPost(t, srv.URL, "/api/update/install")
	if st != http.StatusConflict || !strings.Contains(string(body), "单飞冲突") {
		t.Fatalf("钩子失败应 409 + 错误透传，得到 HTTP %d: %s", st, body)
	}
}

// POST /api/restart（2026-11-23）：钩子未装配 503、装配成功 200、钩子
// 失败 409。spawn/接力本体在 internal/app，此处只验路由与状态码语义。
func TestRestartEndpoint(t *testing.T) {
	s, srv := newConfigServer(t, nil)
	st, body := doPost(t, srv.URL, "/api/restart")
	if st != http.StatusServiceUnavailable {
		t.Fatalf("未装配重启钩子应 503，得到 HTTP %d: %s", st, body)
	}

	called := 0
	s.OnRestart = func() error { called++; return nil }
	st, body = doPost(t, srv.URL, "/api/restart")
	if st != http.StatusOK || !strings.Contains(string(body), "requested") {
		t.Fatalf("装配后应 200 + requested，得到 HTTP %d: %s", st, body)
	}
	if called != 1 {
		t.Fatalf("钩子应恰好调用一次，实际 %d", called)
	}

	s.OnRestart = func() error { return errors.New("spawn 失败") }
	st, body = doPost(t, srv.URL, "/api/restart")
	if st != http.StatusConflict || !strings.Contains(string(body), "spawn 失败") {
		t.Fatalf("钩子失败应 409 + 错误透传，得到 HTTP %d: %s", st, body)
	}
}

// 设置写入 quota_interval_min（2026-11-23）：合法值采纳并热切换
// Server.QuotaInterval + 落盘；0=保留现值；越界 400 不落盘。
func TestSettingsQuotaInterval(t *testing.T) {
	s, srv := newConfigServer(t, nil)
	put := func(v int) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/config/settings",
			strings.NewReader(fmt.Sprintf(`{"quota_interval_min":%d}`, v)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	if st, body := put(3); st != http.StatusOK {
		t.Fatalf("合法间隔应 200，得到 HTTP %d: %s", st, body)
	}
	if got := s.QuotaInterval; got != 3*time.Minute {
		t.Fatalf("间隔应热切换为 3min，实际 %v", got)
	}
	if st, body := put(0); st != http.StatusOK {
		t.Fatalf("0=保留现值应 200，得到 HTTP %d: %s", st, body)
	}
	if got := s.QuotaInterval; got != 3*time.Minute {
		t.Fatalf("0 不应改间隔，实际 %v", got)
	}
	if st, body := put(2000); st != http.StatusBadRequest {
		t.Fatalf("越界应 400，得到 HTTP %d: %s", st, body)
	}
	cur, err := store.LoadSettings(store.Paths{AppDir: s.AppDir})
	if err != nil {
		t.Fatal(err)
	}
	if cur.QuotaIntervalMin != 3 {
		t.Fatalf("越界请求不得改落盘值（应保留 3），实际 %d", cur.QuotaIntervalMin)
	}
}

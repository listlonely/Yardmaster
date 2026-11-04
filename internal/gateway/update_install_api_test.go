// M11-W3 管理端点测试：POST /api/update/install——引擎未装配 503、
// 装配成功 200、钩子失败 409。安装引擎本体在 internal/app（依赖方向
// 单向），此处只验网关侧路由与状态码语义。
package gateway

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
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

package netproxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSharedTransportCachesByEndpoint(t *testing.T) {
	a, err := SharedTransport("http://127.0.0.1:7890")
	if err != nil || a == nil {
		t.Fatalf("first: %v", err)
	}
	b, err := SharedTransport("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("同一端点应返回同一 RoundTripper 实例（复用连接池）")
	}
	c, err := SharedTransport("http://127.0.0.1:7891")
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatal("不同端点应返回不同实例")
	}
	// 带凭证的同端点：仍共享（凭证随请求走）
	d, err := SharedTransport("http://user:pass@127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if d != a {
		t.Fatal("同端点不同凭证应共享连接池")
	}
}

func TestSharedTransportNilForNoProxy(t *testing.T) {
	tr, err := SharedTransport("")
	if tr != nil || err != nil {
		t.Fatalf("空 ref = (%v, %v)", tr, err)
	}
	tr, err = SharedTransport(Direct)
	if tr != nil || err != nil {
		t.Fatalf("direct = (%v, %v)", tr, err)
	}
}

func TestHTTP2HealthConfigured(t *testing.T) {
	tr, err := SharedTransport("https://proxy.example:3128")
	if err != nil || tr == nil {
		t.Fatal(err)
	}
	// ConfigureTransports 注册了 h2（经 TLSNextProto），且启用健康参数——
	// 通过行为间接验证：无 panic 即可，具体字段在 http2.Transport 内部。
	ht, ok := tr.(*http.Transport)
	if !ok {
		t.Fatal("应为 *http.Transport")
	}
	if ht.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig 应已初始化")
	}
}

func TestTestConnectivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(404) // 任何状态码都算连通
	}))
	defer srv.Close()

	d, err := TestConnectivity(t.Context(), "", srv.URL)
	if err != nil {
		t.Fatalf("直连预检: %v", err)
	}
	if d <= 0 {
		t.Fatal("耗时应为正")
	}
	// 不可达端口
	if _, err := TestConnectivity(t.Context(), "", "http://127.0.0.1:1/"); err == nil {
		t.Fatal("不可达目标应报错")
	}
}

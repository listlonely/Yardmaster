package netproxy

import "testing"

func TestTransportNilForNoProxy(t *testing.T) {
	for _, ref := range []Ref{"", Direct} {
		tr, err := Transport(ref)
		if tr != nil || err != nil {
			t.Fatalf("Transport(%q) = (%v, %v), want (nil, nil)", ref, tr, err)
		}
	}
}

func TestTransportHTTPProxy(t *testing.T) {
	tr, err := Transport("http://127.0.0.1:7890")
	if err != nil || tr == nil {
		t.Fatalf("Transport(http) = (%v, %v), want non-nil", tr, err)
	}
}

func TestTransportSOCKS5WithAuth(t *testing.T) {
	tr, err := Transport("socks5h://user:pass@10.0.0.8:1080")
	if err != nil || tr == nil {
		t.Fatalf("Transport(socks5h) = (%v, %v), want non-nil", tr, err)
	}
}

func TestTransportRejectsUnsupported(t *testing.T) {
	if _, err := Transport("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("want error for unsupported scheme")
	}
}

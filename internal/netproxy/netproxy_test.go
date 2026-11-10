package netproxy

import (
	"strings"
	"testing"
)

func TestParseAcceptsSupportedSchemes(t *testing.T) {
	cases := []Ref{
		"http://127.0.0.1:7890",
		"https://proxy.corp.example:3128",
		"socks5://127.0.0.1:1080",
		"socks5h://user:pass@10.0.0.8:1080",
	}
	for _, ref := range cases {
		u, err := Parse(ref)
		if err != nil {
			t.Fatalf("Parse(%q): %v", ref, err)
		}
		if u == nil || u.Host == "" {
			t.Fatalf("Parse(%q) = %+v, want host", ref, u)
		}
	}
}

func TestParseEmptyMeansInherit(t *testing.T) {
	u, err := Parse("")
	if u != nil || err != nil {
		t.Fatalf("Parse(\"\") = (%v, %v), want (nil, nil)", u, err)
	}
}

func TestParseDirect(t *testing.T) {
	u, err := Parse(Direct)
	if err != nil || u == nil || u.Scheme != "direct" {
		t.Fatalf("Parse(direct) = (%v, %v), want direct scheme", u, err)
	}
}

func TestParseRejectsUnsupported(t *testing.T) {
	cases := []Ref{"ftp://127.0.0.1:21", "just-a-host:1080", "http://"}
	for _, ref := range cases {
		if _, err := Parse(ref); err == nil {
			t.Fatalf("Parse(%q): want error, got nil", ref)
		}
	}
}

func TestRedactMasksCredentials(t *testing.T) {
	got := Redact("socks5://alice:secret@10.0.0.8:1080")
	want := "socks5://***@10.0.0.8:1080"
	if got != want {
		t.Fatalf("Redact = %q, want %q", got, want)
	}
	if got := Redact("http://127.0.0.1:7890"); got != "http://127.0.0.1:7890" {
		t.Fatalf("Redact without creds changed value: %q", got)
	}
}

func TestSnippetTruncatesAndSanitizes(t *testing.T) {
	long := strings.Repeat("a", 500) + "\n\x01tail"
	s := Snippet([]byte(long))
	if len(s) > 205 {
		t.Fatalf("snippet too long: %d", len(s))
	}
	if strings.ContainsAny(s, "\n\x01") {
		t.Fatalf("控制字符未转义: %q", s)
	}
	if !strings.HasSuffix(s, "…") {
		t.Fatalf("超长应带省略号: %q", s)
	}
	if Snippet([]byte("short")) != "short" {
		t.Fatal("短内容应原样返回")
	}
}

func TestParseEmptyAndDirect(t *testing.T) {
	if u, err := Parse(""); u != nil || err != nil {
		t.Fatalf("Parse(\"\") = (%v, %v)", u, err)
	}
	u, err := Parse("direct")
	if err != nil || u.Scheme != "direct" {
		t.Fatalf("Parse(direct) = (%v, %v)", u, err)
	}
}

package credential

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yardmaster/internal/store"
)

func newTestStore(t *testing.T) (Store, string) {
	t.Helper()
	appDir := filepath.Join(t.TempDir(), ".yardmaster")
	paths := store.Paths{AppDir: appDir}
	return Store{Path: paths.CredentialsPath(), BackupDir: paths.BackupDir()}, appDir
}

func sampleOAuth() OAuth {
	return OAuth{
		AccessToken:  "at-EXAMPLE-0123456789abcdef",
		RefreshToken: "rt-EXAMPLE-0123456789abcdef",
		ExpiresAt:    1893456000,
		Scopes:       []string{"user:inference"},
		Extra:        map[string]string{"account_id": "0000"},
	}
}

func TestCredentialRoundtrip(t *testing.T) {
	s, appDir := newTestStore(t)
	ref, err := NewRef("anthropic", "main")
	if err != nil {
		t.Fatalf("NewRef: %v", err)
	}
	if err := s.Save(ref, sampleOAuth()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ref)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != "at-EXAMPLE-0123456789abcdef" || got.ExpiresAt != 1893456000 || got.Extra["account_id"] != "0000" {
		t.Fatalf("roundtrip 不一致: %+v", got)
	}
	// 第二个键共存（库不是单凭证文件）
	ref2, _ := NewRef("anthropic", "spare")
	if err := s.Save(ref2, OAuth{AccessToken: "at-2", ExpiresAt: 1}); err != nil {
		t.Fatalf("Save(ref2): %v", err)
	}
	if _, err := s.Load(ref); err != nil {
		t.Fatalf("ref 应仍可读: %v", err)
	}
	// 凭证库不得以明文出现在 providers.json（本包文件除外）：
	// Windows 为 DPAPI 密文，直接断言密文中无明文 token。
	if _, err := os.Stat(filepath.Join(appDir, "credentials.bin")); err == nil {
		raw, _ := os.ReadFile(s.Path)
		if strings.Contains(string(raw), "at-EXAMPLE-0123456789abcdef") {
			t.Fatal("Windows 凭证库应为 DPAPI 密文，不得含明文 token")
		}
	}
}

func TestCredentialLoadMissing(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Load("a/b"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("空库 Load 应 ErrNotExist，got %v", err)
	}
	ref, _ := NewRef("p", "a")
	if err := s.Save(ref, sampleOAuth()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := s.Load("p/other"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺键 Load 应 ErrNotExist，got %v", err)
	}
}

func TestCredentialCorruptIsUnreadable(t *testing.T) {
	s, _ := newTestStore(t)
	ref, _ := NewRef("p", "a")
	if err := s.Save(ref, sampleOAuth()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 篡改文件为乱码（模拟损坏/他处密文）：读不懂 = 未知，必须 fail-closed
	bad := append([]byte{0x00, 0xff, 0x01}, []byte("garbage-not-a-credential-store")...)
	if err := os.WriteFile(s.Path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ref); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("损坏库 Load 应 ErrUnreadable，got %v", err)
	}
	// 拒绝在损坏库上覆写（防止静默把可能仍可用的库替换掉）
	if err := s.Save(ref, OAuth{AccessToken: "at-new"}); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("损坏库 Save 应拒绝（ErrUnreadable），got %v", err)
	}
}

func TestCredentialSaveBacksUpPrevious(t *testing.T) {
	s, _ := newTestStore(t)
	ref, _ := NewRef("p", "a")
	if err := s.Save(ref, OAuth{AccessToken: "at-first"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Save(ref, OAuth{AccessToken: "at-second"}); err != nil {
		t.Fatalf("Save#2: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(s.BackupDir, "credentials*"))
	if len(matches) != 1 {
		t.Fatalf("覆写前应留底 1 份快照，got %d: %v", len(matches), matches)
	}
	got, err := s.Load(ref)
	if err != nil || got.AccessToken != "at-second" {
		t.Fatalf("最新值应生效: %+v, %v", got, err)
	}
}

func TestCredentialDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ref, _ := NewRef("p", "a")
	if err := s.Delete(ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("空库 Delete 应 ErrNotExist，got %v", err)
	}
	if err := s.Save(ref, sampleOAuth()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Delete(ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("删除后 Load 应 ErrNotExist，got %v", err)
	}
}

func TestCredentialRejectsEmptyToken(t *testing.T) {
	s, _ := newTestStore(t)
	ref, _ := NewRef("p", "a")
	if err := s.Save(ref, OAuth{}); err == nil {
		t.Fatal("无 access_token 的凭证应拒绝落库")
	}
	if _, err := os.Stat(s.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("拒绝后不应产生库文件: %v", err)
	}
}

func TestNewRefRejectsInjection(t *testing.T) {
	if _, err := NewRef("a/b", "c"); err == nil {
		t.Fatal("provider 含 / 应拒绝")
	}
	if _, err := NewRef("a", ""); err == nil {
		t.Fatal("空账号应拒绝")
	}
	if _, err := NewRef("a\\x", "c"); err == nil {
		t.Fatal("provider 含反斜杠应拒绝")
	}
	if ref, err := NewRef("anthropic", "main"); err != nil || ref != Ref("anthropic/main") {
		t.Fatalf("正常键: %q, %v", ref, err)
	}
}

func TestParserRegistry(t *testing.T) {
	if got := Names(); len(got) != 0 {
		t.Fatalf("W1 不应内置解析器，got %v", got)
	}
	if err := RegisterParser("test-fixture", func(b []byte) (OAuth, error) { return OAuth{AccessToken: string(b)}, nil }); err != nil {
		t.Fatalf("RegisterParser: %v", err)
	}
	if err := RegisterParser("test-fixture", func(b []byte) (OAuth, error) { return OAuth{}, nil }); err == nil {
		t.Fatal("重复注册应报错")
	}
	p, ok := Lookup("test-fixture")
	if !ok {
		t.Fatal("Lookup 应命中")
	}
	o, err := p([]byte("tok"))
	if err != nil || o.AccessToken != "tok" {
		t.Fatalf("parser 执行: %+v, %v", o, err)
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("未注册格式 Lookup 不应命中")
	}
	if err := RegisterParser("", nil); err == nil {
		t.Fatal("空格式注册应拒绝")
	}
}

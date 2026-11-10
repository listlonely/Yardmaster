package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"yardmaster/internal/provider"
)

type conf struct {
	Port int `json:"port"`
}

func TestSaveLoadRoundtrip(t *testing.T) {
	p := Paths{AppDir: filepath.Join(t.TempDir(), ".yardmaster")}
	if err := SaveJSON(p.SettingsPath(), conf{Port: 8317}); err != nil {
		t.Fatalf("SaveJSON: %v", err)
	}
	var out conf
	if err := LoadJSON(p.SettingsPath(), &out); err != nil {
		t.Fatalf("LoadJSON: %v", err)
	}
	if out.Port != 8317 {
		t.Fatalf("port = %d, want 8317", out.Port)
	}
}

// 还原旧代码（LoadJSON 不剥 BOM）即失败：记事本等编辑器写出的 UTF-8
// BOM 文件会被判为「解析失败」，网关按空配置启动。
func TestLoadJSONToleratesBOMAndCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bom.json")
	content := append([]byte("\xef\xbb\xbf"), []byte("{\"port\": 8317}\r\n")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	var out conf
	if err := LoadJSON(path, &out); err != nil {
		t.Fatalf("BOM+CRLF 文件应可读: %v", err)
	}
	if out.Port != 8317 {
		t.Fatalf("port = %d, want 8317", out.Port)
	}
}

func TestLoadCorruptReturnsErrorNotSilentEmpty(t *testing.T) {
	// 开发文档 §4.2：解析失败必须报错，调用方不得当作空配置。
	p := Paths{AppDir: t.TempDir()}
	if err := os.WriteFile(p.SettingsPath(), []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out conf
	if err := LoadJSON(p.SettingsPath(), &out); err == nil {
		t.Fatal("want error for corrupt json, got nil")
	}
}

func TestLoadMissingIsNotExist(t *testing.T) {
	p := Paths{AppDir: t.TempDir()}
	var out conf
	err := LoadJSON(p.SettingsPath(), &out)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

func TestSettingsRoundtrip(t *testing.T) {
	p := Paths{AppDir: t.TempDir()}
	s := Settings{GlobalProxy: "http://127.0.0.1:7890", L1Classifier: "prov/small", L1TimeoutMS: 500}
	if err := SaveSettings(p, s); err != nil {
		t.Fatal(err)
	}
	out, err := LoadSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, s) {
		t.Fatalf("roundtrip = %+v, want %+v", out, s)
	}
	// 缺文件返回 os.ErrNotExist（调用方按零值处理）
	empty, err := LoadSettings(Paths{AppDir: t.TempDir()})
	if !errors.Is(err, os.ErrNotExist) || !reflect.DeepEqual(empty, Settings{}) {
		t.Fatalf("缺文件应返回 ErrNotExist+零值: %+v %v", empty, err)
	}
}

func TestProvidersFileRoundtrip(t *testing.T) {
	p := Paths{AppDir: t.TempDir()}
	in := ProvidersFile{
		Providers: []provider.Provider{{
			ID: "sf", Protocol: provider.ProtocolChat, BaseURL: "https://x",
			Accounts: []provider.Account{{
				Name: "main", APIKey: "sk", Status: provider.AccountAvailable,
				UsedTokens: 120, QuotaTokensDay: 1000, UsageDay: "2026-10-29",
			}},
		}},
		Groups: []provider.Group{{ID: "g", Strategy: provider.StrategyRotate, Members: []string{"sf/m"}}},
	}
	if err := SaveProvidersFile(p.ProvidersPath(), in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadProvidersFile(p.ProvidersPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Providers) != 1 || len(out.Groups) != 1 ||
		out.Providers[0].Accounts[0].UsedTokens != 120 ||
		out.Providers[0].Accounts[0].UsageDay != "2026-10-29" ||
		out.Groups[0].Members[0] != "sf/m" {
		t.Fatalf("roundtrip = %+v", out)
	}
	if _, err := LoadProvidersFile(filepath.Join(t.TempDir(), "none.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺文件应返回 ErrNotExist, got %v", err)
	}
}

func TestPathsLayout(t *testing.T) {
	p := Paths{AppDir: "root"}
	for _, c := range []struct{ got, name string }{
		{p.SettingsPath(), "settings.json"},
		{p.ProvidersPath(), "providers.json"},
		{p.RulesPath(), "rules.json"},
		{p.KeysPath(), "keys.json"},
		{p.IntentsPath(), "intents.json"},
		{p.AppliedPath(), "applied.json"},
	} {
		if want := filepath.Join("root", c.name); c.got != want {
			t.Fatalf("path = %q, want %q", c.got, want)
		}
	}
	if p.TraceDir() != filepath.Join("root", "trace") || p.StashDir() != filepath.Join("root", "stash") {
		t.Fatal("目录布局不符")
	}
}

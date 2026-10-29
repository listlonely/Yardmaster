package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type conf struct {
	Port int `json:"port"`
}

func TestSaveLoadRoundtrip(t *testing.T) {
	p := Paths{AppDir: filepath.Join(t.TempDir(), ".agenthub")}
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
	if out != s {
		t.Fatalf("roundtrip = %+v, want %+v", out, s)
	}
	// 缺文件返回 os.ErrNotExist（调用方按零值处理）
	empty, err := LoadSettings(Paths{AppDir: t.TempDir()})
	if !errors.Is(err, os.ErrNotExist) || empty != (Settings{}) {
		t.Fatalf("缺文件应返回 ErrNotExist+零值: %+v %v", empty, err)
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

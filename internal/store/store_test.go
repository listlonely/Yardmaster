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

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupConfigCreatesSnapshotAndTrims(t *testing.T) {
	appDir := t.TempDir()
	path := filepath.Join(appDir, "settings.json")
	backupDir := filepath.Join(appDir, "backup")
	base := BaseName(path)

	if err := os.WriteFile(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 同一秒内多次快照文件名相同（时间戳精度），用不同内容验证保留语义时
	// 以文件数量为准。keep=3：写 5 次后应只剩 3 份。
	for i := 0; i < 5; i++ {
		content := []byte(`{"v":` + string(rune('1'+i)) + `}`)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := BackupConfig(path, backupDir, base, 3); err != nil {
			t.Fatalf("BackupConfig #%d: %v", i, err)
		}
	}
	files, err := filepath.Glob(filepath.Join(backupDir, base+"-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("备份栈应保留 3 份，实际 %d 份: %v", len(files), files)
	}
	for _, f := range files {
		if got := filepath.Base(f); !strings.HasPrefix(got, base+"-") || !strings.HasSuffix(got, ".json") {
			t.Fatalf("快照命名不符约定: %s", got)
		}
	}
}

func TestBackupConfigMissingSourceIsNoOp(t *testing.T) {
	appDir := t.TempDir()
	backupDir := filepath.Join(appDir, "backup")
	if err := BackupConfig(filepath.Join(appDir, "absent.json"), backupDir, "absent", 10); err != nil {
		t.Fatalf("源文件不存在应为空操作，得到: %v", err)
	}
	if entries, _ := os.ReadDir(backupDir); len(entries) != 0 {
		t.Fatalf("不应产生快照文件: %v", entries)
	}
}

func TestBackupConfigPreservesContent(t *testing.T) {
	appDir := t.TempDir()
	path := filepath.Join(appDir, "rules.json")
	backupDir := filepath.Join(appDir, "backup")
	original := []byte("{\"rules\":[{\"id\":\"r1\"}]}\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := BackupConfig(path, backupDir, BaseName(path), 10); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(backupDir, "rules-*.json"))
	if len(matches) != 1 {
		t.Fatalf("应恰好 1 份快照，实际 %d", len(matches))
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("快照内容与源不一致:\n got %q\nwant %q", got, original)
	}
}

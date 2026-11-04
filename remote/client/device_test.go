package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDeviceIDStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first := newDeviceStore(dir, "1.0.0", testLogger()).Ensure()
	if !strings.HasPrefix(first.ID, "pc-") || len(first.ID) != len("pc-")+12 {
		t.Fatalf("device id 形态不符：%q", first.ID)
	}
	second := newDeviceStore(dir, "1.0.0", testLogger()).Ensure()
	if second.ID != first.ID {
		t.Fatalf("重启后 device id 应恒定：%q vs %q", first.ID, second.ID)
	}
}

func TestDeviceTokenPersistence(t *testing.T) {
	dir := t.TempDir()
	store := newDeviceStore(dir, "1.0.0", testLogger())
	store.Ensure()
	if err := store.SetToken("tok-1"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	reopened := newDeviceStore(dir, "1.0.0", testLogger())
	if got := reopened.Token(); got != "tok-1" {
		t.Fatalf("token 未持久化：%q", got)
	}
	if err := reopened.ClearToken(); err != nil {
		t.Fatalf("ClearToken: %v", err)
	}
	if got := newDeviceStore(dir, "1.0.0", testLogger()).Token(); got != "" {
		t.Fatalf("token 未清除：%q", got)
	}
}

func TestDeviceCorruptFileRegenerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, deviceFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("写入损坏文件: %v", err)
	}
	desc := newDeviceStore(dir, "1.0.0", testLogger()).Ensure()
	if !strings.HasPrefix(desc.ID, "pc-") {
		t.Fatalf("损坏文件应重新生成 device id：%q", desc.ID)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), desc.ID) {
		t.Fatalf("重新生成后应落盘：%v %s", err, raw)
	}
}

func TestDeviceMissingFileCreatesIt(t *testing.T) {
	dir := t.TempDir()
	desc := newDeviceStore(filepath.Join(dir, "nested"), "1.0.0", testLogger()).Ensure()
	if _, err := os.Stat(filepath.Join(dir, "nested", deviceFileName)); err != nil {
		t.Fatalf("缺失目录应自动创建并落盘：%v", err)
	}
	if desc.Name == "" || desc.Platform == "" || desc.Version != "1.0.0" {
		t.Fatalf("设备信息不完整：%#v", desc)
	}
}

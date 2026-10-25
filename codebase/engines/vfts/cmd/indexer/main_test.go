package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCoalesceChangesLatestWins(t *testing.T) {
	items := []queueItem{
		{ID: "1", Path: "/wd/a.txt", Operation: "create"},
		{ID: "2", Path: "/wd/a.txt", Operation: "write"},
	}
	got := coalesceChanges(items)
	if len(got) != 1 {
		t.Fatalf("同路径应合并为 1 条，实际 %d: %+v", len(got), got)
	}
	if got[0].Path != "/wd/a.txt" || got[0].Op != opWrite {
		t.Fatalf("应保留最新操作 write，实际 %+v", got[0])
	}
}

func TestCoalesceChangesCreateThenRemove(t *testing.T) {
	got := coalesceChanges([]queueItem{
		{ID: "1", Path: "/wd/a.txt", Operation: "create"},
		{ID: "2", Path: "/wd/a.txt", Operation: "remove"},
	})
	if len(got) != 1 || got[0].Op != opRemove {
		t.Fatalf("create 后 remove 应为 remove，实际 %+v", got)
	}
}

func TestCoalesceChangesRemoveThenCreate(t *testing.T) {
	got := coalesceChanges([]queueItem{
		{ID: "1", Path: "/wd/a.txt", Operation: "remove"},
		{ID: "2", Path: "/wd/a.txt", Operation: "create"},
	})
	if len(got) != 1 || got[0].Op != opCreate {
		t.Fatalf("remove 后 create 应为 create，实际 %+v", got)
	}
}

func TestCoalesceChangesRename(t *testing.T) {
	got := coalesceChanges([]queueItem{
		{ID: "1", Path: "/wd/old.txt", Operation: "rename"},
		{ID: "2", Path: "/wd/new.txt", Operation: "create"},
	})
	if len(got) != 2 {
		t.Fatalf("rename 应拆成旧 remove + 新 create，实际 %+v", got)
	}
	byPath := map[string]string{}
	for _, c := range got {
		byPath[c.Path] = c.Op
	}
	if byPath["/wd/old.txt"] != opRemove {
		t.Fatalf("旧路径应为 remove，实际 %+v", got)
	}
	if byPath["/wd/new.txt"] != opCreate {
		t.Fatalf("新路径应为 create，实际 %+v", got)
	}
}

func TestManifestRoundTripAndPlanIndex(t *testing.T) {
	dir := t.TempDir()
	path := manifestPath(dir)

	m := &manifest{
		Version: manifestVersion,
		Files: map[string]manifestEntry{
			"/wd/a.txt":   {DocIDs: []string{"1", "2"}},
			"/wd/gone.md": {DocIDs: []string{"3"}},
		},
	}
	if err := saveManifest(path, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil {
		t.Fatal("manifest 读回为 nil")
	}
	if len(got.Files["/wd/a.txt"].DocIDs) != 2 || got.Files["/wd/a.txt"].DocIDs[1] != "2" {
		t.Fatalf("doc_ids 读回不一致: %+v", got.Files["/wd/a.txt"])
	}

	files, removes := planIndex(got, []change{
		{Path: "/wd/a.txt", Op: opWrite},
		{Path: "/wd/gone.md", Op: opRemove},
		{Path: "/wd/b.txt", Op: opCreate},
	})
	if len(files) != 2 {
		t.Fatalf("files 应为 2 条，实际 %+v", files)
	}
	byPath := map[string]indexFile{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	if got := byPath["/wd/a.txt"]; len(got.DocIDs) != 2 || got.DocIDs[0] != "1" {
		t.Fatalf("更新文件应携带旧 doc_ids，实际 %+v", got)
	}
	if got := byPath["/wd/b.txt"]; len(got.DocIDs) != 0 {
		t.Fatalf("新文件不应携带 doc_ids，实际 %+v", got)
	}
	if len(removes) != 1 || removes[0].Path != "/wd/gone.md" || removes[0].DocIDs[0] != "3" {
		t.Fatalf("remove 规划不符: %+v", removes)
	}
}

func TestLoadManifestMissing(t *testing.T) {
	m, err := loadManifest(filepath.Join(t.TempDir(), "manifest-indexer.json"))
	if err != nil || m != nil {
		t.Fatalf("manifest 不存在应为 (nil,nil)，实际 m=%+v err=%v", m, err)
	}
}

func TestSaveManifestAtomic(t *testing.T) {
	dir := t.TempDir()
	path := manifestPath(dir)
	m := &manifest{Version: manifestVersion, Files: map[string]manifestEntry{
		"/wd/a.txt": {DocIDs: []string{"1"}},
	}}
	if err := saveManifest(path, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatalf("临时文件未清理: %s", path+".tmp")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("manifest 未落盘: %v", err)
	}

	// 覆盖写同样不残留临时文件
	m.Files["/wd/b.txt"] = manifestEntry{DocIDs: []string{"2"}}
	if err := saveManifest(path, m); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatalf("覆盖后临时文件未清理: %s", path+".tmp")
	}
	got, err := loadManifest(path)
	if err != nil || got == nil || len(got.Files) != 2 {
		t.Fatalf("覆盖后读回不一致: %+v err=%v", got, err)
	}
}

func TestAbsKey(t *testing.T) {
	wd := t.TempDir()
	if got, want := absKey(wd, "sub/a.txt"), filepath.ToSlash(filepath.Join(wd, "sub", "a.txt")); got != want {
		t.Fatalf("相对路径应拼为绝对路径，实际 %q want %q", got, want)
	}
	abs := filepath.ToSlash(filepath.Join(wd, "b.txt"))
	if got := absKey(wd, abs); got != abs {
		t.Fatalf("绝对路径应原样保留，实际 %q want %q", got, abs)
	}
}

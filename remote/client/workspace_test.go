package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceIDStableAndShort(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Proj")
	a := workspaceID(dir)
	b := workspaceID(strings.ToUpper(dir) + string(filepath.Separator))
	if a != b {
		t.Fatalf("同一目录大小写 / 尾分隔符差异应得到同一 id：%s vs %s", a, b)
	}
	if len(a) != 12 {
		t.Fatalf("workspace id 应为 12 位 hex，得到 %q", a)
	}
	if strings.ContainsFunc(a, func(r rune) bool {
		return !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f')
	}) {
		t.Fatalf("workspace id 应只含 hex：%q", a)
	}
}

func TestWorkspaceStoreTopLevelOnlyAndResolve(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	s := newWorkspaceStore()
	if !s.Register("i-top", dir, filepath.Join(t.TempDir(), "data"), "") {
		t.Fatal("顶层实例登记应改变工作区集合")
	}
	if s.Register("i-child", dir, filepath.Join(t.TempDir(), "data"), "i-top") {
		t.Fatal("子实例不应进入工作区列表")
	}
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("顶层工作区应只有 1 个，得到 %d", len(list))
	}
	ws := list[0]
	if ws.InstanceID != "i-top" || ws.Path != dir || ws.Name != "proj" || ws.ID != workspaceID(dir) {
		t.Fatalf("工作区字段不符：%+v", ws)
	}

	if got, ok := s.Resolve(strings.ToUpper(dir), ""); !ok || got.InstanceID != "i-top" {
		t.Fatalf("按 work_dir 应命中顶层工作区：%+v ok=%v", got, ok)
	}
	if got, ok := s.Resolve("", "i-child"); !ok || got.InstanceID != "i-top" {
		t.Fatalf("子实例应回溯到父工作区：%+v ok=%v", got, ok)
	}
	if _, ok := s.Resolve("", "missing"); ok {
		t.Fatal("未知实例不应命中工作区")
	}
	if s.Remove("i-child") {
		t.Fatal("移除子实例不应改变工作区集合")
	}
	if !s.Remove("i-top") {
		t.Fatal("移除顶层实例应改变工作区集合")
	}
	if len(s.List()) != 0 {
		t.Fatal("全部移除后列表应为空")
	}
}

func TestWorkspacesFrameAlwaysCarriesArray(t *testing.T) {
	f := workspacesFrame(nil)
	list, ok := f["workspaces"].([]workspace)
	if !ok || list == nil || len(list) != 0 {
		t.Fatalf("空工作区应序列化为 []：%#v", f["workspaces"])
	}
}

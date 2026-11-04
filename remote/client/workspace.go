package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// workspace 是上报给 Remote Server 的工作区（= 一个顶层实例 / 项目窗口）。
type workspace struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	InstanceID string `json:"instance_id"`
}

// instanceInfo 是宿主导出的实例视图（仅保留工作区所需字段）。
type instanceInfo struct {
	WorkDir string
	DataDir string
	Parent  string
}

// workspaceStore 维护 `instance_id → 实例信息`，并派生出顶层工作区列表。
type workspaceStore struct {
	mu        sync.Mutex
	instances map[string]instanceInfo
}

func newWorkspaceStore() *workspaceStore {
	return &workspaceStore{instances: map[string]instanceInfo{}}
}

// Register 登记一个实例；返回顶层工作区集合是否发生变化。
func (s *workspaceStore) Register(instanceID, workDir, dataDir, parent string) bool {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.signatureLocked()
	s.instances[instanceID] = instanceInfo{
		WorkDir: strings.TrimSpace(workDir),
		DataDir: strings.TrimSpace(dataDir),
		Parent:  strings.TrimSpace(parent),
	}
	return before != s.signatureLocked()
}

// Remove 注销一个实例；返回顶层工作区集合是否发生变化。
func (s *workspaceStore) Remove(instanceID string) bool {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[instanceID]; !ok {
		return false
	}
	before := s.signatureLocked()
	delete(s.instances, instanceID)
	return before != s.signatureLocked()
}

// List 返回全部顶层工作区（按路径 / 实例 id 稳定排序，按 workspace id 去重）。
func (s *workspaceStore) List() []workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// Resolve 按 `work_dir`（优先）或 `instance_id`（含子实例回溯父实例）定位工作区。
func (s *workspaceStore) Resolve(workDir, instanceID string) (workspace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wd := normalizeWorkDir(workDir); wd != "" {
		if ws, ok := s.matchWorkDirLocked(wd); ok {
			return ws, true
		}
	}
	cur := strings.TrimSpace(instanceID)
	for i := 0; i < 8 && cur != ""; i++ {
		if ws, ok := s.matchInstanceLocked(cur); ok {
			return ws, true
		}
		info, ok := s.instances[cur]
		if !ok {
			break
		}
		if info.Parent == "" {
			break
		}
		cur = info.Parent
	}
	return workspace{}, false
}

func (s *workspaceStore) listLocked() []workspace {
	out := make([]workspace, 0, len(s.instances))
	seen := map[string]bool{}
	for id, info := range s.instances {
		if info.Parent != "" || info.WorkDir == "" {
			continue
		}
		ws := workspace{
			ID:         workspaceID(info.WorkDir),
			Name:       workspaceName(info.WorkDir),
			Path:       info.WorkDir,
			InstanceID: id,
		}
		if seen[ws.ID] {
			continue
		}
		seen[ws.ID] = true
		out = append(out, ws)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].InstanceID < out[j].InstanceID
	})
	return out
}

func (s *workspaceStore) signatureLocked() string {
	list := s.listLocked()
	var b strings.Builder
	for _, w := range list {
		b.WriteString(w.ID)
		b.WriteByte('\x00')
		b.WriteString(w.InstanceID)
		b.WriteByte('\x01')
	}
	return b.String()
}

func (s *workspaceStore) matchWorkDirLocked(normalized string) (workspace, bool) {
	for _, ws := range s.listLocked() {
		if normalizeWorkDir(ws.Path) == normalized {
			return ws, true
		}
	}
	return workspace{}, false
}

func (s *workspaceStore) matchInstanceLocked(instanceID string) (workspace, bool) {
	for _, ws := range s.listLocked() {
		if ws.InstanceID == instanceID {
			return ws, true
		}
	}
	return workspace{}, false
}

// workspaceID = sha256(小写归一化 work_dir) 前 12 位 hex。
func workspaceID(workDir string) string {
	sum := sha256.Sum256([]byte(normalizeWorkDir(workDir)))
	return hex.EncodeToString(sum[:])[:12]
}

// normalizeWorkDir 归一工作目录（斜杠统一、Clean、去尾分隔符、小写）。
func normalizeWorkDir(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return strings.ToLower(p)
}

// workspaceName 取目录名（取不到 → 原路径）。
func workspaceName(workDir string) string {
	base := filepath.Base(filepath.Clean(workDir))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return workDir
	}
	return base
}

package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 索引内一律使用 '/' 分隔、相对 workdir 的路径（store 可移植）；对外返回时拼回绝对路径。

func sep() string { return "/" }

func dirOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}

// relOf 把绝对路径转为相对 base 的 '/' 规范路径。
func relOf(base, path string) string {
	r, err := filepath.Rel(base, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(r)
}

// cleanPath 规整 './'、'../' 段（基于字符串，不访问磁盘）。
func cleanPath(p string) string {
	segs := strings.Split(p, "/")
	var out []string
	for _, s := range segs {
		switch s {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
}

func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "/" + b
}

var (
	moduleCacheMu sync.Mutex // 包级缓存（多 workspace/并发解析共享）→ 读写加锁，避免 data race
	moduleCache   struct {
		dir string
		mod string
	}
)

// moduleOf 读 root/go.mod 的 module 行（缓存；并发安全）。
func moduleOf(rootDir string) string {
	moduleCacheMu.Lock()
	defer moduleCacheMu.Unlock()
	if moduleCache.dir == rootDir {
		return moduleCache.mod
	}
	mod := ""
	gm := filepath.Join(rootDir, "go.mod")
	if b, err := os.ReadFile(gm); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "module ") {
				mod = strings.TrimSpace(strings.TrimPrefix(line, "module "))
				break
			}
		}
	}
	moduleCache.dir = rootDir
	moduleCache.mod = mod
	return mod
}

// probeRelative JS/TS 相对导入解析（rel 空间：indexed 键为相对路径）。
func probeRelative(dir, imp string, indexed map[string]bool, exts []string) string {
	cand := cleanPath(joinPath(dir, imp))
	if strings.HasPrefix(cand, "..") {
		return ""
	}
	if indexed[cand] {
		return cand
	}
	for _, e := range exts {
		if indexed[cand+e] {
			return cand + e
		}
	}
	return ""
}

// resolveImport 把一条 import 原语解析为 rel 空间下已索引文件（解析不到返回 ""）。
// 规则按语言近似；解析不到的边不参与环检测（get_dependency_graph 仍给原始列表）。
func resolveImport(lang, importerFile, imp, rootDir string, indexed map[string]bool) string {
	imp = strings.TrimSpace(imp)
	if imp == "" || imp == "_" || imp == "." {
		return ""
	}
	dir := dirOf(importerFile)
	switch lang {
	case "javascript", "typescript", "tsx":
		if strings.HasPrefix(imp, ".") {
			return probeRelative(dir, imp, indexed,
				[]string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", "/index.js", "/index.ts", "/index.tsx", "/index.jsx", "/index.mjs"})
		}
	case "go":
		mod := moduleOf(rootDir)
		if mod != "" && strings.HasPrefix(imp, mod) {
			rel := strings.TrimPrefix(imp, mod)
			rel = strings.TrimPrefix(rel, "/")
			if rel == "" {
				return ""
			}
			if indexed[rel] {
				return rel
			}
			for f := range indexed {
				if dirOf(f) == rel {
					return f
				}
			}
		}
	case "python":
		if !strings.HasPrefix(imp, ".") {
			p := strings.ReplaceAll(imp, ".", "/")
			if indexed[p] {
				return p
			}
			if indexed[p+".py"] {
				return p + ".py"
			}
			for f := range indexed {
				if strings.HasPrefix(f, p+"/") {
					return f
				}
			}
		}
	case "java":
		if strings.HasPrefix(imp, "java.") || strings.HasPrefix(imp, "javax.") {
			return ""
		}
		p := strings.ReplaceAll(imp, ".", "/")
		if indexed[p+".java"] {
			return p + ".java"
		}
		for f := range indexed {
			if strings.HasPrefix(f, p+"/") {
				return f
			}
		}
	case "rust":
		seg := strings.TrimPrefix(imp, "crate::")
		if i := strings.Index(seg, "::"); i >= 0 {
			head := seg[:i]
			if head == "super" || head == "self" || head == "crate" {
				seg = seg[i+2:]
			}
		}
		base := strings.TrimSuffix(seg, ".rs")
		for _, rootCand := range []string{dir, "src"} {
			p := joinPath(rootCand, base)
			if indexed[p] {
				return p
			}
			if indexed[p+".rs"] {
				return p + ".rs"
			}
			if indexed[joinPath(p, "mod.rs")] {
				return joinPath(p, "mod.rs")
			}
		}
	}
	return ""
}

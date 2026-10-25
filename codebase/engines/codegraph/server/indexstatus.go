package server

// 索引状态文件（<workdir>/.chonkpilot/codegraph/status.json）。
//
// 由 resident indexer 写入，MCP 查询侧只读：查询门据此判断「是否还有待索引项」。
// 文件不存在 = 未启用该机制，查询门保持既有 EnsureReady 行为（完全向后兼容）。
//
//	{"state":"ready|indexing|error","pending":0,"message":"","updated_at":"2026-10-19T00:00:00Z"}

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// IndexStatus 是 status.json 的内容（跨进程可读）。
type IndexStatus struct {
	State     string `json:"state"`
	Pending   int    `json:"pending"`
	Message   string `json:"message"`
	UpdatedAt string `json:"updated_at"`
}

// IndexStatusPath 返回指定 workdir 的索引状态文件路径。
func IndexStatusPath(workDir string) string {
	return filepath.Join(filepath.FromSlash(StoreDir(workDir)), "status.json")
}

// ReadIndexStatus 读取索引状态：文件不存在 → (nil, nil)；解析失败 → error。
func ReadIndexStatus(workDir string) (*IndexStatus, error) {
	b, err := os.ReadFile(IndexStatusPath(workDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var st IndexStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("status.json 解析失败: %w", err)
	}
	return &st, nil
}

// WriteIndexStatus 原子写入索引状态（临时文件 + Rename）；供 resident indexer 使用。
func WriteIndexStatus(workDir string, st IndexStatus) error {
	path := IndexStatusPath(workDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, b)
}

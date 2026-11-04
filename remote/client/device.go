package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// deviceFileName 是插件数据目录下的设备标识文件（device_id 恒定，token 随登录刷新）。
const deviceFileName = "device.json"

// deviceFile 是 `device.json` 的磁盘形态。
type deviceFile struct {
	DeviceID string `json:"device_id"`
	Token    string `json:"token,omitempty"`
}

// deviceDesc 是 hello 帧上报的设备信息。
type deviceDesc struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
}

// deviceStore 持久化 device_id / token（进程内缓存，写盘原子替换）。
type deviceStore struct {
	path string
	ver  string
	log  *slog.Logger

	mu   sync.Mutex
	cur  deviceFile
	init bool
}

func newDeviceStore(dir, version string, log *slog.Logger) *deviceStore {
	return &deviceStore{path: filepath.Join(dir, deviceFileName), ver: version, log: log}
}

// Ensure 读取（首次生成）设备标识并返回（幂等）。
func (d *deviceStore) Ensure() deviceDesc {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.init {
		d.loadLocked()
	}
	return d.descLocked()
}

// Token 返回当前保存的 token（未初始化时先读盘）。
func (d *deviceStore) Token() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.init {
		d.loadLocked()
	}
	return d.cur.Token
}

// SetToken 保存 token（落盘）。
func (d *deviceStore) SetToken(token string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.init {
		d.loadLocked()
	}
	if d.cur.Token == token {
		return nil
	}
	d.cur.Token = token
	return d.persistLocked()
}

// ClearToken 清空 token（落盘）。
func (d *deviceStore) ClearToken() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.init {
		d.loadLocked()
	}
	if d.cur.Token == "" {
		return nil
	}
	d.cur.Token = ""
	return d.persistLocked()
}

// loadLocked 读盘；缺失 / 损坏 → 生成新 device_id 并立即持久化（不因磁盘问题阻塞启动）。
func (d *deviceStore) loadLocked() {
	d.init = true
	d.cur = deviceFile{}
	raw, err := os.ReadFile(d.path)
	switch {
	case err == nil:
		var f deviceFile
		if jerr := json.Unmarshal(raw, &f); jerr != nil || strings.TrimSpace(f.DeviceID) == "" {
			if d.log != nil {
				d.log.Warn("remote: device.json 损坏，重新生成设备标识", "err", jerr)
			}
			f = deviceFile{}
		}
		d.cur = f
	case errors.Is(err, os.ErrNotExist):
		// 首次运行 → 生成
	default:
		if d.log != nil {
			d.log.Warn("remote: 读取 device.json 失败（重新生成设备标识）", "err", err)
		}
	}
	if strings.TrimSpace(d.cur.DeviceID) == "" {
		d.cur.DeviceID = "pc-" + randomHex(6)
		if perr := d.persistLocked(); perr != nil && d.log != nil {
			d.log.Warn("remote: 保存 device.json 失败", "err", perr)
		}
	}
}

// persistLocked 原子写盘（临时文件 + rename）。
func (d *deviceStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(d.cur, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, d.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (d *deviceStore) descLocked() deviceDesc {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		name = "unknown"
	}
	return deviceDesc{
		ID:       d.cur.DeviceID,
		Name:     name,
		Platform: runtime.GOOS,
		Version:  d.ver,
	}
}

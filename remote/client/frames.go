package main

import (
	"encoding/json"
	"strings"
)

// 出站 / 入站帧类型（26-remote §4.2）。
const (
	frameHello      = "hello"
	frameHelloOK    = "hello-ok"
	frameWorkspaces = "workspaces"
	frameCall       = "call"
	frameResult     = "result"
	framePing       = "ping"
	framePong       = "pong"
)

// wsFrame 是 WSS 上的 JSON 帧（收发共用；未用字段 omitempty）。
type wsFrame struct {
	Type string `json:"type,omitempty"`

	// hello / hello-ok
	Username      string      `json:"username,omitempty"`
	Password      string      `json:"password,omitempty"`
	Token         string      `json:"token,omitempty"`
	Device        *deviceDesc `json:"device,omitempty"`
	ClientVersion string      `json:"client_version,omitempty"`
	Protocol      string      `json:"protocol,omitempty"`
	Workspaces    []workspace `json:"workspaces,omitempty"`
	DeviceID      string      `json:"device_id,omitempty"`
	ServerTime    int64       `json:"server_time,omitempty"`

	// call / result
	ID          string          `json:"id,omitempty"`
	WorkspaceID string          `json:"workspace_id,omitempty"`
	Method      string          `json:"method,omitempty"`
	Args        json.RawMessage `json:"args,omitempty"`
	OK          *bool           `json:"ok,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`

	// event
	Data json.RawMessage `json:"data,omitempty"`
}

// helloFrame 组装首连 / 重连的 hello 帧。
//
// 首连（无 token）带 username + password；重连带 token，并附 username 作提示。
func helloFrame(cfg settings, dev deviceDesc, token string, workspaces []workspace) map[string]any {
	token = strings.TrimSpace(token)
	m := map[string]any{
		"type":           frameHello,
		"device":         dev,
		"client_version": dev.Version,
		"protocol":       protocolVersion,
		"workspaces":     workspaceList(workspaces),
	}
	if token != "" {
		m["token"] = token
		putIf(m, "username", cfg.Username)
		return m
	}
	putIf(m, "username", cfg.Username)
	putIf(m, "password", cfg.Password)
	return m
}

// workspacesFrame 是全量覆盖帧（即使为空也必须显式带 `workspaces: []`）。
func workspacesFrame(workspaces []workspace) map[string]any {
	return map[string]any{"type": frameWorkspaces, "workspaces": workspaceList(workspaces)}
}

// resultFrame 组装成功应答。
func resultFrame(id string, result any) map[string]any {
	return map[string]any{"type": frameResult, "id": id, "ok": true, "result": result}
}

// errorResultFrame 组装失败应答。
func errorResultFrame(id, msg string) map[string]any {
	return map[string]any{"type": frameResult, "id": id, "ok": false, "error": msg}
}

// eventFrame 组装产品事件帧：载荷形状 = `{workspace_id, type, data}`（26-remote §4.2），
// 其中 `type` = **产品事件类型**（`message.delta` 等），Server 只按 workspace_id 路由、不解析。
func eventFrame(workspaceID, typ string, data map[string]any) map[string]any {
	return map[string]any{
		"type":         typ,
		"workspace_id": workspaceID,
		"data":         data,
	}
}

func pingFrame() map[string]any { return map[string]any{"type": framePing} }
func pongFrame() map[string]any { return map[string]any{"type": framePong} }

// workspaceList 把 nil 归一为空数组（JSON 里必须是 `[]` 而非 `null`）。
func workspaceList(in []workspace) []workspace {
	if in == nil {
		return []workspace{}
	}
	return in
}

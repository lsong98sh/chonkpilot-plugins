// Package pluginclient 是插件开发者侧的 SDK：对「帧传输（UDS/TCP）」与「HTTP 传输」
// 提供同一套 Client 接口。
//
// 落点 = core 模块（只依赖 transport 与标准库）：任何插件（含 vfts/codegraph indexer）
// 都能直接 import，不引入 llm/data 的依赖图。
//
// 用法：
//
//	cli, err := pluginclient.Dial(ctx, pluginclient.Options{
//	    HTTPBase: "http://127.0.0.1:5671",
//	    Token:    os.Getenv("CHONK_PLUGIN_HTTP_TOKEN"),
//	    PluginID: "com.example.bot",
//	})
//	defer cli.Close()
//	raw, err := cli.Call(ctx, "plugin/v1/sys.info", nil)
//
// 约定：插件 `id` 必填；`Addr` 与 `HTTPBase` 二选一（都填时优先 HTTP）。
package pluginclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// DefaultTimeout 是单次调用缺省超时。
const DefaultTimeout = 15 * time.Second

// Options 是 Dial 参数。
type Options struct {
	// Addr 是帧传输地址：UDS 路径或 `tcp://host:port`（PRODSIM-1：能力外溢统一走 UDS，
	// 已移除 HTTP 前门传输）。
	Addr string
	// PluginID 是插件自报 id（必填）。
	PluginID string
	// Name / Version / Features 随 plugin.hello 上报（可选）。
	Name     string
	Version  string
	Features map[string]any
	// Timeout 是单次调用超时；<=0 用 DefaultTimeout。
	Timeout time.Duration
}

// Subscription 是一条订阅声明（与 plugin.subscribe 的 payload 对齐）。
type Subscription struct {
	ID      string         `json:"id,omitempty"`
	Pattern string         `json:"pattern"`
	Mode    string         `json:"mode,omitempty"`
	Filter  map[string]any `json:"filter,omitempty"`
}

// Event 是从服务端收到的一条事件 / 控制帧。
type Event struct {
	Kind    string // "evt" | "ctl"
	Name    string
	Payload json.RawMessage
}

// Client 是插件接入的统一接口。
type Client interface {
	// PluginID 返回服务端确认的插件 id。
	PluginID() string
	// InstanceID 返回服务端分配的插件实例 id。
	InstanceID() string
	// Call 调用一个方法，返回结果 JSON。
	Call(ctx context.Context, method string, args any) (json.RawMessage, error)
	// Subscribe 订阅事件（plugin.subscribe）。
	Subscribe(ctx context.Context, subs []Subscription) error
	// Events 返回事件通道；Close 后通道关闭。
	Events() <-chan Event
	// Close 关闭连接（幂等）。
	Close() error
}

// Dial 建立插件连接（UDS / TCP 帧传输）。
func Dial(ctx context.Context, opts Options) (Client, error) {
	opts.PluginID = strings.TrimSpace(opts.PluginID)
	if opts.PluginID == "" {
		return nil, errors.New("pluginclient: Options.PluginID 不能为空")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if strings.TrimSpace(opts.Addr) == "" {
		return nil, errors.New("pluginclient: Options.Addr 不能为空")
	}
	return dialFrame(ctx, opts)
}

func helloPayload(o Options) map[string]any {
	m := map[string]any{"id": o.PluginID}
	if o.Name != "" {
		m["name"] = o.Name
	}
	if o.Version != "" {
		m["version"] = o.Version
	}
	if o.Features != nil {
		m["features"] = o.Features
	}
	return m
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

func errorText(errs []string) string {
	if len(errs) == 0 {
		return "调用失败（无错误详情）"
	}
	return strings.Join(errs, "; ")
}

func nullRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func subscribeArgs(subs []Subscription) map[string]any {
	return map[string]any{"subscriptions": subs}
}

package pluginclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	transport "github.com/lsong98sh/chonkpilot-plugin-framework/protocol"
)

// frameClient 是帧传输（UDS / TCP 行分隔 JSON）实现。
type frameClient struct {
	opts       Options
	conn       transport.Conn
	pluginID   string
	instanceID string

	events    chan Event
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

func dialFrame(ctx context.Context, opts Options) (Client, error) {
	to := transport.Options{Backend: transport.BackendUDS, Addr: opts.Addr}
	if strings.HasPrefix(opts.Addr, "tcp://") {
		to.Backend = transport.BackendTCP
		to.Addr = strings.TrimPrefix(opts.Addr, "tcp://")
	}
	conn, err := transport.Dial(ctx, to)
	if err != nil {
		return nil, fmt.Errorf("pluginclient: 连接 %s: %w", opts.Addr, err)
	}
	res, err := conn.Call(ctx, "plugin.hello", helloPayload(opts))
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("pluginclient: plugin.hello: %w", err)
	}
	if !res.OK {
		_ = conn.Close()
		return nil, fmt.Errorf("pluginclient: plugin.hello 失败: %s", errorText(res.Errors))
	}
	pid, iid := helloIDs(res.Value)
	if pid == "" {
		pid = opts.PluginID
	}
	_, cancel := context.WithCancel(context.Background())
	c := &frameClient{
		opts:       opts,
		conn:       conn,
		pluginID:   pid,
		instanceID: iid,
		events:     make(chan Event, 64),
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *frameClient) PluginID() string   { return c.pluginID }
func (c *frameClient) InstanceID() string { return c.instanceID }
func (c *frameClient) Events() <-chan Event {
	return c.events
}

func (c *frameClient) Call(ctx context.Context, method string, args any) (json.RawMessage, error) {
	ctx, cancel := withTimeout(ctx, c.opts.Timeout)
	defer cancel()
	res, err := c.conn.Call(ctx, method, args)
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, fmt.Errorf("pluginclient: %s 失败: %s", method, errorText(res.Errors))
	}
	raw, err := json.Marshal(res.Value)
	if err != nil {
		return nil, err
	}
	return nullRaw(raw), nil
}

func (c *frameClient) Subscribe(ctx context.Context, subs []Subscription) error {
	_, err := c.Call(ctx, "plugin.subscribe", subscribeArgs(subs))
	return err
}

func (c *frameClient) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.conn.Close()
	})
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func (c *frameClient) readLoop() {
	defer close(c.done)
	defer close(c.events)
	for {
		f, err := c.conn.Recv()
		if err != nil {
			return
		}
		switch f.Kind {
		case transport.KindEvt, transport.KindCtl:
			c.push(Event{Kind: string(f.Kind), Name: f.Name, Payload: f.Payload})
		case transport.KindReq:
			// SDK 不暴露服务端反向请求处理器：明确回 unsupported，避免对端挂起。
			_ = c.conn.Send(transport.Frame{
				Kind: transport.KindRes, ID: f.ID,
				Errors: []string{"unsupported: pluginclient 不处理服务端请求"},
			})
		}
	}
}

func (c *frameClient) push(ev Event) {
	select {
	case c.events <- ev:
	default:
		// 慢消费者：丢弃而不是阻塞读循环。
	}
}

func helloIDs(v any) (pluginID, instanceID string) {
	m, _ := v.(map[string]any)
	if m == nil {
		return "", ""
	}
	pid, _ := m["plugin_id"].(string)
	iid, _ := m["instance_id"].(string)
	return pid, iid
}

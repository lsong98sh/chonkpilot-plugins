// Package pluginproto 是 browser / desktop 常驻插件共用的 UDS 帧协议。
//
// 传输：Unix domain socket（Windows AF_UNIX），一行一 JSON。
// 请求：{"id","endpoint","params"}；响应：{"id","ok","result"|"error"}。
//
// 端点（两个插件同构）：
//   - start    ：建立会话，返回 context_id / session_id
//   - stop     ：结束会话
//   - operation：执行一个原子动作
//   - ping     ：健康检查（版本 / 会话数 / 运行时长）
//   - shutdown ：优雅退出（仅本机，供 supervisor 收尾）
package pluginproto

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBytes 是单帧上限（防止恶意/异常客户端打爆内存）。
const MaxFrameBytes = 8 << 20

// 端点名。
const (
	EndpointStart     = "start"
	EndpointStop      = "stop"
	EndpointOperation = "operation"
	EndpointPing      = "ping"
	EndpointShutdown  = "shutdown"
)

// 错误码（稳定，供调用方分支）。
const (
	CodeBadRequest      = "bad_request"
	CodeUnknownEndpoint = "unknown_endpoint"
	CodeUnknownContext  = "unknown_context"
	CodeContextExpired  = "context_expired"
	CodeTooManyContexts = "too_many_contexts"
	CodeStartFailed     = "start_failed"
	CodeOperationFailed = "operation_failed"
	CodeUnsupported     = "unsupported"
	CodeInternal        = "internal_error"
)

// Request 是一帧请求。
type Request struct {
	ID       string          `json:"id"`
	Endpoint string          `json:"endpoint"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// Error 是结构化错误。
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Response 是一帧响应。
type Response struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  *Error `json:"error,omitempty"`
}

// OK 构造成功响应。
func OK(id string, result any) *Response {
	return &Response{ID: id, OK: true, Result: result}
}

// Fail 构造失败响应。
func Fail(id, code, msg string) *Response {
	return &Response{ID: id, OK: false, Error: &Error{Code: code, Message: msg}}
}

// ReadRequest 从流中读取一帧请求（按行切分，带长度上限）。
func ReadRequest(r *bufio.Reader) (*Request, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, errors.New("empty frame")
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, fmt.Errorf("bad json: %w", err)
	}
	if req.Endpoint == "" {
		return nil, errors.New("endpoint is required")
	}
	return &req, nil
}

// WriteResponse 写出一帧响应。
func WriteResponse(w io.Writer, resp *Response) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > MaxFrameBytes {
		return errors.New("response frame too large")
	}
	_, err = w.Write(b)
	return err
}

// readLine 读取一行并限制最大长度。
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, chunk...)
		if len(buf) > MaxFrameBytes {
			return nil, errors.New("request frame too large")
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// DecodeParams 把 params 解到 v（空 params 视为空对象）。
func DecodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// Package mcp 实现 Model Context Protocol 服务端，把站点的 REST API
// 暴露给各类 AI 客户端（Claude Code、Claude Desktop、Cursor 等）。
//
// 这里刻意不引第三方 SDK。MCP 在传输层就是 JSON-RPC 2.0，服务端真正
// 需要实现的方法只有五个（initialize / notifications/initialized /
// tools/list / tools/call / ping），加起来两百行左右——比引入一个会
// 随协议演进而变动的依赖更可控，也守住了"单个静态二进制"这条线。
package mcp

import (
	"bytes"
	"encoding/json"
)

// protocolVersion 是本服务端实现的 MCP 版本。
// 客户端在 initialize 里报自己支持的版本；若是我们认识的就原样回应，
// 否则回本服务端的版本，由客户端决定是否继续。
const protocolVersion = "2025-06-18"

var knownVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// JSON-RPC 2.0 标准错误码。
const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification 报告这条消息是否为通知（没有 id，不需要回应）。
func (r *request) isNotification() bool {
	return len(r.ID) == 0 || bytes.Equal(r.ID, []byte("null"))
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func ok(id json.RawMessage, result any) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: result}
}

func fail(id json.RawMessage, code int, msg string) *response {
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// --- initialize ---

type initParams struct {
	ProtocolVersion string `json:"protocolVersion"`
	ClientInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initResult struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    caps       `json:"capabilities"`
	ServerInfo      serverInfo `json:"serverInfo"`
	Instructions    string     `json:"instructions,omitempty"`
}

type caps struct {
	Tools *struct{} `json:"tools,omitempty"`
}

// --- tools ---

// tool 是一个工具定义。InputSchema 必须是合法的 JSON Schema object。
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type callParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// callResult 是 tools/call 的返回。isError 为 true 时内容仍然会交给模型，
// 所以错误文本要写成"模型读了就知道下一步该怎么办"的样子，
// 而不是一句 500。
type callResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func textResult(s string) *callResult {
	return &callResult{Content: []content{{Type: "text", Text: s}}}
}

func errResult(s string) *callResult {
	return &callResult{Content: []content{{Type: "text", Text: s}}, IsError: true}
}

// jsonResult 把结构体序列化成紧凑 JSON 文本返回。
// 模型解析 JSON 没有问题，而紧凑格式比缩进省不少 token。
func jsonResult(v any) *callResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errResult("failed to encode result: " + err.Error())
	}
	return textResult(string(b))
}

// mcpserver 是一个最小可用的 MCP (Model Context Protocol) 工具服务器。
//
// 它通过 stdio (stdin/stdout) 与客户端通信，遵循 JSON-RPC 2.0 协议，
// 实现了 MCP 规范中最核心的三个方法：
//   - initialize      : 握手，声明能力
//   - tools/list       : 列出可用工具
//   - tools/call       : 执行工具调用
//
// 目前只实现了一个工具 read_file，方便先跑通整条链路；
// 想加新工具时，只需要在 toolRegistry 里注册一个新的 ToolHandler。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- JSON-RPC 2.0 基础结构 ----------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------- MCP 工具定义 ----------

// Tool 描述一个工具的 schema（对应 tools/list 返回的内容）。
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties"`
	Required   []string               `json:"required"`
}

// ToolHandler 是工具的实际执行逻辑。入参是 tools/call 传来的 arguments，
// 以及一个带超时的 context——工具实现里如果发起了可取消的操作（比如
// exec.CommandContext 跑 shell、或者 http 请求），要把这个 ctx 传下去，
// 这样超时到了就能真正中断，而不只是 caller 端不再等待。
type ToolHandler func(ctx context.Context, args map[string]interface{}) (string, error)

// sandboxRoot 限制 read_file 只能读这个目录下的文件——最基本的沙箱。
// 由调用方（agent）通过命令行参数指定要暴露哪个目录，而不是写死成
// mcpserver 自己所在的位置：mcpserver 应该是无状态的工具服务，
// "该给它看哪些文件"这个决策权在调用方手里，不该跟这个二进制文件本身绑死。
var sandboxRoot = mustSandboxRoot()

func mustSandboxRoot() string {
	if len(os.Args) > 1 {
		abs, err := filepath.Abs(os.Args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "无效的沙箱目录参数:", err)
			os.Exit(1)
		}
		return abs
	}
	// 没传参数时兜底用当前工作目录，方便直接手动调试。
	wd, _ := os.Getwd()
	return wd
}

var toolRegistry = map[string]Tool{
	"read_file": {
		Name:        "read_file",
		Description: "读取沙箱目录下某个文本文件的内容",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "相对于沙箱根目录的文件路径",
				},
			},
			Required: []string{"path"},
		},
	},
}

// toolTimeout 是单次工具执行允许的最长时间，超过就中断并返回错误。
const toolTimeout = 10 * time.Second

var toolHandlers = map[string]ToolHandler{
	"read_file": handleReadFile,
}

func handleReadFile(ctx context.Context, args map[string]interface{}) (string, error) {
	rel, ok := args["path"].(string)
	if !ok || rel == "" {
		return "", fmt.Errorf("缺少参数 path")
	}

	// 沙箱校验：拼出绝对路径后必须仍然落在 sandboxRoot 内，防止 ../.. 逃逸。
	full := filepath.Join(sandboxRoot, rel)
	full = filepath.Clean(full)
	if !strings.HasPrefix(full, filepath.Clean(sandboxRoot)+string(os.PathSeparator)) && full != sandboxRoot {
		return "", fmt.Errorf("拒绝访问沙箱之外的路径: %s", rel)
	}

	// os.ReadFile 本身不支持 context 取消，所以用一个 goroutine + select
	// 包一层：ctx 超时了就直接返回错误给上层，不再等文件系统操作完成。
	// 对本地小文件这几乎不会触发，但换成网络文件系统或超大文件时就有意义了，
	// 也是未来加 run_shell 这类工具时的标准写法（那边可以直接用 exec.CommandContext）。
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := os.ReadFile(full)
		ch <- result{data, err}
	}()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("读取文件超时: %s", rel)
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("读取文件失败: %w", r.err)
		}
		return string(r.data), nil
	}
}

// ---------- 主循环：从 stdin 读一行 JSON-RPC 请求，处理后写一行响应到 stdout ----------

func main() {
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return // stdin 关闭，退出
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			writeResponse(writer, rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
			})
			continue
		}

		resp := handleRequest(req)
		writeResponse(writer, resp)
	}
}

func handleRequest(req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"serverInfo": map[string]string{
					"name":    "mini-agent-mcpserver",
					"version": "0.1.0",
				},
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
			},
		}

	case "tools/list":
		tools := make([]Tool, 0, len(toolRegistry))
		for _, t := range toolRegistry {
			tools = append(tools, t)
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]interface{}{"tools": tools},
		}

	case "tools/call":
		var params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}}
		}

		handler, ok := toolHandlers[params.Name]
		if !ok {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "unknown tool: " + params.Name}}
		}

		// 每次工具执行都套一个超时，防止某个工具卡死整个 server（尤其是
		// 未来加了 run_shell 这种可能挂起的工具时，这层保护就很关键）。
		ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
		defer cancel()

		text, err := handler(ctx, params.Arguments)
		isError := err != nil
		if err != nil {
			text = err.Error()
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"content": []map[string]string{{"type": "text", "text": text}},
				"isError": fmt.Sprintf("%v", isError),
			},
		}

	default:
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}}
	}
}

func writeResponse(w *bufio.Writer, resp rpcResponse) {
	data, _ := json.Marshal(resp)
	w.Write(data)
	w.WriteString("\n")
	w.Flush()
}

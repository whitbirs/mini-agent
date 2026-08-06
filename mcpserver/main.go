// mcpserver 是一个最小可用的 MCP (Model Context Protocol) 工具服务器。
//
// 它通过 stdio (stdin/stdout) 与客户端通信，遵循 JSON-RPC 2.0 协议，
// 实现了 MCP 规范中最核心的三个方法：
//   - initialize      : 握手，声明能力
//   - tools/list       : 列出可用工具
//   - tools/call       : 执行工具调用
//
// 本文件只负责协议层（JSON-RPC 解析/序列化、stdio 读写、超时控制）；
// 工具的具体实现（read_file、run_shell 等）在 tool 包里，
// 想加新工具时去 tool 包里加，不需要改这个文件。
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

	"mini-agent/mcpserver/sandbox"
	"mini-agent/mcpserver/tool"
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

// toolTimeout 是单次工具执行允许的最长时间，超过就中断并返回错误。
// 注意：容器模式下 "docker run" 本身也有几秒的启动开销，10s 大部分情况够用，
// 如果以后发现容器模式经常超时，可以考虑给两种模式设不同的 timeout。
const toolTimeout = 10 * time.Second

// registry 持有所有已注册工具，在 main() 里根据命令行参数和环境变量初始化。
var registry *tool.Registry

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

// ---------- 主循环：从 stdin 读一行 JSON-RPC 请求，处理后写一行响应到 stdout ----------

func main() {
	sandboxRoot := mustSandboxRoot()
	registry = tool.NewRegistry(sandboxRoot)

	// MCP_SANDBOX_MODE=docker 时启用容器沙箱，run_shell 会在隔离容器里执行。
	// 不设置这个环境变量时保持原来的本地执行行为，向后兼容。
	if os.Getenv("MCP_SANDBOX_MODE") == "docker" {
		docker := sandbox.NewDockerExecutor(
			"alpine:3.20", // 体积小、自带 busybox，覆盖白名单里的 ls/cat/grep/find/wc/pwd
			256,           // 内存上限 256MB
			"0.5",         // CPU 上限 0.5 核
			sandboxRoot,
			"/workspace",
		)
		registry.UseDocker(docker)
		fmt.Fprintln(os.Stderr, "[mcpserver] Docker 沙箱模式已启用，镜像:", docker.Image)
	}

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
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]interface{}{"tools": registry.List()},
		}

	case "tools/call":
		var params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}}
		}

		if !registry.Exists(params.Name) {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "unknown tool: " + params.Name}}
		}

		// 每次工具执行都套一个超时，防止某个工具卡死整个 server。
		ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
		defer cancel()

		text, err := registry.Call(ctx, params.Name, params.Arguments)
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

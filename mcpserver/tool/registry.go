// Package tool 是 mcpserver 的工具实现层，和协议层（main 包的 JSON-RPC/stdio 部分）分开。
// main 包只负责："收到 tools/list 就问 Registry 要工具清单"、
// "收到 tools/call 就把 ctx 和参数丢给 Registry.Call，原样透传结果"；
// 工具怎么实现、沙箱怎么校验，都是这个包的内部细节，main 包不需要关心。
package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mini-agent/mcpserver/sandbox"
)

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

// Handler 是工具的实际执行逻辑。入参是 tools/call 传来的 arguments，
// 以及一个 context——工具实现里如果发起了可取消的操作（比如
// exec.CommandContext 跑 shell），要把这个 ctx 传下去，
// 这样调用方（main 包）设置的超时到了就能真正中断子进程，而不只是不再等待。
// 注意：Handler 自己不创建超时，超时统一由调用方通过 ctx 传入，
// 避免出现"套娃超时"或者两边超时设置不一致的问题。
type Handler func(ctx context.Context, args map[string]interface{}) (string, error)

// Registry 管理所有已注册工具及其 schema，并持有沙箱根目录做路径校验。
type Registry struct {
	sandboxRoot string
	tools       map[string]Tool
	handlers    map[string]Handler
	docker      *sandbox.DockerExecutor // 可选：非 nil 时 run_shell 改为在容器里执行
}

// NewRegistry 创建一个 Registry 并注册内置工具（read_file、run_shell）。
// sandboxRoot 由调用方（main 包解析命令行参数后）传入——工具层不关心
// "沙箱目录该是哪里"这个决策，只负责在这个目录范围内做访问控制。
// 默认不启用 Docker 沙箱（run_shell 直接在宿主机执行，仅受白名单约束）；
// 想启用容器隔离，创建完 Registry 后调用 UseDocker。
func NewRegistry(sandboxRoot string) *Registry {
	r := &Registry{
		sandboxRoot: sandboxRoot,
		tools:       make(map[string]Tool),
		handlers:    make(map[string]Handler),
	}
	r.registerReadFile()
	r.registerRunShell()
	return r
}

// UseDocker 启用容器沙箱：调用之后 run_shell 会把命令委托给 DockerExecutor
// 在隔离容器里执行，而不是直接在宿主机上 exec.CommandContext。
// 白名单校验（第一道防线）仍然保留，容器隔离是叠加的第二道防线。
func (r *Registry) UseDocker(d *sandbox.DockerExecutor) {
	r.docker = d
}

// List 返回所有已注册工具的 schema，供 tools/list 使用。
func (r *Registry) List() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// Call 执行指定工具。ctx 的超时/取消由调用方负责设置。
// 调用前建议先用 Exists 判断工具是否存在，以便协议层能返回正确的 JSON-RPC 错误码
// （"方法/工具不存在"和"工具执行失败"在 JSON-RPC 里是不同语义，不应该靠解析错误文案区分）。
func (r *Registry) Call(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	handler, ok := r.handlers[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return handler(ctx, args)
}

// Exists 判断指定名字的工具是否已注册。
func (r *Registry) Exists(name string) bool {
	_, ok := r.handlers[name]
	return ok
}

// register 是内部注册辅助函数，read_file.go / shell.go 里的 registerXxx 都调用它。
func (r *Registry) register(t Tool, h Handler) {
	r.tools[t.Name] = t
	r.handlers[t.Name] = h
}

// resolveSandboxPath 把相对路径拼到 sandboxRoot 下，并校验结果没有逃逸出沙箱
// （防止 ../.. 之类的路径穿越）。read_file 的 path 和 run_shell 的 cwd 共用这个校验。
func (r *Registry) resolveSandboxPath(rel string) (string, error) {
	if rel == "" {
		return r.sandboxRoot, nil
	}
	full := filepath.Join(r.sandboxRoot, rel)
	full = filepath.Clean(full)
	root := filepath.Clean(r.sandboxRoot)
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("拒绝访问沙箱之外的路径: %s", rel)
	}
	return full, nil
}

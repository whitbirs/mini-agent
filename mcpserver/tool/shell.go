package tool

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// allowedShellCommands 是 run_shell 的命令白名单。
// 采用白名单而不是黑名单：黑名单永远列不全，白名单只放行明确需要的只读命令。
// 目前只放开只读检查类命令，写入/网络类命令（mv、rm、curl 等）不在这里开放。
// 注意：这份白名单同时也是容器沙箱模式的白名单，选用的 alpine 镜像自带
// busybox，ls/cat/grep/find/wc/pwd 都是 busybox 内置 applet，不需要额外装包。
var allowedShellCommands = map[string]bool{
	"ls":   true,
	"cat":  true,
	"grep": true,
	"find": true,
	"wc":   true,
	"pwd":  true,
}

// shellMetaChars 用于拒绝任何看起来像 shell 元字符的输入。
// 我们用 exec.CommandContext(ctx, command, args...) 而不是 sh -c "..."，
// 命令和参数以数组形式传递，天然不会被 shell 解释执行；这里的字符校验是双重保险，
// 防止以后有人不小心把实现改成 sh -c 形式后失去这层保护。这个校验在
// 本地执行和容器执行两种模式下都适用（容器内同样不经过 shell 解释）。
var shellMetaChars = []string{"&&", "||", "|", ";", "`", "$(", ">", "<"}

func containsShellMetaChar(s string) bool {
	for _, c := range shellMetaChars {
		if strings.Contains(s, c) {
			return true
		}
	}
	return false
}

// containsPathEscape 拦截两种能绕过 cwd 沙箱限制的参数写法：
//  1. 相对路径穿越，如 "../../../etc/passwd"——不含 shell 元字符，但能让
//     cat/grep/find 之类的命令跳出 cwd 去读沙箱外的文件。
//  2. 绝对路径，如 "/etc/passwd"——完全不受 cwd 限制，命令会直接按绝对路径打开。
//
// 这里只对 args 做拦截，command 本身（可执行文件名）不需要这层校验，
// 因为 command 已经被白名单锁死，不可能是一个路径。
// 目前白名单里都是只读检查类命令，合法用法不需要用 ".." 跳目录或传绝对路径，
// 所以直接拒绝，比试图精确判断"是不是逃出了沙箱"更简单也更不容易漏判。
func containsPathEscape(s string) bool {
	if strings.Contains(s, "..") {
		return true
	}
	if strings.HasPrefix(s, "/") {
		return true
	}
	return false
}

// extractCommand 从原始参数里解析出 command 字符串。
// schema 里 command 定义的类型是 string，但本地模型（尤其是参数量较小的模型）
// 偶尔会因为 command 和 args 挨得太近，把 command 也生成成单元素数组，
// 比如 {"command": ["ls"]} 而不是 {"command": "ls"}。
// 与其指望模型每次都严格遵守 schema，这里做一层防御性容错：
// 正常字符串直接用；单元素字符串数组也接受，拆出唯一元素；
// 其他情况（空、多元素数组、非字符串元素等）一律拒绝，不做更激进的猜测。
func extractCommand(raw interface{}) (string, error) {
	if s, ok := raw.(string); ok {
		if s == "" {
			return "", fmt.Errorf("缺少参数 command")
		}
		return s, nil
	}
	if arr, ok := raw.([]interface{}); ok && len(arr) == 1 {
		if s, ok := arr[0].(string); ok && s != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("缺少参数 command")
}

func (r *Registry) registerRunShell() {
	r.register(
		Tool{
			Name:        "run_shell",
			Description: "执行一个白名单内的单条 shell 命令（不支持管道/重定向/命令链），用于只读检查类任务，如列目录、搜索文本",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]interface{}{
					"command": map[string]interface{}{
						"type":        "string",
						"description": "可执行文件名，例如 'ls'，必须在白名单内",
					},
					"args": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "参数列表，不能包含 shell 元字符，也不能是绝对路径或包含 \"..\"",
					},
					"cwd": map[string]interface{}{
						"type":        "string",
						"description": "相对于沙箱根目录的工作目录，不传则默认为沙箱根目录",
					},
				},
				Required: []string{"command"},
			},
		},
		r.handleRunShell,
	)
}

func (r *Registry) handleRunShell(ctx context.Context, args map[string]interface{}) (string, error) {
	command, err := extractCommand(args["command"])
	if err != nil {
		return "", err
	}
	if !allowedShellCommands[command] {
		return "", fmt.Errorf("命令不在白名单内: %s", command)
	}
	if containsShellMetaChar(command) {
		return "", fmt.Errorf("command 包含非法字符")
	}

	var cmdArgs []string
	if rawArgs, ok := args["args"].([]interface{}); ok {
		for _, a := range rawArgs {
			s, ok := a.(string)
			if !ok {
				return "", fmt.Errorf("args 中的元素必须都是字符串")
			}
			if containsShellMetaChar(s) {
				return "", fmt.Errorf("参数包含非法字符: %s", s)
			}
			if containsPathEscape(s) {
				return "", fmt.Errorf("参数疑似路径穿越或绝对路径，已拒绝: %s", s)
			}
			cmdArgs = append(cmdArgs, s)
		}
	}

	cwdRel, _ := args["cwd"].(string)
	// resolveSandboxPath 只是校验 cwdRel 没有逃逸出沙箱（返回值这里用不上，
	// 本地执行分支会重新拿一次绝对路径；容器分支直接用 cwdRel 相对路径去拼容器内路径）。
	if _, err := r.resolveSandboxPath(cwdRel); err != nil {
		return "", err
	}

	// 容器沙箱模式：命令委托给 DockerExecutor 在隔离容器里执行。
	if r.docker != nil {
		return r.docker.Run(ctx, command, cmdArgs, cwdRel)
	}

	// 本地执行模式（默认）：exec.CommandContext 而不是 "sh -c"，命令和参数
	// 以数组形式传入，不经过 shell 解释，ctx 超时后会自动 kill 掉子进程。
	full, err := r.resolveSandboxPath(cwdRel)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, command, cmdArgs...)
	cmd.Dir = full

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("命令执行失败: %w", err)
	}
	return string(out), nil
}

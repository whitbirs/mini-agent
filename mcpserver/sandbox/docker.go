// Package sandbox 提供命令的容器化隔离执行能力，作为 tool 包里白名单方案的
// 纵深防御层：即使白名单校验被绕过，或者以后新增了风险更高的命令，
// 命令也只能在隔离容器里造成有限影响（无网络、只读文件系统、资源上限）。
package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	stdpath "path"
	"path/filepath"
	"strings"
)

// DockerExecutor 把命令包装进一个一次性容器执行。
type DockerExecutor struct {
	Image      string // 建议用体积小、攻击面小的基础镜像，如 "alpine:3.20"
	MemoryMB   int    // 内存上限，例如 256
	CPUs       string // CPU 上限，例如 "0.5"
	MountRoot  string // 宿主机沙箱根目录（对应 tool.Registry 的 sandboxRoot）
	MountPoint string // 容器内挂载点，例如 "/workspace"
}

func NewDockerExecutor(image string, memoryMB int, cpus, mountRoot, mountPoint string) *DockerExecutor {
	return &DockerExecutor{
		Image:      image,
		MemoryMB:   memoryMB,
		CPUs:       cpus,
		MountRoot:  mountRoot,
		MountPoint: mountPoint,
	}
}

// Run 在容器内执行 command + args。relCwd 是相对于沙箱根目录的工作目录
// （由调用方在传入前完成沙箱路径校验，这里不重复校验路径穿越）。
func (d *DockerExecutor) Run(ctx context.Context, command string, args []string, relCwd string) (string, error) {
	containerWorkDir := d.MountPoint
	if relCwd != "" {
		// 容器内是 Linux 路径，统一用 "/" 拼接；filepath.ToSlash 处理
		// 宿主机在 Windows 上可能出现的 "\" 分隔符。
		containerWorkDir = stdpath.Join(d.MountPoint, filepath.ToSlash(relCwd))
	}

	dockerArgs := []string{
		"run", "--rm",
		"--network=none", // 禁网络，防止数据外传或下载恶意 payload
		"--read-only",    // 根文件系统只读，只有挂载的 MountRoot 可写
		fmt.Sprintf("--memory=%dm", d.MemoryMB),
		fmt.Sprintf("--cpus=%s", d.CPUs),
		"--pids-limit=64", // 防 fork 炸弹
		"-v", fmt.Sprintf("%s:%s", d.MountRoot, d.MountPoint),
		"-w", containerWorkDir,
		d.Image,
		command,
	}
	dockerArgs = append(dockerArgs, args...)

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("容器执行失败: %w (docker %s)", err, strings.Join(dockerArgs, " "))
	}
	return string(out), nil
}

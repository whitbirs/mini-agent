package tool

import (
	"context"
	"fmt"
	"os"
)

func (r *Registry) registerReadFile() {
	r.register(
		Tool{
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
		r.handleReadFile,
	)
}

func (r *Registry) handleReadFile(ctx context.Context, args map[string]interface{}) (string, error) {
	rel, ok := args["path"].(string)
	if !ok || rel == "" {
		return "", fmt.Errorf("缺少参数 path")
	}

	full, err := r.resolveSandboxPath(rel)
	if err != nil {
		return "", err
	}

	// os.ReadFile 本身不支持 context 取消，所以用一个 goroutine + select
	// 包一层：ctx 超时了就直接返回错误给上层，不再等文件系统操作完成。
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
	case res := <-ch:
		if res.err != nil {
			return "", fmt.Errorf("读取文件失败: %w", res.err)
		}
		return string(res.data), nil
	}
}

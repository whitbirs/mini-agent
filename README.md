# mini-agent

最小可运行的 MCP Server + ReAct Agent Loop demo。

## 目录结构
- `mcpserver/main.go`：MCP 工具服务器，走 stdio，实现了 initialize / tools/list / tools/call，
  目前只有一个工具 `read_file`（带基础沙箱：只能读运行目录及其子目录下的文件）。
- `agent/main.go`：agent loop，启动 mcpserver 子进程，调用 Anthropic Messages API，
  按 ReAct 模式循环：LLM 决策 -> 工具调用 -> 结果回填 -> 继续，直到给出最终答案。

## 跑起来

```bash
# 1. 编译两个模块
cd mcpserver && go build -o mcpserver . && cd ..
cd agent && go build -o agent . && cd ..

# 2. 准备一个测试文件，放在 mcpserver 运行目录下（agent 默认从 agent/ 目录启动 mcpserver）
echo "这是一段测试内容，用来验证 agent 能否读到它。" > mcpserver/test.txt

# 3. 设置 API Key 并运行
export ANTHROPIC_API_KEY=sk-xxx
cd agent && ./agent "帮我读取 test.txt 并用一句话总结内容"
```

## 想换成 Kimi API
把 `agent/main.go` 里的 `callLLM` 函数换成 Moonshot 的 OpenAI 兼容格式即可：
- URL: `https://api.moonshot.cn/v1/chat/completions`
- 请求体是标准 OpenAI `tools` / `tool_calls` 格式，和 Anthropic 的 `tools` / `tool_use` 字段名不同，
  但 agent loop 的整体骨架（发消息 -> 解析工具调用 -> 执行 -> 回填 -> 循环）完全不用变。
  这也是这个 demo 想让你体会的地方：**协议细节会变，但 agent 的核心循环逻辑是稳定的**。

## 接下来可以扩展的方向（对应 Agent Infra）
1. 给 mcpserver 加第二个工具（比如 `run_shell`），体会"工具越强大，沙箱设计越重要"
2. 把 read_file 的沙箱校验换成用 Docker/gVisor 跑一个真正隔离的进程
3. 把单 agent 改成多 agent（比如加一个 planner + 多个 worker），对应 Kimi Agent Swarm 的思路
4. 加上下文压缩：当 messages 太长时，让 LLM 自己总结历史再继续（对应 K2.6 的长会话压缩机制）

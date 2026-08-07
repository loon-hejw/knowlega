# ADR-0001: QM 作为产品主干，Knowlega 作为内部知识 Agent

- 状态：Accepted
- 日期：2026-08-07
- 范围：项目知识、个人记忆、文件、会话沉淀和 Agent 集成边界

## 决策

QM 是产品、协作、权限和 Agent runtime 的主干；Knowlega 是同一 Go 后端中的
内部 Agent：

```text
QM Go backend
├── users / projects / memberships / permissions
├── sessions / memory / files / runs
└── internal/agent/knowlega
    ├── immutable raw sources
    ├── durable Markdown wiki + page versions
    ├── queue / compiler / query / writeback
    └── lint / review / graph / derived PostgreSQL sync
```

Markdown/raw 是知识唯一事实来源，PostgreSQL 只保存可重建的索引、图谱、manifest、
review 和 query log。Knowlega 不管理 QM 项目、成员或会话权限，也不把浏览器变成
第二知识实现。

## 来源飞轮

- QM 文件成功写入后，按内容哈希写入 Agent scope 的 `raw/sources/` 并进入
  `.kbcore/ingest-queue.json`。
- Memory revision 成功后进入个人 scope；项目文件和项目记忆只进入对应项目 scope。
- 对话只有在归档/完成且通过最小价值门槛后才进入队列，过滤寒暄和原始噪声。
- Agent 维护任务消费队列，使用两阶段 LLM Wiki 编译、版本化页面、manifest、lint
  和 review；查询必须读取证据并携带引用，合格综合结论写入 `wiki/syntheses/`。

## 清理与权限

清理通过 Agent 的 `DeleteSource` 完成：派生页面、链接、manifest 和 review 按
来源关系更新；raw 证据默认保留，只有显式 `delete_raw` 才删除归档。作用域根目录由
QM 后端派生，调用方不能提交任意文件系统路径。

## 运输边界

Knowlega 不再提供独立服务、前端 panel、MCP 或 Knowledge gRPC。应用使用 QM HTTP
API 以及 QM control/runner gRPC；Agent 只能在 QM 后端内部调用。

## 验收

必须覆盖个人/项目隔离、文件/记忆/高价值对话入队、LLM Wiki 编译、引用与写回、页面
版本归档、队列恢复/重试以及来源清理。启动和调试入口统一为 `cmd/qm-backend`。

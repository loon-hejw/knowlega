# ADR-0001: QM 作为协作底座，Knowledge Core 作为知识插件

- 状态：Accepted（长期方向）
- 日期：2026-08-05
- 范围：项目知识、个人知识、Agent 集成和部署边界

## 背景

Knowledge Core 已经负责持久化 LLM Wiki：`raw/` 保存不可变来源，`wiki/`
保存可读且持续演化的知识，`.kbcore/` 保存可恢复的 manifest、队列和版本
档案，PostgreSQL 保存可重建的索引、图谱和运行状态。

QM 提供更适合长期使用的上层运行环境：用户、项目、成员、权限、频道、任务、
后台工作和 Agent 运行时。QM 的共享 scope、skills、tools 和 deployment
plugins 是组织级能力的扩展点，项目协作不应在 Knowledge Core 中重复实现。

## 决策

长期产品架构采用两层职责边界：

```text
QM
├── identity / users / memberships / permissions
├── projects / channels / tasks / schedules
└── agent runtime / sandbox / approvals

Knowledge Core
├── immutable source ingestion and provenance
├── durable Markdown Wiki and page versions
├── planner-driven query, evidence and citations
├── lint, semantic review and maintenance queue
└── project and personal knowledge promotion
```

QM 是协作与访问控制底座，Knowledge Core 是知识生产、检索和维护插件。
Knowledge Core 不创建或管理 QM 项目、成员、任务和频道，也不复制 QM 的协作
状态。Knowledge Core 以独立 Go 服务运行，通过 QM 的 plugin、skill、tool 调用
窄门面 gRPC API。现有 MCP/HTTP 入口继续用于本地和兼容场景，但不是 QM 的主
集成边界。

QM 的项目或个人 scope 映射到 Knowledge Core 的受控 scope。第一阶段可以用
现有的 `database.project_id` 作为外部标识，同时由服务端维护
`scope_id -> project root` 映射；Agent 不得自行提交任意文件系统路径。

## Scope 与知识晋升

### Personal scope

- 默认只有所属用户可读写。
- 个人笔记可以被检索和整理，但不会自动进入项目 Wiki。
- 晋升到项目 scope 必须由用户显式操作，并保留原始来源、操作者和时间。

### Project scope

- 项目成员按 QM 权限共享项目 Wiki 和来源证据。
- 项目资料、会议记录、决策、代码知识和综合页都归属于项目 scope。
- 资料导入、Wiki writeback 和 review 状态变更必须同时通过 QM 权限与
  Knowledge Core 的写入校验。

### Organization scope（后续阶段）

- 跨项目知识只能通过显式晋升或管理员审核产生。
- 不把项目 Wiki 自动合并成组织知识，避免权限泄漏和未经审查的事实扩散。

## 插件接口边界

第一版面向 QM 的最小能力契约为：

- `project_docs_query`：基于持久化 Wiki 返回带引用的回答；
- `project_docs_search`、`project_docs_read`：读取候选和证据；
- `project_docs_ingest`：提交来源并返回异步任务状态；
- `project_docs_review`：查看待处理的知识维护项；
- `project_docs_status`：返回来源、队列、Wiki 和索引状态。

以下能力默认不向 QM Agent 暴露：任意路径写入、项目初始化、任意源文件删除、
绕过确认的批量 writeback，以及直接修改 PostgreSQL 状态。

Knowledge Core 的回答必须沿用现有 LLM Wiki 约束：搜索只是候选召回，答案必须
来自已读取的 Wiki/raw/graph 证据；引用只能指向实际证据；写回必须显式授权并
产生版本归档。

## 部署形态

推荐的初始部署是：

```text
QM project / personal scope
        ↓
QM skill + fixed tool adapter
        ↓
kbcore serve 或 kbcore mcp
        ↓
Markdown Wiki + raw sources + optional PostgreSQL
```

不要把 Knowledge Core 的实现代码复制进 QM，也不要让 QM 成为 Markdown 的
第二事实来源。QM deployment layer 只保存适配器、skills、工具描述、服务地址
和密钥引用；`raw/`、`wiki/` 和 `.kbcore/` 仍由 Knowledge Core 管理。

现有 `kbcore serve` 是第一阶段的接入基础，新增
`api/proto/knowledge/v1/knowledge.proto` 和 gRPC 窄门面。MCP 工具继续保留，
但不作为 QM 的主通道；QM 应调用高层、带引用的 query 契约，避免 Agent 只拿
搜索片段直接回答。

## 分阶段路线

### Phase 1：只读知识插件

- 固化 scope 映射和访问校验；
- 提供项目/个人文档搜索、读取、带引用问答；
- 在 QM 中以 skill + fixed tools 接入；
- 验证跨用户、跨项目隔离和引用正确性。

### Phase 2：资料沉淀和知识晋升

- 从 QM 附件、文档和用户显式保存的消息导入 `raw/`；
- 支持个人知识晋升为项目知识；
- 支持 source-summary、实体页、概念页和 synthesis writeback；
- 所有写操作保留 provenance、审计信息和 page versions。

### Phase 3：后台知识维护

- 使用 QM 的 cron/watch 触发 scan、queue-ingest、review 和 lint；
- 提供项目知识健康度和待办状态；
- 将高频问题、知识缺口和 review 结果反馈到项目协作流。

### Phase 4：组织知识和独立 Knowledge Agent

- 在管理员审核下形成跨项目组织知识；
- 根据成本和延迟需要，增加独立的知识专家 Agent；
- QM 主 Agent 可选择调用高层答案接口，或调用低层证据工具自行综合。

## 非目标

- 在 Knowledge Core 中重新实现项目管理、任务管理、成员管理或消息系统；
- 把所有 QM 对话自动写入 Wiki；
- 用 QM 的普通 memory 替代 `raw/`、`wiki/` 和版本化 provenance；
- 为了“插件化”而牺牲 Markdown 的人类可读性和 Git/Obsidian 兼容性。

## 验收标准

当第一阶段完成时，应能证明：

1. QM 项目成员可以从项目 scope 查询文档并得到可核验引用；
2. 个人笔记默认不会被其他项目成员读取；
3. 未经授权的路径、项目和写操作会被拒绝；
4. 同一个 Knowledge Core 服务可以安全区分多个 QM scope；
5. 停止 QM 集成后，Knowledge Core 的 Markdown Wiki 仍可独立读取、查询和维护。

## 参考

- [Knowledge Core Architecture](../architecture.md)
- [Knowledge Core Service API](../service-api.md)
- [QM README](https://github.com/yc-software/qm/blob/main/README.md)
- [QM Deployment Directory Contract](https://github.com/yc-software/qm/blob/main/docs/deploy-directory.md)

# QM 集成契约（实验性联合仓库）

Knowledge Core 保留 Markdown Wiki、raw 来源、`.kbcore/` 版本与队列，以及
PostgreSQL 索引；QM 负责用户、项目、成员、权限、会话和审计。QM 不直接传入
文件系统路径，也不复制 Knowledge Core 的知识内容。

## 服务边界

启用 `server.grpc.enabled` 后，`kbcore serve` 在独立地址提供
`knowledge.v1.KnowledgeCore`。协议源文件是
[`api/proto/knowledge/v1/knowledge.proto`](../api/proto/knowledge/v1/knowledge.proto)。

首期 RPC：

- `EnsureScope`：QM 控制面创建或恢复项目 scope；根目录由 Knowledge Core 的
  `server.grpc.scope_root` 派生，调用方不能提交路径。
- `GetStatus`：返回项目、Wiki 页、来源和就绪状态。
- `Query`：返回基于已读取证据的答案、引用和候选结果，不执行 writeback。
- `Search`：只做候选召回，不能替代 `Query` 的证据读取流程。
- `ReadDocument`：只允许读取 `wiki/` 或 `raw/sources/` 下的项目相对路径。

QM 每次调用携带当前 scope；当前 QM 项目实现使用
`group:web-project-<qmProjectId>` 作为项目 scope（以 QM 的
`projectScopeId` 返回值为准），Knowledge Core 不重写这个外部标识。
Knowledge Core 单项目命令行模式的默认绑定仍是
`group:project:<projectID>`。`CallerContext.scope_id` 必须和请求 scope 一致，
否则返回 `PERMISSION_DENIED`。

## 配置

```yaml
server:
  grpc:
    enabled: true
    addr: 127.0.0.1:19830
    auth_token: "development-only-secret"
    require_auth: true
    scope_root: /var/lib/knowledge-core/scopes
```

生产部署应在私有网络使用 TLS/mTLS；`auth_token` 仅用于当前窄门面服务认证，
不应写入仓库。数据库启用时应先执行现有 `--migrate-db`，以创建
`scope_bindings` 表。

## QM 侧适配约定

QM 集成代码应放在独立的 `knowledge-core` 命名空间中：

1. 从 proto 生成 Node gRPC client，并注入 QM wiring/BuiltApp。
2. 从当前项目会话读取 scope 和 capability，不让工具接收本地路径。
3. 暴露 `knowledge.query`、`knowledge.search`、`knowledge.read`、
   `knowledge.status` 四个只读工具。
4. 调用使用 QM 现有 request id、幂等 ledger 和 audit log。
5. Knowledge Core 不可用时返回可识别的 `knowledge_unavailable`，不影响 QM
   的其他 Agent 能力。

写入、来源摄取、review 和知识晋升暂不加入首期 RPC；它们将在独立的后续契约
中增加，并继续使用 Knowledge Core 的版本化写入与 provenance 校验。

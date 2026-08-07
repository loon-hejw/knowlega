# QM 与 Knowlega Agent 集成

QM 是产品主干，Knowlega 是 QM 后端中的内部 Go Agent。两者共享 QM 的
PostgreSQL，但知识内容的权威来源仍是每个作用域下的 Markdown/raw 文件。

## 作用域

- `personal:<principal>`：个人记忆与个人文件。
- `group:web-project-<id>`：QM 项目文件、项目记忆和项目会话。
- 其他 QM scope：按 `kind + external_scope_id` 隔离，根目录由后端派生，调用方
  不能提交任意文件系统路径。

## 写入飞轮

1. QM 成功写入文件、memory revision 或会话归档。
2. 后端把内容以内容寻址方式写入 `raw/sources/`，再追加
   `.kbcore/ingest-queue.json`。
3. Agent 维护任务消费队列，调用完整的两阶段 LLM Wiki 编译器，并使用版本化
   Wiki 写入保留旧证据。
4. 查询先读取导航页，再通过 planner/action loop 读取 raw/wiki/graph 证据，答案
   携带引用；符合条件的综合结论写入 `wiki/syntheses/`。
5. PostgreSQL 只保存可重建的索引、图谱、manifest、review 和 query log。

## 清理

清理通过 Agent 的 `Cleanup`/`DeleteSource` 执行：raw 证据默认保留并标记忽略，
派生 Wiki 页面、链接、manifest 和 review 状态按来源关系清理；需要明确
`delete_raw` 才会删除 raw 归档。

## 运输边界

Knowlega 不再提供独立服务、前端 panel 或 Knowledge gRPC。应用使用 QM HTTP
API 及 QM control/runner gRPC；Agent 只能在 QM 后端内部调用。

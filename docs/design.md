# 业务与接口契约（拟实现）

**权限**：上游只建 Pull Mirror；权威仓保护 main/master，禁止直接推送，要求指定评审批准；开发者写 Fork 或授权分支。Bridge 不用管理员令牌替用户合并。每次展示、导出和写操作都按当前用户重新检查涉及仓库的权限；跨仓回执不可泄漏不可见成员。

**存储**：独立 SQLite WAL。ProjectGroup 记录组及成员；TopicChangeSet 记录 Topic、版本号与逐仓操作结果；DeliveryReceipt 记录不可覆盖的回执版本、摘要与快照位置。首版成员清单可用 JSON；另设技术表 WebhookInbox，事件投递 ID 唯一，用于去重和失败重试。服务重启后继续未完成任务。

**联调**：按项目白名单分页读取 PR，从标题或描述识别完整 `[Topic: CAD-TOL-01]`，也可手选仓库、源分支和目标分支。以仓库 ID + PR 编号去重。界面逐仓展示变更文件数、CI、冲突、审批、head SHA 和最后刷新时间。未知不是通过。批量建 PR 记录逐项结果，重试时复用已有 PR。

**合并与恢复**：每仓独立确认，检查预期 head SHA、审批与分支保护，交由 Gitea 最终裁决。请求超时先回查，不盲重试。部分成功时保留已合并项，将失败仓标红；可选择修复冲突重试，或以实际合并提交生成反向 PR，仍须人工批准。不能安全自动反向时转人工，不 reset 主干。

**证据**：内部 webhook 接收 pull_request、release，校验原始请求体 HMAC 并持久去重，异步回查 Gitea。绑定仓库、PR、实际合并 SHA 或 release 标签解析后的提交、变更清单、验证范围、执行人和时间。制品用受管根目录下的相对标识，拒绝越界路径和重解析点；先复制到受控快照再流式计算 SHA-256。未验证项目显式标记。JSON 为权威快照，Markdown 从快照生成。摘要不是防篡改保证：签名并将摘要送独立只追加存储，公开验签材料；更正只能生成关联的新版本。

|页面 / 接口|作用|
|---|---|
|GET /bridge/api/projects|项目总览、分支保护巡检、最新提交|
|POST /bridge/api/topics|按组或显式成员建单，支持幂等键|
|GET /bridge/api/topics/{id}|逐仓只读状态与部分成功摘要|
|POST /bridge/api/topics/{id}/members/{member}/merge 或 /revert-pr|当前用户确认的单仓操作|
|POST /internal/hooks/gitea|仅服务间访问；验签入队后返回 202|
|POST /bridge/api/receipts；GET /bridge/api/receipts/{id}|创建快照、查看、导出 JSON/Markdown|

页面使用原生 ES Modules；导航只挂“多仓联调”和“交付回执”两个链接。部署时选择并验证对应版本的导航扩展点，不覆盖整套模板；注入失效时保留 `/bridge/` 独立入口。

## 已实现的保存接口

配置 Bridge 独立 SQLite 后，`POST /bridge/api/topics` 接收同源 JSON 请求及 `Idempotency-Key`。请求字段为 `project_id`、`topic` 和可选的 `members`；省略 `members` 表示使用整组，显式指定时每项填写 `repository_id`，可同时填写 `source_branch` 和 `target_branch`。建单时每个成员仓都必须在项目白名单内，且创建者有读取权限。建单不创建分支或 PR。

首次保存返回 `201`，同用户重试相同请求返回原联调单（`200`）；同用户用同一个键提交不同内容返回 `409`。`GET /bridge/api/topics/{id}` 返回固定成员、版本、建单人和即时查询结果；任一成员不再可访问时，整张单返回与不存在相同的 `404`。这些接口当前只创建版本 1，逐仓写入及操作结果更新仍待实现。

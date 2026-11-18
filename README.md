# Gitea Topic Bridge

Windows 自建 Git 服务的多仓联调与交付回执扩展。Gitea 管单仓权限；Bridge 汇总多仓 PR。**当前支持项目总览与默认分支巡检、联调单、同仓及白名单直接 Fork 建 PR、逐仓合并、反向 PR、内部 Webhook，以及可选的网页回执中心、签发、导出和独立存证。** 提供 Gitea 菜单片段及 Bridge 原生 Windows 服务模式。临时 Windows VM 已验证 Bridge 进程故障自动恢复，以及四个服务全部停启后联调和交付数据仍可读取。目标主机开机自启仍须现场验收。

运行 `go test ./...` 可验证核心与模拟 HTTP 流程。真实 Gitea 测试和版本见[验收记录](docs/acceptance.md#已验证本地只读查询)，实际启动参数见[部署说明](docs/deployment.md#启动看板)。

```text
浏览器 / Git HTTPS → 网关 ┬─ /        → Gitea :3000 → 裸仓与业务库
                         └─ /bridge/ → Bridge :3001 → 独立 SQLite 与回执
Gitea → 签名 Webhook → Bridge；Bridge → 按用户授权访问 Gitea API
```

[首版实施设计](docs/mvp.md) · [部署与访问入口](docs/deployment.md) · [业务设计与接口](docs/design.md) · [实施与验收](docs/acceptance.md) · [网关样例](examples/Caddyfile)

2026-11-18 核对：[官方版本 v28.4.0](https://github.com/go-gitea/gitea/releases/tag/v28.4.0)，提供 `gitea-28.4.0-windows-amd64.exe`。上线前核验签名及 SHA-256。单文件交付是指每个服务各一个程序，不是整套系统只有一个文件；主机还需 Git。

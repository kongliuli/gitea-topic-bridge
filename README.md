# Gitea Topic Bridge

Windows 自建 Git 服务的多仓联调与交付回执扩展。Gitea 管单仓权限；Bridge 把多个 PR 放进一张联调单。**当前已有设计文档、配置样例和首版查询核心；尚无可运行的 Bridge 服务。**

使用 `go test ./...` 运行模拟仓库测试，覆盖 Topic 匹配、权限过滤、分页和查询失败。测试不需要 Gitea 账号；真实 Gitea 的授权、接口和页面尚未联调。

```text
浏览器 / Git HTTPS → 网关 ┬─ /        → Gitea :3000 → 裸仓与业务库
                         └─ /bridge/ → Bridge :3001 → 独立 SQLite 与回执
Gitea → 签名 Webhook → Bridge；Bridge → 按用户授权访问 Gitea API
```

[首版实施设计](docs/mvp.md) · [部署与访问入口](docs/deployment.md) · [业务设计与接口](docs/design.md) · [实施与验收](docs/acceptance.md) · [网关样例](examples/Caddyfile)

2026-11-18 核对：[官方版本 v28.4.0](https://github.com/go-gitea/gitea/releases/tag/v28.4.0)，提供 `gitea-28.4.0-windows-amd64.exe`。上线前核验签名及 SHA-256。单文件交付是指每个服务各一个程序，不是整套系统只有一个文件；主机还需 Git。

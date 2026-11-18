# Gitea Topic Bridge

Windows 自建 Git 服务的多仓联调与交付回执扩展。Gitea 管单仓权限；Bridge 汇总多仓 PR。**当前支持联调单、逐仓合并、反向 PR、内部 Webhook，以及可选的回执签发和导出 API。** 跨 Fork 提交、独立只追加存证、回执中心和 Windows 服务安装仍待实现。

运行 `go test ./...` 可验证核心与模拟 HTTP 流程。真实 Gitea 测试和版本见[验收记录](docs/acceptance.md#已验证本地只读查询)，实际启动参数见[部署说明](docs/deployment.md#启动看板)。

```text
浏览器 / Git HTTPS → 网关 ┬─ /        → Gitea :3000 → 裸仓与业务库
                         └─ /bridge/ → Bridge :3001 → 独立 SQLite 与回执
Gitea → 签名 Webhook → Bridge；Bridge → 按用户授权访问 Gitea API
```

[首版实施设计](docs/mvp.md) · [部署与访问入口](docs/deployment.md) · [业务设计与接口](docs/design.md) · [实施与验收](docs/acceptance.md) · [网关样例](examples/Caddyfile)

2026-11-18 核对：[官方版本 v28.4.0](https://github.com/go-gitea/gitea/releases/tag/v28.4.0)，提供 `gitea-28.4.0-windows-amd64.exe`。上线前核验签名及 SHA-256。单文件交付是指每个服务各一个程序，不是整套系统只有一个文件；主机还需 Git。

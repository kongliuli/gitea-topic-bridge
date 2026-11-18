# 服务与网络

Gitea、Go Bridge、Caddy 分别作为 Windows 服务运行。Bridge 原生支持 SCM 启停，内嵌 HTML/CSS/ES Modules。服务使用独立低权限账户，自动启动、失败退避重启。程序目录只读，数据与备份分开。Gitea 独占裸仓；Bridge 只读制品，不读写 Gitea 数据库。

|模式|示例入口|上线条件|
|---|---|---|
|内网 DNS|`https://git.office.example:8443/bridge/`|内网解析，办公网段白名单，终端信任内部 CA|
|域名|`https://git.example.com/bridge/`|有效证书；有域名不代表必须开放公网|
|IP:端口|`https://192.0.2.10:8443/bridge/`|替换文档 IP，证书 SAN 包含实际 IP，防火墙限源|
|Tailscale|`https://git-host.example-tailnet.ts.net:8443/bridge/`|实际 MagicDNS 全名，Serve 终止 TLS，tailnet grants 限制成员，不开 Funnel|

每个实例选一个规范入口：Gitea `ROOT_URL`、Bridge `PUBLIC_BASE_URL`、OAuth 回调与克隆地址保持一致。多网共用优先选同一域名配分区 DNS；其他别名只有在规范地址可达时才跳转，且别名本身也需要有效证书。不从 Host 头动态生成登录回调。

后端仅绑定 127.0.0.1:3000、127.0.0.1:3001，默认 Git over HTTPS。示例网关用于内网 DNS；Tailscale Serve 模式改为 HTTP 回环网关 127.0.0.1:8080，由 Serve 转发至它，仅在 tailnet 暴露 HTTPS 端口。外部网关不转发内部 webhook，不从转发头推断身份。

Bridge 用 Gitea OAuth2 授权码、PKCE 与 state。固定回调 `/bridge/auth/callback`，令牌只保存在服务端；会话 Cookie 使用 Secure、HttpOnly、SameSite=Lax，写请求验证 CSRF。复用已有 Gitea 登录，不复制 Gitea 会话密钥；首次可能要求授权。

## 启动看板

1. 在 Gitea 注册机密 OAuth2 应用。回调须完整匹配网关地址，例如 `https://git.office.example:8443/bridge/auth/callback`。默认申请 `read:user` 和 `read:repository`，不需要管理员令牌。`enable_pull_creation` 和 `enable_merge` 独立开关、默认关闭；开启任一个都需要 Bridge 数据库和重新登录授权 `write:repository`。这是 Gitea 的仓库级写权限，不是仅限 PR。只在需要写操作的实例启用。
2. 复制 `examples/bridge.json` 到部署目录，填写 Gitea 内部地址、两个浏览器入口、OAuth client ID 及仓库白名单。仓库 `id` 是 Gitea API 返回的数字 ID；删除重建仓库后需确认新 ID，不按同名自动接管。若要保存联调单，先在本机磁盘创建受限的数据目录，再配置 `database_path` 为其中的绝对路径；不要使用共享盘、Gitea 数据库或其他 Gitea 实例的 Bridge 数据库。未设置 `database_path` 时只开放即时查询。
3. 使用 Go 1.26 或以上版本，在仓库目录执行 `go build -o bridge.exe ./cmd/bridge`。从安全的启动环境设置 `BRIDGE_OAUTH_CLIENT_SECRET`，再执行 `.\bridge.exe -config C:\path\to\bridge.json`。默认监听 `127.0.0.1:3001`，通过网关访问 `/bridge/`。不要把密钥写进配置或提交到 Git。
4. 当前以前台进程运行。会话只保留在内存，最长一小时，进程重启须重新登录。`allow_loopback_http` 仅用于隔离实验，非回环 HTTP 不允许。

## 运维约束

先确认 github443 是 HTTP 代理还是 SSH 别名，分别配置服务账号的出站代理或 SSH 配置，不能混用。校验证书或主机密钥；Webhook 出站仅放行指定 Bridge 地址。不要修改机器全局代理。

升级前备份数据库、裸仓、配置、密钥与快照；SQLite 使用在线备份或停写后复制。在副本验证 OAuth、模板、API 与 webhook，数据库迁移后需要连同数据一起回滚。独立扩展不等于永远兼容。

参考：[Gitea 配置](https://docs.gitea.com/administration/config-cheat-sheet)、[模板](https://docs.gitea.com/administration/customizing-gitea/)、[Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve)。

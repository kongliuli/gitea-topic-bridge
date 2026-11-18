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

先确认 github443 是 HTTP 代理还是 SSH 别名，分别配置服务账号的出站代理或 SSH 配置，不能混用。校验证书或主机密钥；Webhook 出站仅放行指定 Bridge 地址。不要修改机器全局代理。

升级前备份数据库、裸仓、配置、密钥与快照；SQLite 使用在线备份或停写后复制。在副本验证 OAuth、模板、API 与 webhook，数据库迁移后需要连同数据一起回滚。独立扩展不等于永远兼容。

参考：[Gitea 配置](https://docs.gitea.com/administration/config-cheat-sheet)、[模板](https://docs.gitea.com/administration/customizing-gitea/)、[Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve)。

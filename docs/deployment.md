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

1. 在 Gitea 注册机密 OAuth2 应用。回调须完整匹配网关地址，例如 `https://git.office.example:8443/bridge/auth/callback`。默认申请 `read:user` 和 `read:repository`，不需要管理员令牌。`enable_pull_creation`、`enable_merge`、`enable_revert` 独立开关、默认关闭；开启任一个都需要 Bridge 数据库和重新登录授权 `write:repository`。这是 Gitea 的仓库级写权限，不是仅限 PR。只在需要写操作的实例启用。
2. 复制 `examples/bridge.json` 到部署目录，填写 Gitea 内部地址、两个浏览器入口、OAuth client ID 及仓库白名单。仓库 `id` 是 Gitea API 返回的数字 ID；删除重建仓库后需确认新 ID，不按同名自动接管。若要保存联调单，先在本机磁盘创建受限的数据目录，再配置 `database_path` 为其中的绝对路径；不要使用共享盘、Gitea 数据库或其他 Gitea 实例的 Bridge 数据库。未设置 `database_path` 时只开放即时查询。
3. 使用 Go 1.26 或以上版本，在仓库目录执行 `go build -o bridge.exe ./cmd/bridge`。从安全的启动环境设置 `BRIDGE_OAUTH_CLIENT_SECRET`，再执行 `.\bridge.exe -config C:\path\to\bridge.json`。默认监听 `127.0.0.1:3001`，通过网关访问 `/bridge/`。不要把密钥写进配置或提交到 Git。
4. 初装先以前台运行，确认配置和回环入口正常，再注册 Windows 服务。会话只保留在内存，最长一小时，进程重启须重新登录。`allow_loopback_http` 仅用于隔离实验，非回环 HTTP 不允许。

跨 Fork 提交需在项目的 `forks` 数组填写目标仓数字 ID 和已存在的直接 Fork（见示例配置）。Fork 不会自动加入目标仓列表。启用建 PR 后，只有能读取目标、能写源仓的用户才会看到该选项；移走配置、删除源仓或撤销其读权限后，Bridge 不再显示对应 PR、联调单或原回执。

### 注册 Windows 服务

在测试机上为 Bridge、Gitea、Caddy 和可选存证服务分别准备低权限账号。Bridge 账号只读程序目录和配置，读取制品，且只对自己的数据库、快照及日志目录有写权限；不得接触 Gitea 裸仓和独立存证目录。Gitea 和 Caddy 分别按 [Gitea 服务文档](https://docs.gitea.com/installation/windows-service/) 与 [Caddy 服务文档](https://caddyserver.com/docs/running#windows-service) 注册，设置自己的账号与目录，不照搬默认 LocalSystem。

为 Bridge 新建只允许管理员和服务账号读取的 JSON 密钥文件，保存 `oauth_client_secret`；按需加 `webhook_secret`、`webhook_reader_token`、`receipt_signing_seed`、`anchor_bearer`。独立存证服务用不同文件，只放 `anchor_signing_seed` 和 `anchor_bearer`。启用 `-secrets` 后只读该文件，不再混用启动环境中的密钥。密钥值不要写入命令行、服务注册表或 Git。

用管理员 PowerShell 注册（前提是目标账号拥有「作为服务登录」权限，目录及 ACL 已就绪）：

```powershell
$credential = Get-Credential -UserName '.\BridgeSvc' -Message '专用低权限账号'
.\examples\Register-BridgeService.ps1 -Name GiteaTopicBridge -Credential $credential `
  -Executable 'C:/Bridge/bin/bridge.exe' -Config 'C:/Bridge/config/bridge.json' `
  -Secrets 'C:/Bridge/config/secrets.json' -LogFile 'C:/Bridge/log/bridge.log' -Listen '127.0.0.1:3001'
Start-Service -Name GiteaTopicBridge
```

命令末尾的反引号表示换行。需要 Webhook 时额外传 `-HookListen '127.0.0.1:3002'`。独立存证改用 `-AnchorDatabase` 替代 `-Config`，另选服务名、账号、密钥文件和端口。注册脚本不会覆盖已有服务，也不会立即启动；可先用 `-WhatIf`。服务以延迟自动启动，失败后依次等 5、30、120 秒再启动；用 `sc.exe qfailure GiteaTopicBridge` 核对。所有监听绑定完成后才报告 Running；停止时不再接新请求并等待后台任务退出。预留日志容量并单独做轮转。回滚注册可先 `Stop-Service -Name GiteaTopicBridge` 再 `sc.exe delete GiteaTopicBridge`，不会删除业务文件。

### 内部 Webhook

启用时，在已有 Bridge 数据库配置下添加 `-hook-listen 127.0.0.1:3002`。启动环境提供两个独立密钥：`BRIDGE_WEBHOOK_SECRET`（至少 32 字节的随机共享密钥）及 `BRIDGE_WEBHOOK_READER_TOKEN`（专用非管理员账号，只授予白名单仓的读取权限，令牌只给 `read:repository`）。两者都不得进入 Git、日志或浏览器。内部回调必须直连 `http://127.0.0.1:3002/internal/hooks/gitea`；不监听公网，不交给 Caddy 或 Tailscale 转发。

在对应仓配置 Gitea 类型的 JSON Webhook，选择 PR 和 Release 事件，填写相同的签名密钥。在 Gitea 的 `[security]` 出站允许列表中增加准确的 `127.0.0.1:3002`，不要为了回调打开全部内网地址。查看 Gitea 投递日志确认收到 `202`；这是持久入队，不是回执签发。遇到 `503` 须从投递历史重试，并检查 Bridge 日志、磁盘及只读账号权限。

### Gitea 菜单

同一入口网关在根路径提供 Gitea、在 `/bridge/` 提供 Bridge 时，将 [菜单片段](../examples/gitea/extra_links.tmpl) 合并到 Gitea 实例的 `$GITEA_CUSTOM/templates/custom/extra_links.tmpl`。有现存片段时先备份，再只追加尚未存在的链接；未启用回执时不添加第二行。不要替换 Gitea 的整个导航模板。重启 Gitea 后检查两条链接；失败可撤下两行、重启后直接从 `/bridge/` 访问。升级前在副本验证此扩展点。

### 交付回执中心与 API

默认关闭。在配置中添加 `"receipts":{"artifact_root":"C:/BridgeInbox","snapshot_root":"C:/BridgeSnapshots","issuers":[42]}`，并配置 Bridge 数据库。用实际 Gitea 账号数字 ID 替换 `42`，仅授权负责交付的用户。两个目录都必须已存在，且位于本机磁盘、互不包含；Bridge 账号对收件目录只读，对快照目录独占写入；不要让其他用户写入或重映射快照目录。上传的制品放在收件根目录下的 `<仓库数字ID>/<相对路径>`，先停止写入再签发。启用后，可从 Bridge 联调单打开「交付回执」，或访问 `/bridge/receipts/` 输入回执 ID。

从安全启动环境设置 `BRIDGE_RECEIPT_SIGNING_SEED` 为 32 字节密码学随机种子的标准 Base64。种子不是密码，必须保密并做离线备份；当前服务只识别一个签发公钥，不支持在线轮换。用 `/bridge/api/receipt-key` 查看公钥与指纹，经独立可信渠道分发，不要用回执自带数据建立信任。离线验签：`.\bridge.exe -verify-receipt C:/path/receipt.json -trusted-receipt-key <可信公钥Base64>`。该命令验 JSON，不验文件；另对快照目录 `<回执ID>/<slot>.blob` 计算 SHA-256，与签名快照的对应项比对。

回执签名不等于独立存证。备份时暂停签发，先停 Bridge，再一起保全 SQLite 和快照目录；断电或数据库写入状态不明可能留下无对应记录的快照目录，人工核对后才可清理。

#### 独立存证服务

在另一个权限边界运行同一二进制的存证模式：`.\bridge.exe -anchor-db D:/Anchor/anchor.db -listen 127.0.0.1:3003`。使用不同的服务账户和独立存储，推荐单独主机与备份账户；Bridge 账户不得读取或写入账本目录，不得访问存证私钥。存证进程从安全环境读取 `BRIDGE_ANCHOR_SIGNING_SEED`（与回执密钥不同的 32 字节随机种子、标准 Base64）及 `BRIDGE_ANCHOR_BEARER`（至少 32 字符的随机共享令牌）。启动日志显示存证服务的公钥。

在 Bridge 的 `receipts` 顶层配置内增加 `"anchor":{"url":"https://anchor.example.com/v1/anchors","public_key":"<可信存证公钥Base64>"}`，只经独立网关转发该 POST 路由，不将存证服务挂在 Gitea 公共网关。使用有效证书，并在 Bridge 的安全环境设置相同的 `BRIDGE_ANCHOR_BEARER`。HTTP 仅允许显式回环 IP，适合本机实验。应用层只支持追加；对能修改存证数据库文件或拥有存证私钥的管理员不承诺防篡改，请隔离运维权限并单独做受保护备份。

独立凭证从 `GET /bridge/api/receipts/{id}?format=proof` 下载，有仓库权限且验证通过才返回。离线核验时为原有验签命令再加 `-anchor-proof receipt.proof.json -trusted-anchor-key <可信存证公钥Base64>`。不要只信 Markdown 状态或放在回执旁边的公钥。

## 运维约束

先确认 github443 是 HTTP 代理还是 SSH 别名，分别配置服务账号的出站代理或 SSH 配置，不能混用。校验证书或主机密钥；Webhook 出站仅放行指定 Bridge 地址。不要修改机器全局代理。

### 上游拉取镜像

1. 用 Gitea 服务账号检查目标上游的 DNS、证书和 `git ls-remote <上游地址> HEAD`。HTTPS 使用该服务的代理环境或 Git 配置；SSH 则检查该账号的密钥、主机指纹和实际端口。使用代理时由代理限制目标地址。直接连接时可在 Gitea v28.4.0 的 `[migrations]` 设置 `EGRESS_MODE = strict` 和 `ALLOWED_HOST_LIST = github.com:443`，按实际 HTTPS 上游替换主机名和端口。参见 [对应版本配置样例](https://github.com/go-gitea/gitea/blob/v28.4.0/custom/conf/app.example.ini)。
2. 在 Gitea 选择「迁移外部仓库」，填写上游 URL、镜像仓名，选中拉取镜像并设置更新间隔。先用公开的小仓验证，私有上游使用仅能读取目标仓的凭据。权威仓单独创建并配置分支保护。参见 [Gitea 镜像文档](https://docs.gitea.com/1.27/usage/repository/repo-mirror/)。
3. 在镜像设置执行一次同步，比对上游与镜像默认分支的 SHA。用具备仓库写权限的普通账号克隆镜像，尝试推送一个新测试分支，确认 Gitea 明确拒绝且该分支没有出现。Bridge 可展示镜像最新提交；镜像只用作上游参考，不放进需要创建或合并 PR 的联调组。

### 升级

升级前备份数据库、裸仓、配置、密钥与快照；SQLite 使用在线备份或停写后复制。在副本验证 OAuth、模板、API 与 webhook，数据库迁移后需要连同数据一起回滚。独立扩展不等于永远兼容。

### 停机备份与空目录演练

1. 在低峰暂停签发和写操作，记录 Gitea、Bridge、存证服务版本及公钥指纹。正常停止 Bridge，再正常停止存证服务；确认所有 SQLite 连接已关闭。不要删除残留的 `-wal`、`-shm` 或 `-journal` 来强行备份；若残留，应先在受控条件下恢复数据库并正常关闭，再重试。另安排 Gitea 停机，用 [Gitea 备份与恢复流程](https://docs.gitea.com/administration/backup-and-restore/) 保全业务库、裸仓、LFS/附件、配置和自定义目录；对外部数据库用该数据库的原生备份。单独保全 Caddy 配置及所需证书材料。
2. 在各自权限边界，用 `examples/Copy-StoppedDatabase.ps1` 复制 Bridge 数据库和快照，以及独立存证账本。目标必须是已建好的、受限 ACL 的空目录；脚本不覆盖已有文件，不复制密钥。受限保存 OAuth、Webhook、签发和存证私钥的对应版本，单独备份服务配置和收件目录。不要将独立账本的备份交给 Bridge 服务账号。
3. 用同一脚本把每份备份再复制到新的隔离空目录。输出固定为 `state.sqlite3` 及可选的 `snapshots/`。保留生产目录原样；脚本报错时，不要使用部分输出。对带回执的备份，用下面的离线核验命令复验；比对签名版本数、制品数、存证数与备份时的记录。未完成的待存证项不等于已存证。若核对独立账本，需由同时获得两个备份副本的独立验收人员执行，不授予 Bridge 账号账本文件权限。

```powershell
.\examples\Copy-StoppedDatabase.ps1 -Database 'C:/Bridge/data/bridge.db' -Snapshots 'C:/Bridge/snapshots' -Destination 'D:/Backup/bridge-copy'
.\examples\Copy-StoppedDatabase.ps1 -Database 'D:/Backup/bridge-copy/state.sqlite3' -Snapshots 'D:/Backup/bridge-copy/snapshots' -Destination 'C:/RestoreLab/bridge'
.\bridge.exe -audit-restored-db 'C:/RestoreLab/bridge/state.sqlite3' -audit-snapshots 'C:/RestoreLab/bridge/snapshots' -trusted-receipt-key $ReceiptPublicKey -trusted-anchor-key $AnchorPublicKey -audit-anchor-db 'C:/RestoreLab/anchor/state.sqlite3'
```

在存证权限边界复制及恢复账本时不传 `-Snapshots`。从独立渠道核对公钥后填入对应变量。没有独立存证的实例省略存证相关参数；若数据库带有存证凭证，不能跳过该凭证验签。离线命令只读检查 SQLite、签名版本链、快照字节和可选账本，不启动服务，也不会迁移旧版库。对旧版备份先保留原件，使用对应版本或迁移另一份副本。通过后再用独立端口和恢复目录启动，重新登录测试，检查仓库 ID 和权限未变、收件及回执可用；最后才安排正式切换，保留原仓和切换记录。

参考：[Gitea 配置](https://docs.gitea.com/administration/config-cheat-sheet)、[模板](https://docs.gitea.com/administration/customizing-gitea/)、[Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve)。

# 服务与网络

Gitea、Go Bridge、Caddy 分别作为 Windows 服务运行。Bridge 原生支持 SCM 启停，内嵌 HTML/CSS/ES Modules。服务使用独立低权限账户，自动启动、失败退避重启。程序目录只读，数据与备份分开。Gitea 独占裸仓；Bridge 只读制品，不读写 Gitea 数据库。

|模式|示例入口|上线条件|
|---|---|---|
|内网 DNS|`https://git.office.example:8443/bridge/`|内网解析，办公网段白名单，终端信任内部 CA|
|域名|`https://git.example.com/bridge/`|有效证书；有域名不代表必须开放公网|
|IP:端口|`https://192.0.2.10:8443/bridge/`|替换文档 IP，证书 SAN 包含实际 IP，防火墙限源|
|Tailscale|`https://git-host.example-tailnet.ts.net:8443/bridge/`|实际 MagicDNS 全名，Serve 终止 TLS，tailnet grants 限制成员，不开 Funnel|

每个实例选一个规范入口。设定 Gitea `[server]` 的 `ROOT_URL` 和 `PUBLIC_URL_DETECTION = never`，并让 Bridge `public_base_url`、OAuth 回调与克隆地址使用同一入口。Gitea `LOCAL_ROOT_URL` 可单独设为内部回环地址。这样内网 API 请求也会得到规范克隆地址。多网共用优先选同一域名配分区 DNS；其他别名只有在规范地址可达时才跳转，且别名本身也需要有效证书。

后端仅绑定 127.0.0.1:3000、127.0.0.1:3001，默认 Git over HTTPS。外部网关不转发内部 webhook，不从转发头推断身份。

使用 [Caddyfile](../examples/Caddyfile) 时，内网 DNS 把站点名换成实际内网域名，终端信任该内部 CA。IP 入口把站点名换成实际 IP 和端口，证书 SAN 必须包含该 IP。使用外部域名证书时，按签发方式配置自动 HTTPS 或提供证书与私钥路径。用 Windows Git 实测克隆，客户端还须能访问企业 CA 所需的吊销信息。路由配置保留 `handle @private`，使内部路径先由网关拦下。

Bridge 用 Gitea OAuth2 授权码、PKCE 与 state。固定回调 `/bridge/auth/callback`，令牌只保存在服务端；会话 Cookie 使用 Secure、HttpOnly、SameSite=Lax，写请求验证 CSRF。复用已有 Gitea 登录，不复制 Gitea 会话密钥；首次可能要求授权。

### Tailscale Serve

选择 [Caddyfile.tailnet](../examples/Caddyfile.tailnet) 后，Caddy 只在 `127.0.0.1:8080` 接收 HTTP，Gitea 和 Bridge 的规范入口填写真实的 `https://节点名.实际-tailnet.ts.net:8443` 和同地址的 `/bridge/`。OAuth 回调也必须用这个地址。启用 tailnet HTTPS，配置 grants 只允许指定成员访问该节点的 `tcp:8443`，确认端口未被现有 Serve 配置占用后，在目标机运行：

```powershell
tailscale serve --bg --https=8443 http://127.0.0.1:8080
tailscale serve status
```

从另一台已授权的 tailnet 客户端核对登录回跳、证书、页面链接与 Git 克隆。在拒绝访问的客户端确认无法连接该入口，并核对 `3000`、`3001`、`8080` 都没有对网络开放。此模式使用 Serve。参见 [Tailscale Serve 官方文档](https://tailscale.com/docs/reference/tailscale-cli/serve)。

### 从客户端检查入口

在不同网络的测试客户端使用 PowerShell 7.4 或以上版本运行 [Test-BridgeIngress.ps1](../examples/Test-BridgeIngress.ps1)。先从已授权客户端验证入口在线，记录其实际入口 IP，再从需要拒绝的客户端测试同一入口 IP。将样例域名和 IP 替换为该客户端实际应连接的入口和后端主机地址。检查多个 IP 时传入数组。后端端口与部署配置一致，启用 Webhook 时加 `3002`，使用 Tailscale 回环网关时加 `8080`。

```powershell
# 在授权客户端执行
.\examples\Test-BridgeIngress.ps1 -BridgeUrl 'https://git.office.example:8443/bridge/' `
  -ExpectedAccess Allowed -SourceLabel 'office-allowed' -BackendAddress '192.0.2.10' `
  -BackendPorts 3000,3001,3002 > ingress-office.json

# 在拒绝访问的客户端执行。入口 IP 由前面的授权测试确认。
.\examples\Test-BridgeIngress.ps1 -BridgeUrl 'https://git.office.example:8443/bridge/' `
  -ExpectedAccess Denied -SourceLabel 'guest-denied' -IngressAddress '192.0.2.10' `
  -BackendAddress '192.0.2.10' -BackendPorts 3000,3001,3002 > ingress-guest.json
```

授权模式直连规范 HTTPS 入口，检查证书和三个状态码。它对内部 Webhook 发一个无签名、空请求体的探针，预期由网关返回 `404`。拒绝模式直接检查所给入口 IP 的 TCP 连通性，避免把 DNS 或证书错误当成访问限制。两种模式都检查列出的后端端口，逐项写出 JSON，失败时以非零状态退出。默认使用客户端系统信任库。隔离实验可用 `-TrustedCAFile` 提供当前探针专用的 PEM CA，继续验证证书链和主机名。OAuth 回跳、私有仓 Git 克隆和实际客户端的吊销检查仍需分别操作核对。

## 启动看板

1. 在 Gitea 注册机密 OAuth2 应用。回调须完整匹配网关地址，例如 `https://git.office.example:8443/bridge/auth/callback`。默认申请 `read:user` 和 `read:repository`，不需要管理员令牌。`enable_pull_creation`、`enable_merge`、`enable_revert` 独立开关、默认关闭；开启任一个都需要 Bridge 数据库和重新登录授权 `write:repository`。这是 Gitea 的仓库级写权限，不是仅限 PR。只在需要写操作的实例启用。
2. 复制 `examples/bridge.json` 到部署目录，填写 Gitea 内部地址、两个浏览器入口、OAuth client ID 及仓库白名单。仓库 `id` 是 Gitea API 返回的数字 ID；删除重建仓库后需确认新 ID，不按同名自动接管。若要保存联调单，先在本机磁盘创建受限的数据目录，再配置 `database_path` 为其中的绝对路径；不要使用共享盘、Gitea 数据库或其他 Gitea 实例的 Bridge 数据库。未设置 `database_path` 时只开放即时查询。
3. 使用 Go 1.26 或以上版本，在仓库目录执行 `go build -o bridge.exe ./cmd/bridge`。从安全的启动环境设置 `BRIDGE_OAUTH_CLIENT_SECRET`，再执行 `.\bridge.exe -config C:\path\to\bridge.json`。默认监听 `127.0.0.1:3001`，通过网关访问 `/bridge/`。不要把密钥写进配置或提交到 Git。
4. 初装先以前台运行，确认配置和回环入口正常，再注册 Windows 服务。会话只保留在内存，最长一小时，进程重启须重新登录。`allow_loopback_http` 仅用于隔离实验，非回环 HTTP 不允许。

跨 Fork 提交需在项目的 `forks` 数组填写目标仓数字 ID 和已存在的直接 Fork（见示例配置）。Fork 不会自动加入目标仓列表。启用建 PR 后，只有能读取目标、能写源仓的用户才会看到该选项；移走配置、删除源仓或撤销其读权限后，Bridge 不再显示对应 PR、联调单或原回执。

### 注册 Windows 服务

在测试机上为 Bridge、Gitea、Caddy 和可选存证服务分别准备低权限账号。Bridge 账号只读程序目录和配置，读取制品，且只对自己的数据库、快照及日志目录有写权限；不得接触 Gitea 裸仓和独立存证目录。Gitea 和 Caddy 分别按 [Gitea 服务文档](https://docs.gitea.com/installation/windows-service/) 与 [Caddy 服务文档](https://caddyserver.com/docs/running#windows-service) 注册，设置自己的账号与目录，不照搬默认 LocalSystem。

Gitea 的服务命令带绝对 `--work-path`、`--config`，SQLite、裸仓和日志使用该账号可写的绝对路径。用服务账号检查 Git；如系统 PATH 与管理员终端不同，在 Gitea `[git]` 的 `PATH` 填入 `git.exe` 的绝对路径。Caddy 的服务命令用 `run --config <绝对路径> --adapter caddyfile`，并为证书存储指定该账号可写的目录。复验顺序是先 Gitea，再 Bridge 和网关，最后从客户端验证 Git HTTPS 与 OAuth。

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

`-LogFile` 使用独立日志目录中的绝对路径。程序启动时会拒绝把配置、密钥、存证 CA、数据库和 SQLite 附属文件当成日志，也会拒绝在收件与回执快照目录内写日志。遇到路径校验失败时先检查前台启动输出或服务启动状态，再修正路径后重试。

### 在目标主机检查开机自启

用管理员 PowerShell 7.4 或以上版本运行[主机探针](../examples/Test-BridgeWindowsServices.ps1)。将下面的服务名、专用账号和端口换成实际配置。启用 Webhook 时给 Bridge 的 `Ports` 加上内部端口 3002。用 Tailscale Serve 时按实际配置填写 Caddy 回环端口，并为 Caddy 加上 `LoopbackOnly=$true`。独立存证按它所在的主机单独检查，使用该机的服务名和端口。

```powershell
$services = @(
    @{ Name='Gitea'; Account='.\GiteaSvc'; Ports=3000; LoopbackOnly=$true }
    @{ Name='GiteaTopicBridge'; Account='.\BridgeSvc'; Ports=3001; LoopbackOnly=$true; DelayedAutoStart=$true }
    @{ Name='Caddy'; Account='.\CaddySvc'; Ports=443 }
)

# 在计划重启前，保存同一台机器的基线
.\examples\Test-BridgeWindowsServices.ps1 -Services $services > services-before.json

# 整机重启后，在同一台机器重新定义上面的 $services，待服务自行启动后采集
.\examples\Test-BridgeWindowsServices.ps1 -Services $services -BeforeRestart .\services-before.json > services-after.json
```

报告记录开机时间、服务状态、自动启动设置、注册账号和运行进程的账号，以及该进程实际监听的 TCP 地址与端口。带基线运行时，当前开机时间必须比基线更新。任何一项未通过时，命令以非零状态退出。验收期间让 SCM 自行启动各服务，再从目标网络完成[客户端探针](#从客户端检查入口)、OAuth 登录和 Git 克隆。

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

独立网关可使用 [Caddyfile.anchor](../examples/Caddyfile.anchor)。样例用内部 CA，部署时替换主机名和端口，按网络策略只允许 Bridge 访问。使用系统已信任的证书时，保留默认的系统信任库。使用独立内部 CA 时，在 `anchor` 中额外配置 `"ca_file":"C:/Bridge/config/anchor-ca.pem"`，填写只读的本机 PEM CA 文件。Bridge 对存证 HTTPS 仅使用这份证书包，并继续校验主机名。更新 CA 后重启 Bridge；`public_key` 仍须填独立 Ed25519 存证公钥，用来验证凭证签名。

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

1. 在低峰暂停签发和写操作，记录 Gitea、Bridge、存证服务版本及公钥指纹。正常停止 Bridge，再正常停止存证服务。Windows 显示服务已停止后，还要等待原进程退出，确认 SQLite 已关闭再复制。不要删除残留的 `-wal`、`-shm` 或 `-journal` 来强行备份；若残留，应先在受控条件下恢复数据库并正常关闭，再重试。另安排 Gitea 停机，用 [Gitea 备份与恢复流程](https://docs.gitea.com/administration/backup-and-restore/) 保全业务库、裸仓、LFS/附件、配置和自定义目录；对外部数据库用该数据库的原生备份。单独保全 Caddy 配置及所需证书材料。
2. 在各自权限边界，用 `examples/Copy-StoppedDatabase.ps1` 复制 Bridge 数据库和快照，以及独立存证账本。目标必须是已建好的、受限 ACL 的空目录；脚本不覆盖已有文件，不复制密钥。受限保存 OAuth、Webhook、签发和存证私钥的对应版本，单独备份服务配置和收件目录。不要将独立账本的备份交给 Bridge 服务账号。
3. 用同一脚本把每份备份再复制到新的隔离空目录。输出固定为 `state.sqlite3` 及可选的 `snapshots/`。保留生产目录原样；脚本报错时，不要使用部分输出。对带回执的备份，用下面的离线核验命令复验；比对签名版本数、制品数、存证数与备份时的记录。未完成的待存证项不等于已存证。若核对独立账本，需由同时获得两个备份副本的独立验收人员执行，不授予 Bridge 账号账本文件权限。
4. Gitea 的 SQLite 冷备在正常停止且没有残留日志后，复制数据库、裸仓、应用数据、自定义目录和实际 `app.ini`。恢复到隔离空目录，使用新的测试入口，逐一改写配置中的路径和端口，保全原密钥。移动 Gitea 目录后，用恢复配置执行 `gitea --work-path <恢复目录> --config <恢复的app.ini> admin regenerate hooks`。用测试协作者登录并克隆，核对仓库数字 ID、默认分支 SHA、保护规则，再在允许的测试分支验证推送生效。如果使用 LFS 或发布附件，从新克隆重新下载 LFS 文件，登录后下载附件，与备份前记录的 SHA-256 比较；如果使用外部对象存储，也要恢复对应存储。生产路径切换前保持源实例和备份可用。参见 [Gitea 恢复流程](https://docs.gitea.com/administration/backup-and-restore/)。

```powershell
.\examples\Copy-StoppedDatabase.ps1 -Database 'C:/Bridge/data/bridge.db' -Snapshots 'C:/Bridge/snapshots' -Destination 'D:/Backup/bridge-copy'
.\examples\Copy-StoppedDatabase.ps1 -Database 'D:/Backup/bridge-copy/state.sqlite3' -Snapshots 'D:/Backup/bridge-copy/snapshots' -Destination 'C:/RestoreLab/bridge'
.\bridge.exe -audit-restored-db 'C:/RestoreLab/bridge/state.sqlite3' -audit-snapshots 'C:/RestoreLab/bridge/snapshots' -trusted-receipt-key $ReceiptPublicKey -trusted-anchor-key $AnchorPublicKey -audit-anchor-db 'C:/RestoreLab/anchor/state.sqlite3'
```

在存证权限边界复制及恢复账本时不传 `-Snapshots`。从独立渠道核对公钥后填入对应变量。没有独立存证的实例省略存证相关参数；若数据库带有存证凭证，不能跳过该凭证验签。离线命令只读检查 SQLite、签名版本链、快照字节和可选账本，不启动服务，也不会迁移旧版库。对旧版备份先保留原件，使用对应版本或迁移另一份副本。通过后再用独立端口和恢复目录启动，重新登录测试，检查仓库 ID 和权限未变、收件及回执可用；最后才安排正式切换，保留原仓和切换记录。

### 非关键项目试接

1. 选一个非关键仓，记录源仓位置、默认分支、分支和标签的对象 ID。为导入创建独立临时克隆，从空的私有 Gitea 仓开始。对纯 Git 历史可用 `git clone --mirror <旧仓地址> <临时目录>`，在临时目录用 `git push --atomic <新仓地址> 'refs/heads/*:refs/heads/*' 'refs/tags/*:refs/tags/*'` 复制分支和标签。旧仓和旧目录保留原位。若有 LFS、PR、发布、Wiki 或其他平台数据，按实际源和目标选择 Gitea 迁移方式，并逐项核验。
2. 用 `git ls-remote --refs <新仓地址>` 对比分支和标签对象，检查默认分支，设置主干保护和成员权限。将新仓的数字 ID 加入 Bridge 试点项目组。由普通开发者创建测试分支和带 Topic 的 PR，用独立只读账号核对 Bridge 中的仓名、PR 号、head SHA 和检查状态。
3. 记录切换时间、执行人、源与目标 URL、分支和标签比对结果、Gitea 仓 ID、试点 PR、验收结果与回退负责人。试点通过后才逐批导入。切换窗口冻结旧仓写入并再核一遍对象；已写入新仓的提交先保全，回退入口后再处理待同步改动。

参考：[Gitea 配置](https://docs.gitea.com/administration/config-cheat-sheet)、[模板](https://docs.gitea.com/administration/customizing-gitea/)、[Tailscale Serve](https://tailscale.com/docs/reference/tailscale-cli/serve)。

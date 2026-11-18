# 实施顺序与上线门槛

以下 1–4 是完整上线门槛，尚未全部通过。已通过的局部测试单独记录在末尾。

1. **底座**：干净 Windows 测试机，校验官方二进制，配置专用账户、回环监听、SQLite 和网关。重启后验证服务自动恢复。通过指定出站路径建立一个公开测试镜像，普通开发者推送镜像必须失败。
2. **后端**：三个合成仓建同名 Topic，逐项比对 API 的 PR、head SHA、CI 与冲突。直接推送受保护主干须失败；无权用户不能借 Bridge 合并或查看回执。重复、乱序与伪造 webhook 分别验证去重、回查与拒绝。
3. **网页**：从 Gitea 菜单打开总览、联调、回执中心；验证 OAuth 返回原页。分别从内网、域名、IP:端口、tailnet 客户端检查证书、登录回调、克隆 URL 和网页链接。非授权网络不能直连后端。模拟三仓两成功一冲突，网页不得显示全部成功；检查回滚 PR 仍需审批。
4. **交付与迁移**：对同一制品独立计算 SHA-256，检查 JSON、Markdown 与签名一致；篡改副本须验签失败。在空目录恢复备份并重复核验。先接一个非关键项目，通过后才分批导入；保留旧仓和切换记录，不搬动历史目录。

## 已验证：本地只读查询

使用 Windows amd64 的 Gitea v28.4.0，测试前将二进制的 SHA-256 与该版本 GitHub 发布资产的摘要核对。配置 Git 后，可在 PowerShell 中复现：

```powershell
$env:BRIDGE_TEST_GITEA_BINARY = 'C:\path\to\gitea-28.4.0-windows-amd64.exe'
go test -v -timeout 3m -run '^TestGiteaLocalLab$' -count=1 ./...
```

测试创建临时回环实例和 SQLite 数据库，自动生成测试账号及令牌，运行后停止实例。一个账号建立四个私有测试仓，Bridge 查询使用另一个普通账号的授权。验证已合并和开放 PR、同仓多个匹配、分页上限、相近编号不误匹配、不可见仓过滤，以及撤销仓库权限后不再返回该仓。

同一测试还提交真实 Gitea 登录和 OAuth 授权表单，经回调进入 Bridge，在会话中查询私有仓，再退出并确认 API 返回 `401`。测试为开放 PR 提交合成的 CI 成功/失败状态和一条正式批准，核对返回的 CI、批准数量、变更文件数。有冲突的样例与无冲突的样例呈现不同状态，`mergeable=false` 显示为暂不可合并、原因未知。真实 Runner 验收见下一节。

## 已验证：真实 Runner 的 CI 结果

在 Windows 回环实例启动 Gitea Runner v6.0.0，注册范围是一个私有合成仓。两条分支触发同一条 PowerShell 工作流，分别成功和失败，普通只读账号通过 Bridge 在两个 PR 的 head SHA 上读到对应的状态。工作流只执行测试内定义的命令，未从外部加载 Actions。生产仓的工作流、Runner 隔离、出站和必跑检查名称须按实际项目复验。Runner 二进制来自 [v6.0.0 发布目录](https://dl.gitea.com/gitea-runner/6.0.0/)，Windows amd64 文件的 SHA-256 为 `d85ac9d2a2814704836c808a7ad166a57a81f6b9e1e3afc7d3eda798540062a3`。

```powershell
$env:BRIDGE_TEST_GITEA_BINARY = 'C:\path\to\gitea-28.4.0-windows-amd64.exe'
$env:BRIDGE_TEST_RUNNER_BINARY = 'C:\path\to\gitea-runner-6.0.0-windows-amd64.exe'
go test -v -timeout 3m -run '^TestGiteaRunnerLab$' -count=1 ./...
```

## 已验证：项目总览与保护巡检

真实 Gitea 切换默认分支保护前后核对最新提交 SHA、不安全规则提示，以及普通账号和仓库管理员看到的检查范围。管理员能看到允许推送、未指定评审范围与审批绕过等问题；普通账号无法查看完整规则。撤销一个私有仓权限后，总览立即滤掉该仓；整组不可见与不存在都返回相同的 `404`。另在隔离 Edge 中用模拟接口验证总览页面、提交链接和未知提示。

隔离 Edge 验证切换项目组或修改需求编号后会清除上一组联调结果并隐藏保存按钮。重新查询时使用新选择的项目。

## 已验证：保存联调单

`go test ./...` 验证 Bridge 使用 SQLite WAL，关闭并重开数据库仍能读取原联调单，失败事务不会留下半张单，且不会改写非 Bridge 数据库。上述 Gitea 联调还验证普通账号建单、重复提交返回同一张单、同键不同内容报冲突、拒绝跨站写入，以及仓库权限撤销后无法读取原单或用原键重试。空目录恢复和 Windows 服务的验收范围见下方对应记录。

## 已验证：同仓分支批量建 PR

本地 Gitea 联调用普通写权限账号验证同批次中新建 PR、复用已有 PR、以及源分支不存在导致的失败分别记录；再次提交保留已完成结果，只读账号不能写入。模拟 HTTP 测试验证请求超时后重开同一数据库，回查为空不重发请求，原 PR 稍后出现（即使已合并）可通过操作标识恢复关联。

## 已验证：公开上游拉取镜像

Windows 回环 Gitea v28.4.0 继承实验会话的 HTTPS 出站设置，从 [octocat/Hello-World](https://github.com/octocat/Hello-World) 建立私有 Pull Mirror。确认镜像有原始提交、Bridge 总览与 Git 克隆的 SHA 一致，手动请求同步后 Gitea 更新最后同步时间。普通协作者有仓库写权限，但推送新分支收到镜像只读错误，随后查询远端确认该分支未出现。生产服务账号的 github443 路径仍须在目标机器复验。

```powershell
$env:BRIDGE_TEST_GITEA_BINARY = 'C:\path\to\gitea-28.4.0-windows-amd64.exe'
$env:BRIDGE_TEST_PUBLIC_MIRROR = '1'
go test -v -timeout 3m -run '^TestGiteaPullMirrorLab$' -count=1 ./...
```

## 已验证：跨 Fork 建 PR

本地 Gitea 用普通开发者账号从改过名的私有 Fork 向只读目标仓提交 PR，验证源和目标可用同名分支、新联调单可复用原 PR、名单内的不相关仓无法充当 Fork。作者不能合并。对源仓只读、对目标仓有写权限的维护者看到同一张联调单，取得指定评审批准与必跑检查结果后合并成功；Bridge 记录的合并 SHA 和执行人与 Gitea 一致。删除源 Fork 后，联调单和原请求的幂等重放均返回 `404`，Topic 查询也会过滤原 PR。

隔离 Edge 使用模拟接口点击保存，确认网页能选择 Fork、用两端同名分支提交，并显示新联调单链接。模拟 API 测试补充覆盖源仓权限不足和来源未核实的过滤。

## 已验证：逐仓合并

相同的本地 Gitea 测试使用普通协作者的 OAuth 会话，对三个私有仓逐个确认：两个通过审批后成功，一个有冲突仍受阻。实际验证了错误提交 SHA、未批准、降为只读、跨站请求、缺少必跑检查均不能合并；向受保护分支直接写文件的 API 操作也被拒绝。用同一普通协作者和同一提交执行真实 Git HTTP 推送，允许的测试分支收到提交，`main` 被 Gitea 分支保护拒绝，且远端 SHA 保持不变。两个成功结果的合并 SHA 和执行人与 Gitea 一致。模拟测试另验证未受保护的目标不会收到合并请求，以及请求超时后换联调单、重开数据库都不会重复提交，稍后可核对已完成的结果。未验证生产网络中的 Git HTTPS。

## 已验证：反向 PR

本地 Gitea 测试验证目标分支前进后拒绝自动准备，也不会新建分支；符合条件时用当前协作者建反向 PR，创建与重试不改主干、不重复开单。给反向 PR 补齐 CI 后，仍须另行人工批准才能合并；审批后由 Gitea 合并，验证原 PR 新增文件已撤回。模拟测试覆盖两阶段超时、数据库重开后回查、只读核对与恢复原 PR 关联。

隔离 Edge 使用三仓模拟响应，网页汇总展示「已核验合并 2/3 仓，受阻 1 仓」，受阻仓有对应标记。反向 PR 链接明确指向 Gitea 审批页。

## 已验证：内部 Webhook

本地 Gitea v28.4.0 实际向独立回环入口投递 PR 与 Release 事件；只读协作者令牌回查得到合并 SHA、带注解的斜杠标签所指提交、执行人及时间。模拟测试覆盖无效签名、非回环请求、仓库白名单、重复 UUID、同 UUID 内容不一致、早到/迟到事件及租约中断后数据库重开重试。没有启用生产网关入口。

## 已验证：签名回执 API

本地 Gitea 测试用普通 OAuth 协作者签发回执：同一单分别记录合并提交、带注解的发布标签所指提交和未验证仓。独立计算制品字节的 SHA-256；比较 JSON、Markdown 和签名；更正关联旧版摘要但不覆盖旧版；无效更正与重复首版被拒绝；撤销任一成员仓权限后，各格式和幂等重放都不可读取。修改 JSON 快照后验签失败。Windows 文件测试覆盖输入更新后快照不变，以及拒绝越界、硬链接和目录联接点。

## 已验证：独立存证协议与重试

本地集成测试通过独立 HTTP 入口、签名密钥及账本数据库获取存证凭证，并将其与签名回执一起离线核验；撤权后凭证下载同样返回 `404`。故障测试模拟账本写入后丢失成功响应，Bridge 数据库重开后重新取回原凭证；验证摘要替换、篡改凭证和通过应用账本连接修改旧记录均失败。不同服务账户的 ACL、独立主机 HTTPS 和存储管理员权限隔离仍须部署验收。

另用真实 Caddy 验证独立 HTTPS 网关。网关只转发 `POST /v1/anchors`。没有信任临时 CA 或凭据错误时，账本保持空白。配置正确 CA 与凭据后，Bridge 存证客户端收到可离线验签的凭证，重复提交返回相同字节。测试使用独立回环监听和临时 CA。

```powershell
$env:BRIDGE_TEST_CADDY_BINARY = 'C:\path\to\caddy.exe'
go test -v -run '^TestIndependentAnchorHTTPS$' -count=1 ./...
```

## 已验证：网页回执中心

使用临时 Edge 配置和模拟 HTTP 接口验证详情、未核验标记、版本列表、更正入口与下载链接。另实测填入制品路径、发布 ID 和 SHA，点击确认签发，验证提交内容和幂等键，随后跳到新回执并显示「独立存证待确认」。真实 Gitea 另验证签发、OAuth 读取两版历史和各格式，撤权后历史同样返回 `404`。生产环境的签发与独立存证待现场验收。

```powershell
# 签发跳转测试使用 Node.js 内置 WebSocket（本次使用 v26.13.0）。
$env:BRIDGE_TEST_EDGE_BINARY = 'C:\path\to\msedge.exe'
go test -v -run '^Test(PartialMerge|ReceiptIssue)Edge$' -count=1 ./...
```

## 已验证：Gitea 菜单扩展点

本地 Gitea v28.4.0 在临时自定义模板目录加载示例，仅增加「多仓联调」和「交付回执」两条链接。HTTP 测试确认实际首页各渲染一次并指向正确路径。HTTPS 网关的本地 OAuth 联调见下方记录。

## 已验证：HTTPS 网关回环演练

使用 Windows amd64 的 Gitea v28.4.0 和 Caddy v2.11.9，在临时目录分别以 `git.office.example:随机端口` 和 `127.0.0.1:随机端口` 运行网关配置。验证 Gitea 首页两条菜单、普通账号 OAuth 回到原联调查询、Secure 会话、Bridge 提交链接和 Gitea 克隆地址都使用规范入口。用同一普通账号成功克隆私有仓，`/internal/hooks/gitea` 由网关直接返回 `404`。

从同一 HTTPS 入口保存联调单并签发回执，验签并与制品 SHA-256 比对。清空客户端会话后，原回执接口返回 `401`。重新经过 Gitea OAuth 回到同一条回执及联调单，签名 JSON 仍能用原公钥核验，Markdown 指向同一 Topic。

测试只在客户端进程内信任临时 CA。Windows Git 保留证书链和主机名校验，因该临时 CA 没有吊销端点，单条克隆命令关闭 Schannel 吊销检查。没有修改 Windows 信任库或 Git 全局配置。实际办公网 DNS、公网域名证书、跨主机访问、tailnet 与防火墙仍须在部署环境验证。Caddy 二进制来自 [v2.11.9 发布包](https://github.com/caddyserver/caddy/releases/tag/v2.11.9)，Windows amd64 ZIP 的 SHA-256 为 `ad93112d335ee687100c4c9f11bced16ca590377976b7fe7883882b9e797e472`。

```powershell
$env:BRIDGE_TEST_GITEA_BINARY = 'C:\path\to\gitea-28.4.0-windows-amd64.exe'
$env:BRIDGE_TEST_CADDY_BINARY = 'C:\path\to\caddy.exe'
go test -v -timeout 3m -run '^TestGiteaGatewayTLS$' -count=1 ./...
```

## 已验证：Tailscale Serve 后的 HTTP 回环路由

使用真实 Caddy v2.11.9 与两个合成上游，验证 [回环网关样例](../examples/Caddyfile.tailnet) 将原始 Git 查询路径交给 Gitea、将 OAuth 回调及查询参数交给 Bridge，且内部 Webhook 由网关直接返回 `404`。`/bridge` 会跳转到 `/bridge/`。真实 Serve 证书、MagicDNS 和 grants 仍须由指定 tailnet 的客户端验收。

```powershell
$env:BRIDGE_TEST_CADDY_BINARY = 'C:\path\to\caddy.exe'
go test -v -run '^TestCaddyTailnetLoopback$' -count=1 ./...
```

## 已验证：客户端入口验收脚本

在 Windows 上用真实 Caddy 和隔离上游，运行 [客户端探针](../examples/Test-BridgeIngress.ps1)。可信 CA 的 HTTPS 入口通过，移除信任就失败。模拟放开后端或错误开放拒绝入口时，探针输出失败项并以非零状态退出。关闭连接的回环地址用于模拟拒绝，实际办公网和 tailnet 的网络限制须从对应客户端采证。运行方法见[部署说明](deployment.md#从客户端检查入口)。

## 已验证：Windows 服务模式的本地测试

Windows 构建和测试验证 SCM 处理器的启动、询问、停止、关机及失败状态，回环 HTTP 监听在取消时释放端口，后台任务完成后才退出；占用任一个端口不会报告就绪。当前工作机通过 `-WhatIf` 验证两种注册参数。

用临时配置、密钥、证书和数据库路径验证日志路径检查。即使日志通过硬链接指向密钥，也会拒绝启动。数据库及附属文件不会被创建或追加，正常日志可连续追加两次。

## 已验证：Windows 虚拟机真实服务注册和进程故障恢复

在一次性 Windows Server 2025 VM，用实际部署脚本为 Bridge 和独立存证分配两个专用本地账号并注册服务。SCM 记录的服务身份、延迟自动启动和三段恢复延迟符合配置。两个服务在回环实际监听并响应请求。每个服务账号能读取自己的密钥文件，读取对方的密钥文件会收到拒绝。强制结束测试 Bridge 后，SCM 拉起不同 PID 的服务并恢复 HTTP；随后用 SCM 正常停止两个服务。[通过的 GitHub Actions 记录](https://github.com/kongliuli/gitea-topic-bridge/actions/runs/44381792177)

随后在同类 VM 加入 Gitea v28.4.0 和 Caddy v2.11.9，下载后校验 SHA-256。四个服务分别以专用账号启动并绑定回环。普通 Gitea 协作者从真实 Caddy HTTPS 入口完成 OAuth，保存联调单，核对私有仓的 Git HTTPS 引用和 Bridge 项目总览指向同一提交。匿名账号看不到私有仓，外部网关拒绝内部 Webhook，四个服务都接受 SCM 停止。[完整链路运行记录](https://github.com/kongliuli/gitea-topic-bridge/actions/runs/44383280169)

增强验收用 Windows 服务登录令牌验证实际文件权限。Bridge 能读取收件制品，无法写入制品及自身配置，也无法打开 Gitea 数据库或独立存证账本。普通签发者在 HTTPS 入口签发该制品，回执 SHA-256 与原件匹配，独立进程的凭证与回执一起通过离线验签。Git HTTPS 也完成了私有仓克隆，克隆 SHA 与 Bridge 总览一致。[权限与交付验收记录](https://github.com/kongliuli/gitea-topic-bridge/actions/runs/44385352523)

可在 GitHub Actions 手动运行「Windows service lab」复验。目标主机的整机重启、实际账户和业务路径 ACL、远程网络与项目试接仍须现场验收。

## 已验证：Bridge 与独立账本空目录恢复

Windows 集成测试从真实 Gitea 签发两版回执后，干净关闭 Bridge 和存证账本，再用部署脚本分别复制到备份目录、从备份恢复到新的空目录。离线核验两版签名、版本链、两份制品 SHA-256 和两份独立凭证；恢复库返回与原导出相同的回执字节和凭证。验证核验命令不修改数据库、同长度制品损坏会被发现，目标非空或 SQLite 有残留日志时拒绝复制。

## 已验证：真实进程中断后恢复待存证回执

用同一 Windows 二进制分别启动 Bridge 和独立存证模式，两者使用不同的临时密钥文件和数据库。存证进程未启动时，普通 Gitea 协作者成功签发，凭证下载返回 `409`，Bridge 日志记录待重试。强制结束测试 Bridge 进程后，启动存证，再用原库启动 Bridge。客户端重新登录，找到同一回执，下载并离线验签。原签名 JSON 字节保持一致。再次重启存证进程后，重放原请求，返回的凭证字节也与首次一致。本项使用前台回环进程；SCM 自动恢复和 Windows 账户隔离仍须现场验证。

```powershell
go build -o "$env:TEMP\bridge-recovery-lab.exe" ./cmd/bridge
$env:BRIDGE_TEST_BRIDGE_BINARY = Join-Path $env:TEMP 'bridge-recovery-lab.exe'
$env:BRIDGE_TEST_GITEA_BINARY = 'C:\path\to\gitea-28.4.0-windows-amd64.exe'
go test -v -timeout 3m -run '^TestBridgeProcessAnchorRecoveryLab$' -count=1 ./...
```

## 已验证：Gitea 裸仓与 SQLite 空目录恢复

用 Gitea v28.4.0 的 `manager shutdown` 正常停止隔离实例并等待进程退出，确认 SQLite 日志已经关闭。将裸仓、应用数据、自定义目录及配置复制到备份目录，再恢复到另一个空目录。调整目录和回环端口并重建 Git hooks 后，原普通协作者可使用原权限克隆私有仓。仓库数字 ID、默认分支 SHA、保护规则及克隆文件内容与原实例一致。恢复后推送获准的新分支，Gitea API 可读取到新分支和正确 SHA。

另启用本地 Git LFS 和发布附件。恢复后，从新克隆下载原 LFS 对象，由普通协作者登录下载原发布附件，两份文件的 SHA-256 均与备份前一致。外部数据库、对象存储、证书和服务账户权限仍须按目标环境的启用范围验收。

```powershell
$env:BRIDGE_TEST_GITEA_BINARY = 'C:\path\to\gitea-28.4.0-windows-amd64.exe'
$env:BRIDGE_TEST_GITEA_RESTORE_ASSETS = '1' # 同时验证 LFS 和附件，需安装 git-lfs
go test -v -timeout 3m -run '^TestGiteaColdRestoreLab$' -count=1 ./...
```

## 已验证：合成旧仓试点导入

从合成的旧裸仓克隆到临时目录，再将两个分支和一个带注解的标签推到新的空私有 Gitea 仓。逐项核对对象 ID，确认旧裸仓引用保持原样且通过 `git fsck`。配置新仓主干保护后创建一条试点 PR，普通只读账号通过 Bridge 读到与 Gitea 一致的 PR 编号和 head SHA。测试日志记录新仓 ID 和试点 PR；真实试点和按批切换待现场执行。

```powershell
$env:BRIDGE_TEST_GITEA_BINARY = 'C:\path\to\gitea-28.4.0-windows-amd64.exe'
go test -v -timeout 3m -run '^TestGiteaPilotMigrationLab$' -count=1 ./...
```

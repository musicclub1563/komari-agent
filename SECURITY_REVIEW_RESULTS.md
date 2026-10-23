# Komari-Agent 安全复核结果

- 复核对象：`github.com/komari-monitor/komari-agent`
- 复核基准：`a51819e`（工作区含本次未提交修改）
- 复核日期：2026-10-23
- 对照报告：`audit_report_Agent审计-Komari-Agent_2026-10-23.md`（DeepAudit，23 项）

结论摘要：原报告 23 项中，**2 项 CRITICAL 与 1 项 HIGH（HIGH-1）均建立在“Agent 对外提供 WebSocket 端点”这一错误前提上**；Agent 全仓无任何监听代码（`ListenAndServe` / `net.Listen` / `http.Server{` 命中数为 0），它是主动外连的 WebSocket 客户端。这些功能属预期远程管理能力，真正的授权边界在服务端（跨仓库）。本仓库内确认并修复的是：**上传会话与临时文件永久残留**、**token 文件权限过宽**，以及 6 项并发正确性问题。

---

## 1. 原报告 23 项逐项映射与结论

| 编号 | 原报告标题 | 结论 | 主要证据 | 处置 |
|---|---|---|---|---|
| CRITICAL-1 | `agent.exec` 直接执行系统命令 | **不成立为 Agent 侧漏洞**（前提错误）；命令不过滤属实，属设计内远程执行 | `server/websocket.go:384-394`、`server/task.go:25-40`、`server/task.go:73-105` | 本地开关已生效；授权链跨仓库待核实 |
| CRITICAL-2 | `agent.file` 路径无根目录限制 | **不成立为独立漏洞**；无根白名单属实，属设计内主机文件管理 | `server/files.go:99-137`、`server/files.go:912-925` | 本地开关已生效；破坏性缺陷前轮已修 |
| HIGH-1 | `agent.ping` SSRF | **成立但需服务端授权**；目标完全由服务端控制，无内网/云元数据过滤 | `server/websocket.go:395-406`、`server/task.go:250-274`、`server/task.go:203-248` | 未改（功能所需）；**注意不受 `--disable-web-ssh` 约束** |
| HIGH-2 | `server/websocket.go` | 占位项。消息级无额外认证，仅 JSONRPC 版本校验 + 事件去重 | `server/websocket.go:357-377`、`312-341` | 无独立高危；消息大小无上限为加固项 |
| HIGH-3 | `server/task.go` | 占位项。开关生效；Windows 走临时 `.ps1` + `-ExecutionPolicy Bypass` | `server/task.go:33-36, 73-105` | 无独立高危；临时脚本窗口为观察项 |
| HIGH-4 | `server/files.go` | 占位项。已复核越权、根删除、跨卷、别名覆盖 | `server/files.go:239-256, 270-294, 337-353, 408-434` | 无独立高危；破坏性缺陷前轮已修 |
| HIGH-5 | `server/file_stream.go` | 占位项。分片元数据、区间、一致性校验完整 | `server/file_stream.go:84-108, 354-369` | 无独立高危；**会话残留本次修复** |
| HIGH-6 | `server/switch_version.go` | 占位项。开关拦截 + 目标严格解析 + 固定下载源 + 摘要校验 | `server/switch_version.go:10-16`、`update/update.go:690-700` | 无独立高危 |
| HIGH-7 | `terminal/terminal.go` | 占位项。开关拦截、同 ID 会话复用、断连保留计时 | `terminal/terminal.go:84-108, 240` | 无独立高危；**无会话数量上限**（观察项） |
| HIGH-8 | `terminal/terminal_unix.go` | 占位项。PTY 描述符泄漏前轮已修并回归 | `terminal/terminal_unix_close_test.go` | 无独立高危 |
| HIGH-9 | `terminal/terminal_windows.go` | 占位项。ConPTY 关闭路径前轮已修并回归 | `terminal/terminal_windows_close_test.go` | 无独立高危 |
| HIGH-10 | `cmd/root.go` | 占位项。`--ignore-unsafe-cert` 全局降级；`UnknownFlags=true` 会静默吞掉拼错的开关 | `cmd/root.go:122-124, 195, 163-166` | 见 §2 FINDING-2；LOW-1 已加告警 |
| HIGH-11 | `cmd/autodiscovery.go` | 占位项。注册走 `Authorization: Bearer`，**token 文件 0644** | `cmd/autodiscovery.go:96-137` | **本次修复为 0600** |
| HIGH-12 | `protocol/v2/jsonrpc.go` | 占位项。纯常量/结构/JSON 编解码，无 I/O 与逻辑分支 | `protocol/v2/jsonrpc.go:99-149` | 无发现 |
| HIGH-13 | `update/update.go` | 占位项。sha256 摘要 fail-closed、资产大小上限、仓库与 API 基址硬编码 | `update/update.go:30-40, 470-517` | 无独立高危；信任边界见 §4 |
| HIGH-14 | `utils/utils.go` | 占位项。纯日期边界计算，无外部输入拼接 | `utils/utils.go:9-58` | 无发现 |
| HIGH-15 | `fake_agent.py` | 占位项。测试脚本，需显式 `--server/--adkey`，无硬编码凭据，明确不实现 exec/terminal | `fake_agent.py:110-146, 185-207` | 无发现 |
| MEDIUM-1 | endpoint 构造与 token 拼接 | **部分成立**：token 置于 URL query，会进入服务端/代理访问日志；endpoint 来自操作者配置，Agent 不接收 endpoint，无 SSRF 面 | `server/websocket.go:143-152, 452-454` | 日志脱敏跨仓库；未改协议 |
| MEDIUM-2 | POST 回退通信 | **不成立**：回退通道复用 `dnsresolver` 同一客户端构建，TLS 与逐 IP 尝试策略一致，无额外降级 | `server/websocket.go:154-185, 224-260`、`dnsresolver/resolver.go:197-215` | 无发现 |
| MEDIUM-3 | 依赖漏洞扫描 | **已执行**：`govulncheck@v1.9.0`，DB 2026-10-21，`./...` → `No vulnerabilities found` | 见 §3 | 仅覆盖 Go 可达代码 |
| MEDIUM-4 | `--ignore-unsafe-cert` | **属实但为显式 opt-in**，非默认；MITM 风险真实 | `server/websocket.go:487-489`、`cmd/root.go:122-124`、`dnsresolver/resolver.go:209` | 未移除（兼容）；无独立告警为观察项 |
| MEDIUM-5 | `agent.switchVersion` 远程更新 | **严重性下调**：需先控制已认证服务端通道；version 不能指定 URL；下载源固定仓库 + 摘要校验 | `server/switch_version.go:10-16`、`update/update.go:325-337, 470-517` | 无独立高危 |
| LOW-1 | 默认 token 可为空 | **属实**：`MarkPersistentFlagRequired` 被注释，空 token 仍会外连 | `cmd/root.go:163-166` | **已加启动告警**；拒绝策略跨仓库 |

### 需要明确纠正的表述

1. **CRITICAL-1/2 的 PoC 步骤不成立。** 报告写“连接到 agent 的 WebSocket 端点”，但 Agent 不监听端口。真实攻击路径是：先取得服务端凭据（或控制服务端），再借服务端下发指令。因此这两项在本仓库内不是“任意人可触发”的 RCE/任意文件读写，而是“远程管理的授权是否可靠”问题，属跨仓库。
2. **`--disable-web-ssh` 只证明本地开关生效，不等于认证与授权链安全。** 它拦截 `exec`、`file`、`terminal`、`switch_version` 四类入口（`server/files.go:100`、`server/task.go:33`、`terminal/terminal.go:85`、`server/switch_version.go:11`）；`ping`、`startup_config`、`report` 不受该开关约束——`ping` 是监控面功能，这是设计边界，但报告未区分，容易被误读为“开关可绕过”。
3. **`file_size` / `modified_at` 校验只缓解部分文件变化风险，不是 TOCTOU 的彻底解决。** 它缩小了“打开前被替换”的窗口，但校验与 `os.Open` 之间仍存在间隙，且不覆盖打开后内容变化。
4. **digest 校验不等于来源认证。** 摘要来自 GitHub Releases API 的同一响应，能防 CDN/传输篡改，但不能防仓库账号或 API 响应本身被控。
5. **`transfer_token` 不应被要求“整次传输只能使用一次”。** 分块上传需要同一票据覆盖多个分片，并容忍重试与重连；合理约束是绑定 `op + upload_id + 目标路径 + 有效期`，而非一次性满足。

### 关于路径首尾空格（撤回此前建议）

路径首尾空格可能是**真实文件名字符**。前端、服务端、Agent 都必须保留原值，`TrimSpace` 只能用于判空，不能改写实际操作路径；界面若提示可疑空白，应由用户显式选择是否修改，不得静默处理。Agent 侧行为见 `server/files.go:903-925`，前轮已修复并有 `server/files_path_test.go` 覆盖。

---

## 2. 报告未覆盖、本次确认的问题

### UPLOAD-1 上传会话与临时文件永久残留（已修复）

**确认缺口的依据（修复前）**：`uploadChunks` 只在 `upload_commit` 成功与 `upload_cancel` 中被 `delete`（`server/files.go:654, 778`）；全仓无定时回收；`uploadChunkState.CreatedAt` 被写入但无任何读取点，且每收到一个分片就被覆写。客户端放弃上传时，内存条目与 `.<name>.komari-upload-<id>.part` 永久残留。

**修复**（新增 `server/upload_cleanup.go`）：

| 机制 | 实现 |
|---|---|
| 过期依据 | 新增 `LastActivity`，每个分片刷新；`CreatedAt` 恢复为真实创建时间 |
| 回收阈值 | `uploadIdleSessionTTL = 30min`，巡检 `uploadReapInterval = 5min` |
| 在途保护 | 有活跃流的会话不回收（`uploadLifecycles[id].active > 0`） |
| 并发放行 | 与 `upload_commit` / `upload_cancel` 共用每会话写锁，`TryLock` 失败即跳过 |
| 三件套清理 | 临时文件 + `uploadChunks` 条目 + 引用计数写锁条目一并清理 |
| 重启孤儿 | `removeStaleUploadParts`：新会话创建 part 时定向清扫同目录、同目标名、mtime 超 TTL 且不属于存活会话的 part |
| 容量上限 | `maxUploadSessions = 64`，防止大量 `upload_id` 造成 part 文件与内存膨胀 |

### 并发六问的源码结论

| 问题 | 修复前 | 处理 |
|---|---|---|
| 1. 最后检查与新流注册之间是否会删掉其 part 文件 | **存在**：`uploadHasActiveStreams` 在独立临界区，释放后才 unlink。Unix 上会删掉正在写的文件，Windows 上 unlink 失败 | **已修复**：`reclaimUploadSessionIfIdle` 全程持 `uploadLifecycleMu`，`beginUploadStream` 用同一把锁。新增 `TestReclaimHoldsLifecycleLockAgainstRegistration`，并把实现临时改回非原子后确认该测试**失败** |
| 2. `forgetUploadWriteLock` 删条目时仍有 goroutine 持有/等待旧锁 | **成立**：`Unlock` 后 `delete(map)`，等待者仍在旧 mutex 上，第三个调用者会新建 mutex，同一 `upload_id` 出现两把锁 | **已修复**：`uploadWriteLockEntry{mu, refs}` 引用计数，`refs > 0` 时绝不删除。`TestUploadWriteLockNeverSplitsIntoTwoLocks` 断言等待期间条目保留且拿到同一 mutex |
| 3. 孤儿扫描快照后新会话接管候选文件被误删 | **存在**：快照后释放 `chunksMu` 才删除；同 `upload_id` 重传会复用同一 part 路径 | **已修复**：扫描全程持 `uploadLifecycleMu`；part 文件只在 `beginUploadStream` 成功后才创建，故扫描期间不会产生新 part |
| 4. 回收后迟到分片 / commit / cancel / 重试 / 同 ID 重建 | 行为已确定 | 迟到 `first=false` → `unknown upload session`；`first=true` → 重建（`O_TRUNC`）；commit → `unknown upload session`；cancel → 幂等，走确定性路径删除。**未引入墓碑**：`first=false` 无法复活会话，同 ID 重建必须显式 `first=true`。`TestLateChunkAfterReapRequiresFirst` 固定该行为 |
| 5. 删除失败是否保留可重试状态 | 已正确 | `removeUploadFileLocked` 失败时不删 map 条目，下轮重试。`TestReapRetainsStateWhenPartRemovalFails` 在 Windows 用打开句柄触发真实 sharing violation 验证 |
| 6. 长时间无进展的在途请求是否有超时 | 有 | 上传/下载均用 `GetHTTPClientWithoutHTTP2(30min)`，`http.Client.Timeout` 覆盖响应体读取；下载另有 30min context；并发槽位 8。证据 `dnsresolver/resolver.go:207-212`、`server/file_stream.go:76-78, 115` |

### FINDING-1 自动发现 token 文件权限过宽（已修复）

`saveAutoDiscoveryConfig` 原以 `0644` 写入含 token 的 `auto-discovery.json`，同机其他用户可读。现改为 `0600`，并对已存在的文件显式收紧（`os.WriteFile` 的 perm 只在创建时生效）。测试：`cmd/autodiscovery_test.go`。

### FINDING-2 `ParseErrorsWhitelist.UnknownFlags = true`（未修复）

`cmd/root.go:195` 让所有未知参数被静默忽略。若运维把 `--disable-web-ssh` 拼错，Agent 会**带病启动且无任何提示**，与“已关闭远程控制”的预期相反。改为 `false` 会破坏对历史参数的兼容（`Execute()` 目前手工剔除废弃参数），故仅记录，建议改为“对已知安全开关做拼写近似检测并告警”。

### FINDING-3 / FINDING-4 / FINDING-5（未修复，观察项）

- 终端会话无数量上限（`terminal/sessions` map 按 `request_id` 无界增长，每会话持有 PTY）。
- WebSocket 未设 `SetReadLimit`，消息大小无上限；来源为已认证服务端，属信任边界内。
- Windows 任务执行先落 `.ps1` 再交给 PowerShell，多用户主机存在理论上的临时文件窗口。

---

## 3. 实际执行的测试与结果

| 命令 | 结果 |
|---|---|
| `go build ./...` | exit 0 |
| `go vet ./server/` | exit 0 |
| `go test ./server/ -race -count=2 -run "TestReclaim\|TestReap\|TestUploadWriteLock\|TestLateChunk\|TestUploadSessionCapacity"` | PASS（无竞态报告） |
| `go test ./server/ -race -count=1 -run "TestUploadStream\|TestCommitFileUpload\|TestCancelUploadStreams\|TestWriteUploadStreamChunk\|TestRemoveStaleUploadParts"` | PASS |
| `go test ./cmd/ -run TestWriteAutoDiscoveryConfig -v` | 1 PASS，1 SKIP（Windows 无 POSIX 权限位） |
| `go test ./... -count=1` | `cmd`、`monitoring/unit`、`terminal`、`update`、`ws` 全部 ok；`server` 仅 `TestICMPPing`、`TestTCPPing`、`TestHTTPPing` 失败 |
| `go test ./... -count=1 -skip "TestICMPPing\|TestTCPPing\|TestHTTPPing"` | 全部 ok |
| `git worktree add --detach <tmp> HEAD` 后在同一环境跑同样 3 个 ping 测试 | 失败集合与当前工作树**完全一致**（时长归一化后 `Compare-Object` 差异数 = 0） |
| `govulncheck ./...` | `No vulnerabilities found`（govulncheck v1.9.0，DB 2026-10-21，Go 1.27.3） |

**关于 ping 测试失败**：基线与当前工作树失败集合相同，可确认与本次改动无关；失败原因是外网依赖（`v4-sh-cm.oojj.de`、`v6-sh-cm.oojj.de` DNS 解析失败、IPv6 地址无路由超时、`117.185.125.154` 返回非 2xx/3xx）。同一环境下 `proxy.golang.org` 也因 IPv6 路由不可达而超时，进一步佐证为网络环境问题。

**格式与换行**：仓库为 `core.autocrlf=true` + `.gitattributes: * text=auto`，工作区为 CRLF，`gofmt -l` 会对**所有**文件报警。改用“CRLF→LF 归一化副本”逐文件执行 `gofmt -l`，本次全部改动文件无输出（格式合规）；`git diff --check` 退出码 0；`git diff --stat` 显示仅 `30 insertions / 35 deletions`，**未发生全仓换行重写**。

---

## 4. 未验证条件、跨仓库协作项与剩余限制

### 跨仓库待核实（不写成“已确认安全”，也不写成“已确认漏洞”）

1. **服务端 token 校验与操作级授权**：Agent 仅用 `?token=` 标识身份，是否放行 `exec`/`file`/`terminal` 完全由服务端决定。
2. **`transfer_token` 语义**：建议绑定 `op + upload_id + 目标路径 + 有效期`，并容忍同一票据覆盖多个分片、重试与重连（不要求一次性）。
3. **访问日志脱敏**：token 出现在 URL query（`/api/clients/v2/rpc?token=`、`/api/clients/terminal?token=&id=`、`/api/clients/transfer/<id>?token=&transfer_token=`），服务端与反向代理应过滤 query。
4. **会话超时后的服务端行为**：应主动下发 `upload_cancel`；收到 `unknown upload session` 后必须从 `first=true` 重传，不能死循环重试同一分片。
5. **`ping` 目标策略**：内网/云元数据地址段的限制只能在服务端做（Agent 侧不新增黑名单以免破坏监控语义）。
6. **空 token 是否拒绝连接**：属服务端策略；Agent 侧已加告警但未硬拒绝，避免破坏依赖服务端 IP 认证或前置代理认证的部署。

### 剩余限制（本仓库内已知未解决）

1. 进程重启后的孤儿 part 文件**无法全盘枚举**，仅在“同一目标再次上传”时定向清扫；需要 Agent 落盘会话索引或服务端主动取消才能彻底解决。
2. 终端会话数量无上限、WebSocket 消息无大小上限（见 FINDING-3/4）。
3. `UnknownFlags=true` 会静默吞掉拼错的开关（FINDING-2）。
4. `--ignore-unsafe-cert` 无独立启动告警，仅伴随 remote control 的 MOTD/Toast 提示。
5. 原报告 HIGH-2～HIGH-15 是**只有文件名的占位项**（置信度 60%、未验证），只能给出文件级结论，无法还原报告作者原本设想的缺陷点。
6. 本仓库的 23 项编号结论基于 `a51819e` + 未提交修改；提交后如需归档，应附 commit hash。

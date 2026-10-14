# HANDOFF — 项目转接文档

写给接手的 agent：读完这份 + README，你应该能跑构建、跑测试、接着做进行中的任务，全程不用问任何人。所有口径都有出处（提交号 / fork 行号），不确定的以代码为准。

## 0. 一分钟速览

buddyhub 是一个 Go 单二进制网关（Go 1.22，模块 `github.com/chipchipss/buddyhub`），对外暴露 OpenAI 兼容 HTTP 接口，内部把请求按**模型名前缀**路由到多个上游平台（腾讯 WorkBuddy 账号池 + 十几个外部平台）。带 Web 面板（原生 JS 无构建步骤）、账号池轮转、用量记账、定时任务、钉钉告警。

- 主开发路径：`D:\github-backup\buddyhub`（Windows，远端 `github.com/chipchipss/buddyhub` main）
- 转接基线：`42d79f0`（2026-10-14，工作区干净，与 origin/main 同步）
- README 的「模型路由（前缀即协议）」「KNOWN-LIMITATIONS」两节是功能全景与已知边界的权威

## 1. 构建与测试

```bash
go build ./...          # 全仓构建
go vet ./internal/...   # 静态检查

go test -count=1 ./...            # 全量（-count=1 禁缓存，核验一律带）
go test -race ./...               # 本机跑不了（Windows 无 gcc/cgo）——只在 CI ubuntu 跑
```

- 双验口径：**清缓存重建**（`go clean -cache -testcache` 后 build+test），别信增量缓存
- 全仓 32~33 个测试包，panel 一包约 19s（含前端 harness）、server 约 17s，其余秒级
- 面板前端测试（Go 内嵌 harness）本地无 node 时自动跳过；CI 的 ubuntu runner 有 node，会真实执行——首次 CI 可能暴露本地没跑到的断言，属覆盖增强
- 纪律：**源码文件 ≤500 行**（超出要拆）；提交信息带 fork 来源与行号；小步提交

## 2. 仓库结构（职责地图）

```
cmd/server/            主入口。config.go（全部配置字段与 JSON 标签）+ main.go（装配：
                       pool/panel/handler/scheduler 的依赖注入）
internal/server/       请求管线核心。handler.go（主逻辑）+ dispatch.go（模型前缀 →
                       各平台桥接的路由）+ 每平台一个 *_bridge.go（qoder/zcode/cursor…）
                       + gateways.go（前缀注册表）+ responses*.go（OpenAI 兼容层）
internal/extprovider/  每上游平台一个包（客户端：登录/凭据刷新/业务 API）。通用的：
                       authcore/（device-flow 五态轮询 + PKCE + refresh-lead + spent 缓存
                       + 登出谓词——多平台登录底座，新平台从这里填空）
internal/extstore/     外部平台账号持久化（ext-accounts.json）+ 轮转排序 + 续期注册表
internal/pool/         腾讯 WorkBuddy 账号池（状态机/冷却/挑选）
internal/usage/        用量记账：内存小时桶 + 30s 防抖落盘（usage.json）+ per-Key token 桶
internal/contextbudget/ 出站上下文预算裁剪（Kovak 规则：只删不改，预算内逐字透传）
internal/session/      会话粘性路由
internal/scheduler/    定时任务（签到/保活/旅行/外部平台 hook）
internal/panel/        Web 面板（Go 后端 + web/ 原生 JS 前端，无构建步骤）
internal/dingtalk/     钉钉告警（加签/冷却/五类模板）
internal/upstream/     腾讯池上游客户端（SSE/重试/头部指纹）
internal/zai/          Z.AI 专用栈（JWT/验证码/额度）
```

**新平台接入四件套**（照 cursor/zcode 抄）：① `extprovider/<名>/` 客户端；② `extstore` 加
`P<名>` 枚举 + 续期注册表；③ `server/<名>_bridge.go` + `dispatch.go`/`gateways.go` 注册；
④ `panel/platforms.go` 注册表行（Login/Renew 标记驱动面板交互）。

## 3. 进行中的任务（转接时点）

### 3.1 CI/CD（未开工，有完整设计）

- GitHub Actions workflow：ubuntu runner 跑 `go test -race ./...` + `go vet ./...` + 基线式文件大小门禁（workflow 文件放 `.github/workflows/ci.yml`）
- **基线门禁**：以 `42d79f0` 冻结违例清单（存量超 500 行：`cursor/run.go` 813、`cursor/runbuild.go` 688），CI 只拦**新增**违例，存量拆分排后续——否则第一天自锁
- **-race 照射重点**（auxiliary 点名）：`cursor/run.go` 心跳 goroutine + `appendSender`（mutex+seq）+ `ChatStream.Close()` 与 pump 竞态、`cursor.CatalogCache`（Get 会并发拉两路）、`session.Store`、`authcore.SpentCache`、`dingtalk.Notifier`；`contextbudget` 是纯函数照不出东西
- **测试约束**：cursor/zcode 桥接测试替换全局 `http.DefaultClient`（仓里既有手法），**不能加 `t.Parallel`**（会假失败）——写进 CI 配置注释
- CI 的 ubuntu runner 有 node：面板前端 harness 会真实执行（本地跳过），首次跑可能暴露新断言失败，如实修
- `-race` 只在 CI 有意义；本地 Windows「没有 race 输出」**不是**检查通过的依据

### 3.2 MiniMax 网关接线（未开工，范围已定，auxiliary 负责）

客户端已移植（`internal/extprovider/minimax`，device flow + 刷新 + 登出判定，双站点 CN/Global），缺网关四件套：

1. `internal/server/minimax_bridge.go`：OpenAI 请求 → MiniMax agent 端点，流式/聚合两路（照 `zcode_bridge.go` 骨架）
2. dispatch/gatewayRoutes/PlatformOf 注册（`minimax:` 前缀）
3. `/v1/models` 列 `minimax:` 名单（双站点）
4. 面板账号侧核对（客户端签名/登录已就绪，桥只消费）

两条已定口径：每日签到积分看客户端 `CheckinDaily` 与 extstore 现状决定是否进 `checkinCapable`；tool 语义若端点不支持则**如实报错不静默降级**（与 Cursor 桥同口径）。

### 3.3 后续队列（Chief 已排）

第二波：模型目录可视化（面板透出 `/v1/models`，**必须后台预取 + stale 服务**——冷启动同步拉 Cursor 目录有 35s 上限，会卡页面）· usage 账本回查（~130 行，工具步用量修正）。第三波按需：ZCode 60s 冷却、配置热加载、钉钉每日摘要、三批登录平台（Kiro/Cline/Grok/Factory，authcore 现成纯填空）。

## 4. 代码口径（红线，违反会被驳回）

- **假流验证 ≠ 真流**：httptest 假上游全绿只证明内部一致；接真实上游前保持 experimental 标记与 README KNOWN-LIMITATIONS 记录。Cursor 真连验证被用户取消（无账号），步骤留在 README
- **fail-open / 如实报错的分界**：配额基础设施出错放行（fail-open）；端点不支持的能力**如实报错**，绝不静默降级成假旋钮
- **登出判定每平台一个谓词**：MiniMax 只认 `invalid_grant`+400（authcore.LogoutPredicate）；Cursor 上游 401 **不**标账号 lapsed（fork 语义，双验驳回过补标的建议）
- **流式铁律**（qoder 五态状态机教训）：正文已向客户端输出后不可原位 retry/failover；流在终局前结束必须报错（premature EOF ≠ 正常收尾）；上游 [DONE] 早于 finish_reason 也是失败
- **只删不改**：contextbudget 预算内逐字透传，绝不改写/摘要/重排
- **外部行号参考用前必须对本地 HEAD 重核**（防漂移）；批量编辑用文件编辑工具，别用 PowerShell 正则改写含中文的源码（会坏 UTF-8）
- **测试双验**：每个提交由另一成员 fresh 清缓存跑全量；进度每批次报群（用户要求主动汇报，不催）

## 5. 已知边界速查（详情在 README KNOWN-LIMITATIONS）

| 平台 | 边界 |
|---|---|
| Cursor | experimental 假流验证级（真连被取消）；HTTP/1.1 降级路径；模型目录已接（10min TTL 缓存，冷启动同步拉有 35s 上限）；usage 无 ledger 回查（工具步偏低）；采样参数不支持（上游无此位，如实丢弃） |
| Qoder | CN 企业 VPC 路由未实现（若做必须 fail-closed）；流内定时 keepalive 未实现；COSY 签名不适用（api2-v2 Bearer 链路） |
| ZCode | Start Plan JWT 路径未实现（入库侧硬拒）；无 60s blocked 冷却（PRE 门覆盖） |
| MiniMax | 客户端就绪，网关未接线（进行中，见 3.2） |
| 全局 | `context_budget_by_provider` 装配期解析（重启生效）；Qoder 出站裁剪钳制 ≤900K |

## 6. 参考源码（fork 克隆，MIT）

| 克隆（%TEMP% 下） | HEAD | 用途 |
|---|---|---|
| `magpie-plugins` | f573cfd | 登录五平台行号级参考（qoder/zcode/cursor/minimax/kiro 等 15 auth 插件） |
| `9router-qoder-plus` | 290c68c | Qoder 增强五阶段与 Cursor 协议的原始参照（open-sse/executors/qoder.js 2074 行） |
| `ai-proxy-cloudflare` | — | 小时桶用量聚合（src/lib/usage-db.ts 的 usage_hourly UPSERT 口径） |
| `zcode-leech` | — | context_window.py（contextbudget 语义）与 tool→system 编码 |
| `qoder-farm` | — | PAT 批量导入（inject_qoder_pat.py，已移植为 /panel/api/import/qoder-pat） |

⚠️ %TEMP% 可能被清理；行号参考用前对本地 HEAD 重核。远程参考仓库：magpie-community/plugins、MoonCoder-HAPPY/9router-qoder-plus、vhondo100/zcode-leech、tobyberry666/codearts-account-pool、sctg-development/ai-proxy-cloudflare、fazulfi/qoder-farm。

## 7. 团队协作口径

- 分工现状：auxiliary 执行主力，coder 辅助 + 独立双验（fresh 清缓存全量），Chief 分派与裁定
- 用户口径：**主动汇报**（开工一句/每笔提交核验后一句/阶段收口总结），不催不问
- 有分歧在群里摆事实，Chief 裁定；驳回要有 fork 源码依据
- 转接完成后本群归档，接手方独立作业

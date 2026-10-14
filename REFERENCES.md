# 参考源码清单与移植对照（转接用）

> 用途：本项目（buddyhub）的多个模块是从外部开源仓库**逐字移植/改写**而来。
> 接手的人需要知道"这段代码的原始出处与对应行号"，才能校对语义、跟进上游修复、
> 或补齐我们有意没做的部分。
>
> 本文件由 10-12 ~ 10-14 的开发过程整理。**行号对应下表 HEAD 的源码**，
> 上游若移动过，请以函数名/常量名为准重新定位。

## 0. 先看这一条：克隆是临时的

下表"本机路径"里的克隆都在**当初那台开发机**的临时目录，随时可能被清理，
**不要依赖它们存在**。换机器/隔几天后按下面命令重取（都是浅克隆，秒级）：

```bash
# 放在仓库根的 refs/ 即可（refs/ 已在 .gitignore 里，不会被误提交）
cd <本仓库>/refs
git clone --depth 1 https://github.com/MoonCoder-HAPPY/9router-qoder-plus
git clone --depth 1 https://github.com/magpie-community/plugins magpie-plugins
git clone --depth 1 https://github.com/vhondo100/zcode-leech
git clone --depth 1 https://github.com/tobyberry666/codearts-account-pool
git clone --depth 1 https://github.com/sctg-development/ai-proxy-cloudflare
git clone --depth 1 https://github.com/fazulfi/qoder-farm
git clone --depth 1 https://github.com/decolua/9router        # 上游本体（buddyhub qoder 通道的祖先）
# OmniRoute 约 720MB（含技能/文档资产），只在真要查它的策略层时再克隆：
git clone --depth 1 https://github.com/diegosouzapw/OmniRoute
```

钉死的版本（已验证的 HEAD，浅克隆后 `git rev-parse HEAD` 应是新头，但下表是
我们**实际读过的内容**对应的提交）：

| 仓库 | 我们读过的 HEAD | 大小 | 本机当时的路径 |
|---|---|---|---|
| MoonCoder-HAPPY/9router-qoder-plus | `290c68c9340d` | 13 MB | `%TEMP%\9router-qoder-plus` |
| magpie-community/plugins | `f573cfdcbae0` | 2.2 MB | `%TEMP%\magpie-plugins` |
| vhondo100/zcode-leech | `fc733eeeb4e3` | 0.1 MB | `%TEMP%\zcode-leech` |
| tobyberry666/codearts-account-pool | `95206924b20d` | 0.4 MB | `%TEMP%\codearts-account-pool` |
| sctg-development/ai-proxy-cloudflare | `1779dd90d537` | 2.6 MB | `%TEMP%\ai-proxy-cloudflare` |
| fazulfi/qoder-farm | `1c56e3f02440` | 0.2 MB | `%TEMP%\qoder-farm` |
| diegosouzapw/OmniRoute | `7f3cbb046b37` | 720 MB | `%TEMP%\OmniRoute` |

**许可**：以上全部 MIT（magpie-plugins 的 LICENSE 为 MIT，已在提交信息中逐笔注明
fork 来源；`internal/extprovider/cursor/pb.go` 等文件头部也写了出处与 MIT）。
qoder-farm 只取了"凭证注入 + 余额校验"，其**账号注册农场部分刻意不移植**（不进
buddyhub 主体，属既定裁定）。

## 1. 移植对照表（我们的代码 ← 原始出处）

| buddyhub 位置 | 来源 | 关键定位 |
|---|---|---|
| `internal/extprovider/qoder/resilience.go` | 9router-qoder-plus `open-sse/executors/qoder.js` | 排队识别 403/10605/`isQueued`；`QODER_QUEUE_*` 默认 15 次 5s→60s；首 token 超时三档策略 |
| `internal/extprovider/qoder/`（会话隔离、1M/900K 压缩、compaction 桥接、AutoContinue） | 同上 + `open-sse/utils/sessionManager.js`（`resolveSessionIdentity` 约 L208）、`src/shared/constants/codexCompat.js`（`QODER_CONTEXT_WINDOW=1_000_000`、`QODER_AUTO_COMPACT_TOKEN_LIMIT=900_000`）、`open-sse/translator/concerns/qoderCompaction.js` | 悬挂意图判定与 CONTINUE_NUDGE 见 qoder.js（约 L1124-1167、L1554-1605、L1845-1906） |
| `internal/extprovider/qoder/quota.go` + 面板配额 | `src/shared/services/apiKeyPolicy.js`（约 450 行）、`src/shared/services/ccSwitchUsage.js` | PRE 门 fail-open、CC-Switch `/api/usage` 与 `/user/balance` 契约字段 |
| 钉钉告警（`internal/dingtalk`） | `src/shared/services/modelIdleAlert.js` | 5 类告警、`signDingTalkWebhook` 加签、按 Key/账号独立冷却 |
| `internal/extprovider/authcore/` | magpie-plugins 各平台 `index.mjs` 的公共形态 | PKCE S256、device flow 五态、refresh-lead 三参数、登出谓词 |
| `internal/extprovider/minimax/minimax.go` | magpie `packages/minimax/index.mjs` | `startDevice` L226-257、`pollDevice` L259-286、`refreshToken` L289-299、**登出判定 L304（仅 invalid_grant+400）** |
| `internal/extprovider/qoder/site.go`（双站点） | magpie `packages/qoder/index.mjs` | `deviceSignIn` L249-312、`refreshJob` L327-336（**refresh 一次性**）、CN 的 `deviceChat` 特例 L279-282、REFRESH/RENEW_LEAD L48-49 |
| `internal/extprovider/zcode/` | magpie `packages/zcode/index.mjs` | `jwtExpiry` L110-123、`authorize` L870-927、三级凭证探测 L823-864、`spentUp` L247-255（429 与 200-body 1113/1005） |
| `internal/extprovider/cursor/signin.go` | magpie `packages/cursor/index.mjs` | `browserSignIn` L270-323、`keyToken` L238-255、`authFile` L111-116、`expiry` L133-142、**refresh 不能换 access（L264-267）** |
| `internal/extprovider/cursor/pb.go` | 同上 L861-1030 | 手写 protobuf 编解码 + Connect 帧（5 字节头/64MB 上限/gzip/end） |
| `internal/extprovider/cursor/runbuild.go` | 同上 L1079-1091、L1146-1308、L1545-1591 | AI SDK message 构造、`conversation` L1171-1246、`buildRun` L1552-1590、`conversationID` L1285-1308 |
| `internal/extprovider/cursor/run.go` | 同上 L1700-1799、L1803-1887、L1891-2122 | HTTP/1.1 降级路径（RunSSE 下行 + BidiAppend 上行）、`decode` 事件流、错误分类 L1036-1077 |
| `internal/extprovider/cursor/models*.go`（4 个文件） | 同上 L522-827、L2167-2180 | 模型目录、`pick`/`named`/`request`、`usable` L765-783、maxOnly L2145/L2198-2199 |
| `internal/contextbudget/` | zcode-leech `pool/context_window.py` | 预算内逐字透传；超预算必留 system/末条 user/近期尾部 + 关键词精确检索回填；tool_calls 与 tool 响应不拆散 |
| `internal/usage/`（按 Key 小时桶） | ai-proxy-cloudflare `src/lib/usage-db.ts` | `usage_hourly` 每 (user, provider, keyOwner, keyHint) 每小时一行的**维度口径**（我们的写路径本来就是内存+防抖，没照抄它的 SQLite upsert） |
| `internal/panel/patimport.go` | qoder-farm `inject_qoder_pat.py` | 读 `[{email,pat}]`、校验 `pt-` 前缀、幂等 upsert、坏行不阻断好行 |
| 账号池状态机设计（`internal/extstore`、`internal/pool` 的思路参照） | codearts-account-pool `internal/pool/pool.go`、`internal/server/handler.go` | 三类冷却、`PickExcluding`、每账号单槽、**会话亲和 + "流式已输出绝不重放"** |
| 路由/配额策略的**长期演进方向**（未落地） | OmniRoute `src/domain/`：`policyEngine.ts`、`keyQuota.ts`、`quotaCache.ts`、`lockoutPolicy.ts`、`fallbackPolicy.ts` | "选谁"与"怎么发"分离成纯函数；设计文档 `docs/OMNIROUTE_PROVIDER_FAILOVER.md`、`ROUTING_POLICY.md`、`QUOTA_TELEMETRY.md` |

## 2. 未完成任务清单（接手时的在途项）

### 2.1 CI（含 `-race`）——注意三个坑
1. **`-race` 只能在 CI（ubuntu）跑**：开发机是 Windows 且无 gcc/cgo，本地永远照不到
   race。PR 上别把"本地无 race 输出"当检查通过依据。
2. **该被 race 照的重点对象**（都是真共享态）：
   `cursor/run.go` 的心跳 goroutine + `appendSender`（mutex/seq）+ `ChatStream.Close()`
   与 pump 的竞态；`cursor/models_fetch.go` 的 `CatalogCache`（mutex+map，Get 内并发两路）；
   `qoder` 的 `SessionStore` / `CreditLedger` / `UsageAccumulator` / `SpentCache`；
   `dingtalk` Notifier；`usage` 的 `keyBuckets`（同 mutex 下，理论上安全）。
   反例：`internal/contextbudget` 是纯函数，race 照不出东西，别为它加噪声断言。
3. **桥接测试替换了全局 `http.DefaultClient`**（zcode/cursor 桥测试同款手法）。
   现在安全只因**没用 `t.Parallel`**——加并行会假失败。要并行化必须先改成注入式 client。
4. **文件大小门禁要做成基线式**（"不得新增违例"）：包里仍有 `cursor/run.go` 813 行、
   `cursor/runbuild.go` 688 行超 500 上限（`models.go` 的 1103 行已在 `42d79f0` 拆完）。
   硬门禁第一天就会自锁；拆这两笔排在第二波。

### 2.2 MiniMax 网关接线（范围比看上去大得多：整平台未接）

**实测现状**（`internal/extprovider/minimax/`，333 行 + 204 行测试）：
包里**只有设备登录四件事**——`StartDevice` / `Poll` / `WaitDevice` / `Refresh`
（含 fork 的两个坑：无 device_code 时用 user_code 轮询、`expired_in` 毫秒/秒歧义）。

而且**这个包目前无人引用**（全仓 grep `extprovider/minimax` 零命中）：
`extstore` 没有 `PMiniMax` 常量、面板注册表没有条目、`extlogin.go` 没有登录入口——
**连"登录能用"都还没成立**，不是只差一座桥。接线时的顺序：先把登录接进面板/账号表，
再补签名与 chat，最后上桥与清单。

其余缺口：

| 缺件 | 说明 / fork 定位 |
|---|---|
| 请求签名 | `x-mini-max-*` 头的 md5 口径，fork `signed()` L318 / `SIGN_SECRET` L22 / `md5` L55。**注意坑**：包注释把"请求签名"列进了"协议要点（对齐 fork）"（`minimax.go` L14），但**包里没有任何签名实现**（全仓 grep 该符号只命中那行注释）——读注释会误以为已就绪。接线时先实现，再把注释改成"未实现/待接"，别让下一位再踩一次。 |
| chat 端点与流式 | fork `api()` L53 = `<agent>/mavis/api/v1/llm/v1`，Anthropic 兼容形状？需自行核实（fork 的 `NPM = "@ai-sdk/anthropic"` L23 提示走 anthropic 消息协议，与 zai/zcode 桥同一套翻译可复用 `upstream.ZaiTranslateIn/OutJSON`） |
| 模型清单 | fork `liveModels` L161 / `parseModels` L129 / `MODELS` L69 |
| 每日签到 | fork 有"每日签到积分"（账号 credits），Go 侧**完全没有** `CheckinDaily`；若要做，还要同步登记 `extstore.checkinCapable` + `CheckinOne` 分支（两处一致性由 `platforms_test.go` 交叉校验，漏一处会红） |

**接线四件套（照 zcode 的成功路径抄形状）**：
1. `internal/extprovider/minimax/`：补签名 + chat 客户端 + 模型清单（先做签名与 chat）。
2. `internal/server/minimax_bridge.go`：OpenAI 请求 → MiniMax 端点，流式 + 非流式两条路径
   都要（`h.clientWantsStream` 分流）；参考 `zcode_bridge.go`（约 257 行）的结构，
   含"流式已输出绝不换号重放"。
3. 注册三处：`dispatch.go` 分支、`gateways.go` 的 `gatewayRoutes`、
   `resolve_model.go` 的 `PlatformOf`（+ 前缀常量 `minimax:`）；
   面板 `internal/panel/platforms.go` 加条目 —— **一致性测试会交叉校验这四处，
   漏一处必红**。
4. 账号侧：`extstore` 加 `PMiniMax` 常量 + `ViewOne` 分支；`/v1/models` 列
   `minimax:` 名单（双站点 cn/global）。

**边界照实声明**：MiniMax 的 tools 是"tools→system 编码"那套（与 zcode-leech 同源，
fork `_toolcalls.py`）；若请求带 tools 而端点不支持，**如实报错、不静默降级**
（与 Cursor 桥同口径）。

### 2.3 已知边界（README `KNOWN-LIMITATIONS` 各节都有明细，此处只列名）
- Cursor 保持 **experimental + 假流验证级**：真连验证因用户无账号已搁置。真连时
  **最可能先撞模型名映射**（目录外名字原样送那条兜底），排查法已写在 README。
- Cursor 有意未做：HTTP/2 bidi 主路径、ServerConfigService 区域路由、
  usage 账本回查（工具步用量偏低，约 130 行）、本机 cursor-agent 版本探测。
- ZCode：额度耗尽无 60s blocked 短冷却（PRE 门已覆盖）；Start Plan JWT-only 通道
  未开放（面板入库侧直接拒绝）。
- `/v1/models` 冷启动会**同步**拉一次 Cursor 目录（picker 30s + usable 8s，另加 35s
  上限，之后 10 分钟缓存）。**面板不要直接拿它渲染**，会卡住页面；
  做"模型目录可视化"时必须配后台预取 + stale 服务。

## 3. 代码口径（本仓库既定纪律，移植时别漂）
- 每个 ext 平台文件头部写 fork 出处 + 行号；提交信息带 fork 来源与 MIT 标注。
- **小步提交**，每笔 `go build ./...` + `go test -count=1 ./...` 全绿再提。
- 排队/限流识别**优先于**普通失败；退避参数走环境变量，别写死。
- 配额/额度门一律 **fail-open**：配额基础设施出错不能挡正常流量。
- 记账 **fire-and-forget**，不拖慢主链路。
- 出站改写（提示词、context_budget 裁剪）**fail-open**：解析失败原样返回。
- **流式已输出绝不换号重放**（codearts 的教训）。
- 流在终局前结束必须**报错**，不能当正常收尾（"仅正常 EOF 可当缺 DONE"）。
- 登出判定每平台一个谓词：Qoder = jobToken refresh 401/403；MiniMax = invalid_grant+400；
  ZCode coding key 的 401 **不标**失效（永久 key）。网络/5xx 一律不固化为掉号。
- 临时值走 env / 配置；面板改动走 livecfg 热生效。

<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">BuddyHub</h1>

<p align="center">
  <b>多平台 Buddy 账号统一积分与网关中心 · OpenAI / Anthropic / Responses API 兼容</b><br>
  Web 面板 · 账号池轮转 · 工具调用自愈 · Responses API · 定时签到 / 活跃 / 旅行 / 保活 · 成长任务一键完成 · <b>讯飞 Loomy + LobsterAI + 小浣熊 + Qoder + 华为云 积分自动领取 · Z.AI / ZCode · GitHub Copilot · Cline（免费池）· AutoClaw（智谱）· QClaw（腾讯）</b>
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="Transport" src="https://img.shields.io/badge/Transport-SSE%20%2F%20Streaming-0DBD8B?style=flat-square">
</p>

---

> 本项目融合了两个上游的血统：**[workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)**（可视化运维层基座）+ **[workbuddy-gateway](https://github.com/CangShui/workbuddy-gateway)**（工具调用自愈 / Responses API 协议层移植），并新增讯飞 Loomy 与 4 个外部积分平台的自动化。
> 差异概览见 [与上游的差异](#-与上游的差异)；上游设计的精巧之处（账号池调度、错误分类、提示词体系）原样保留，详见下文与上游 README。

## 项目简介

WorkBuddy2API 是一个自托管的 **OpenAI 兼容反向代理网关**，将腾讯 CodeBuddy（`copilot.tencent.com`）账号包装为统一的 `/v1/chat/completions` 服务。

- 官方不提供 OpenAI 形态的开放 API，本项目通过 **OAuth 设备授权**（面板「添加账号」或 `login.sh`）获取账号凭证，在网关侧做 token 自动刷新、账号池调度与流量治理；
- 面向 **个人多账号** 场景：多账号共享、单号故障自动换号、冷却 / 熔断防止雪崩、会话粘性保证多轮上下文不跳号；
- 对客户端只暴露 OpenAI 兼容接口，现有 SDK / 前端 / 工具 **零改造接入**。

> ⚠️ 合规须知：本项目是**非官方**网关，使用 CodeBuddy 账号作为上游，**仅限本人授权账号、本机 / 私有环境测试**。详细边界见[安全与合规](#安全与合规)。

## 核心能力

| 能力 | 说明 |
|---|---|
| 🔑 **OAuth 一键登录** | `login.sh` 设备授权流程，自动落盘凭证并重启容器加载新账号 |
| 🔄 **多账号池** | 三因子加权随机选号（积分占比 ×10 + 闲置补偿 + 成功率 ×3），Top-5 候选 + 防惊群 |
| 🛡️ **熔断与冷却** | 429 软冷却 600s 起指数退避（封顶 `soft_rate_max`）、404 固定 60s 短冷却、402 硬冷却至次日 04:00、连续失败熔断、在途租约限流 |
| 🧲 **会话粘性** | 同一会话（`conversation_id`）尽量绑定同一账号，TTL 滚动续期，失败自动解绑，可镜像 Redis 防重启丢失 |
| ⏰ **定时任务** | 签到（09/21 点，末尾自动跑**连登管家**：兑换已解锁档位 + 抽完抽奖次数）+ 活跃上报（10 点，点亮连登 / 解锁领养 + streak 自检）+ 猫猫旅行（09/21 点，独立排程）+ token 保活（22 点），四类独立开关 |
| ⚡ **流式 + 非流式** | 出站强制 `stream:true`；SSE 帧按规范白名单重建；非流式由本地聚合为单响应 |
| 🧠 **推理模型兼容** | DeepSeek 思维链注入（`thinking.type=enabled` + 默认档）、`reasoning_content` 多轮回填、effort 档位自动降级 |
| 💬 **系统提示词体系** | 网关自有提示词替换客户端 system（默认 `custom`），从源头消灭 system 来源的内容误报；`passthrough` 遇拦截自动降级重试 |
| 🗑️ **指纹脱敏** | 出站请求体黑名单指纹字段清洗（可关闭），与提示词体系两层叠加 |
| 📊 **可观测** | 每请求一行表格日志（TTFB / token 速率 / uid）；`/healthz` 带 `service` 身份标识可接负载均衡 / 宿主探活 |
| 💾 **状态持久化** | 池状态本地原子落盘 + Upstash Redis 异步镜像（可选），重启择新恢复 |
| 🖥️ **Web 管理面板** | 内嵌单色玻璃面板（前端为原生 ES 模块，无构建步骤），总览 / 账号 / 用量与积分 / 自动化 / 模型档位 / API 密钥 / 配置（热生效）/ 运行日志，`⌘K` 命令面板，见 [Web 管理面板](#-web-管理面板) |
| 🤖 **多平台账号池** | 腾讯 WorkBuddy（OAuth 设备授权 + 成长任务全自动）· 讯飞 Loomy（客户端检测 / 密码 / 短信 / Token）· 外部平台（**小浣熊微信扫码 · Qoder 设备授权 · GitHub Copilot 设备码** 一键入池；LobsterAI / 华为云 CodeArts 手工填写）· **Z.AI / ZCode**（Coding Plan JWT + API Key 双通道、额度、套餐领取），见 [外部平台账号池](#-外部平台账号池) / [Z.AI 账号池](#-zai--zcode-账号池) / [GitHub Copilot 通道](#-github-copilot-通道) |
| ➕ **统一入池入口** | 「添加账号」抽屉覆盖全部平台：腾讯 WorkBuddy · Loomy · Z.AI · 外部平台（含 Copilot 设备码授权）· 手动 JSON |

## 🎯 成长任务一键完成（17/18）

官方「成长计划」的 18 个成长任务中，**17 个可在面板上一键纯 API 完成**——无需安装官方客户端、无需人工交互，点一下「一键完成」即自动推进进度、等待异步计分落定并**自动领奖**。剩余任务展示操作指引。

### 任务覆盖与奖励

| 任务 | 奖励 | 一键完成方式 |
|---|---|---|
| `first_buddy` | +300c +8e | 解锁上报 → 同意协议 → 领养第一只 Buddy |
| `create_canvas` | +300c +5e | 设计画布创建事件组（Ardot 遥测） |
| `chat_5` | +100c | 对话活跃上报 ×5（自动补足差额） |
| `Model_chat_GLM5.2` | +100c +5e | glm-5.2 真实对话一次（发一条短消息） |
| `RichMeow_Chat` | +100c +5e +UR Buddy | 桌面端对话事件链（6 事件，含成功回执） |
| `Buddy_App` | +100c +5e | Buddy 应用「发现→进入→授权」事件链 |
| `Buddy_App_QQ` | +50c +5e | 企鹅教师助手进入事件链（与上一条共用） |
| `automation_1` | +100c +5e | 定时任务创建成功事件 |
| `Library_read` | +100c +5e | 资料库阅读点击（web 域上报） |
| `template_5` | +100c +5e | 模板使用事件组 ×5 |
| `playbook_prompt` | +100c +5e | 灵感案例「做同款」发送事件 |
| `expert_5` | +100c +5e | 真实专家召唤+使用链 ×5（专家市场拉真实专家 → 真实对话 → 使用事件） |
| `Expert_team_use_3` | +100c +5e | 专家团召唤+使用链 ×3 |
| `Hp_Appearance` | +100c +5e | 主题设置 + 皮肤生效事件 |
| `Expert_lighthouse` | +100c +5e | 轻量云专家召唤+使用链（真实对话 requestId，**可免费领一个月轻量服务器**） |
| `skill_1` | +100c +5e | 真实对话 + 技能加载事件（skill_info） |

**全新账号一键全做完 ≈ +1950 credits +78 能量**，其中仅数个任务涉及真实对话（`Model_chat_GLM5.2` 一条、`expert_5`/`Expert_team_use_3`/`skill_1` 各数条 fast-model 短对话），其余全部为行为事件上报，零对话消耗。

### 不可自动的 1 个

| 任务 | 原因 |
|---|---|
| `Expert_Philanthropy` | 需真实捐款（服务端领奖时校验捐赠回执，已实测无法绕过） |

### 实现原理（简述）

任务计分走 `/v2/report` 行为上报，但**不同任务认不同客户端指纹**：CLI 指纹（`www.codebuddy.cn`）、桌面指纹（`copilot.tencent.com` + `WorkBuddy/5.5.6` UA + `workbuddy-desktop` 事件族）、web 指纹（`www.workbuddy.cn` + `x-client-platform: web`）。网关为每类任务构造对应指纹的判据事件链（`internal/upstream/desktop.go`）；专家类任务额外要求真实专家 id 与真实对话回执（`internal/upstream/streak.go` 之外的 expert 序列）。上报 200 ≠ 计分——面板在执行后轮询任务进度，达标即自动调用 Web 域领奖接口。

> ⚠️ 行为事件按天幂等：重复点「一键完成」不会重复扣资源，已达标的任务自动跳过。

### 🧭 任务工作台（面板「自动化」视图 · 腾讯任务段）

散落的任务能力收拢在一处（详见 [Web 管理面板](#-web-管理面板)）：

- **全账号任务扫描**：一键拉取每个账号的成长任务（未完成且可自动化的 19 项，含小程序口径的「校园日」与「小程序首对话」）+ 开学季待办，列表一目了然
- **执行队列**：把待办按账号排队执行——账号内串行（与单任务/一键完成共用互斥锁），账号间可选并发（1-3）；执行进度实时更新到每个条目
- **开学季独立状态卡**：每账号 5 任务（分享/桌面/对话×3/专家/学生认证）的状态矩阵 + 剩余抽奖次数，一键触发全账号闭环
- **日志分频道**：运行日志按「任务 / 对话 / 系统」三个频道筛选——对话流量再大，任务结果也不会被冲掉；日志条目带频道徽标与时间

### 🎒 开学季活动（5/5 全自动，活动期至 2026-09-24）

官方「AI 好 Buddy，开学有好礼」小程序活动的 5 个任务**全部纯 API 自动完成**（挂签到排程末尾，幂等）：

| 任务 | 奖励（每日） | 判据（已逆向） |
|---|---|---|
| 分享活动 | +100c +1抽奖 | `share-complete` 直调即点亮 |
| 桌面端体验（单次） | +100c +1抽奖 | viewed 激活 + 真实 chat + 桌面六事件链 |
| 和 AI 对话 3 次 | +50c +1抽奖 | viewed 后 3 条 `chat_request_send` 埋点（无需真实会话） |
| 召唤开学季专家 | +50c +1抽奖 | viewed 后 mp 事件链（召唤×3 + 对话） |
| 学生认证 | +100c | 需微信学生真实认证，不做 |

抽奖次数自动全部抽完。期间逆向成果（cf-connect 加密通道、mp 云对话全链路）记录在 `data/desktop-task-protocol.md` §8。

同一活动在成长任务中心还有两条**小程序口径**任务（`X-Client-Platform: miniprogram` 专属下发，默认列表不可见，各 +100c+5e）：

| 任务 | 判据（已逆向） |
|---|---|
| `school_season` 校园日 | mini `chat_request_send` + `activityId=school_open_day_2026`（无 activityId 不点亮；accept/claim 均要求 mp 头） |
| `Sequential_Tasks_1` 小程序首对话 | mini `chat_request_send`（无 activityId，服务端按 source=mini_program 指纹关联） |

任务中心扫描自动合并 mp 口径待办；accept 带**登记回读验证**（上游存在 200+OK 但未落账的形态，未生效自动重试一次）。

### 连登兑换与抽奖（自动）

成长中心连登档位（连续登录 7/14/28 天）兑换后发放积分 / 能量 / 补签卡 / **抽奖次数**，抽奖次数只能从兑换获得。网关把它挂在每日签到排程末尾自动跑闭环（见[定时任务](#定时任务)）：档位解锁当天自动兑换、有抽奖次数自动抽完，全程无需人工盯。

## 🧩 外部平台账号池

腾讯池之外的积分型平台统一收在 `data/ext-accounts.json`，面板「自动化 → 外部平台」管理，每天 10:00 自动签到，也可一键全部签到。

| 平台 | Provider | 入池方式 | 凭据字段 | 签到 |
|---|---|---|---|---|
| **小浣熊**（商汤） | `raccoon` | 🟢 **微信扫码登录** | `access_token` · `refresh_token`（**到期自动续期并回写**） | 登录奖励 |
| **Qoder**（阿里） | `qoder` | 🟢 **设备授权登录** | `access_token` · `machine_id` · `refresh_token` · `security_oauth_token` | 双通道领取（campaigns → activity claim） |
| **QClaw**（腾讯） | `qclaw` | 🟢 **微信扫码登录** | `access_token`（sk key，对话用）+ `refresh_token`（JWT）+ `guid` | 无（网关直连通道） |
| **AutoClaw**（智谱） | `autoclaw` | 🟢 **手机号短信登录**（国内版） | `token` + `refresh_token`（**单飞续期**）+ `region` | 无（网关直连通道） |
| **Cline** | `cline` | 🟢 **设备码授权** | `access_token`（含 `workos:` 前缀）+ `refresh_token`（**单飞续期**） | 无（网关直连通道，**含免费池**） |
| **GitHub Copilot** | `copilot` | 🟢 **设备码授权** | `github_token` + `copilot_token`（自动续期） | 无（网关直连通道） |
| **LobsterAI**（有道） | `lobsterai` | ⚪ 手工填写 | `access_token`* · `uuid`* · `refresh_token` · `first_key_from` | 每日签到 |
| **CodeArts**（华为云） | `codearts` | ⚪ 手工填写 | `access_key_id`* · `secret_access_key`* · `security_token`（仅临时凭据需要） | 每日签到 |

\* 为必填项。

### 登录入池（不用再去客户端里手抄 token）

三个平台在**协议层**就实现了登录，面板把它们接上了——选到平台直接点按钮，浏览器/手机上完成一步，账号自动落库：

| 平台 | 交互 |
|---|---|
| **小浣熊** | 点「微信扫码登录」→ 面板出二维码 → 手机微信扫一扫并确认 → 自动入池 |
| **Qoder** | 点「浏览器授权登录」→ 打开 PKCE 授权链接完成登录 → 面板轮询取 token → 自动入池 |
| **GitHub Copilot** | 点「开始授权」→ 拿到形如 `ABCD-1234` 的设备码 → 在 `github.com/login/device` 输入 → 自动入池 |
| **Cline** | 点「开始授权」→ 设备码 → 在 `authkit.cline.bot/device` 输入 → 自动入池（**免费池无需订阅**） |
| **AutoClaw** | 填手机号 → 发验证码 → 填 6 位码 → 自动入池（国内版；国际版上游已关短信入口） |
| **QClaw** | 点「微信扫码登录」→ 扫码确认 → 把回调地址里的 code 贴回来 → 自动入池 |

统一接口（面板 Bearer 鉴权，可脚本化调用）：

```
POST /panel/api/ext/{provider}/login/start → {mode, session, qr_url|auth_url|user_code, ...}
POST /panel/api/ext/{provider}/login/poll  → {done:false, status} | {done:true, account}
```

轮询期上游抖动只会保持 `pending` 继续轮询，不会把整个登录判死；会话用后即焚，重复 poll 返回 404。

### 手工填写（无登录协议的平台）

**LobsterAI / CodeArts** 没有可复用的登录协议，走逐字段表单（按该平台的凭据结构生成，必填项留空会被拦住，不用手写 JSON）：

- 也支持在「自动化 → 外部平台」页内直接添加（同一套表单），或用底部的「高级：粘贴完整凭据 JSON」批量导入
- **CodeArts 建议用永久 AK/SK**：`Security Token` 留空即可（网关会**省略** `x-security-token` 头，与华为云永久密钥的签名口径一致）。临时 STS 凭据几小时就过期，放进池里等于每隔几小时重填一次
- **Qoder 的 `machine_id` 必填**——缺失会被上游直接拒绝

> 💡 LobsterAI 的登录是绑在 Electron 客户端里的本地 OAuth 回调流程，还需要 `uuid` / `first_key_from` 等客户端渠道字段，无法在网关侧复刻；CodeArts 的 AK/SK 本就是在华为云控制台创建的，没有可自动化的「登录」这一步。这两个平台保持手工填写是设计选择，不是没做完。

## 🤖 Z.AI / ZCode 账号池

把 Z.AI（智谱 GLM）的 **Coding Plan 订阅账号**与 **API Key** 收进同一账号池，对外仍是同一个 OpenAI 兼容接口——模型名带 `zai:` 前缀即可（`zai:GLM-5.3`、`zai:glm-5.2` 等，小写别名自动映射到官方大小写敏感名）。

### 两条通道

| 通道 | 端点 | 鉴权 | 额度来源 | 验证码 |
|---|---|---|---|---|
| **Plan**（JWT） | `zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages` | `Bearer <三段 JWT>` | Coding Plan 订阅 | **必需** |
| **回退**（API Key） | `api.z.ai/api/anthropic/v1/messages` | `x-api-key` | 充值 / 兑换额度 | 免 |

一个账号可同时持有两者：OAuth 登录会**自动把 JWT 兑换成 API Key 一并入池**，JWT 额度耗尽或 429 耗尽时无缝回落，不需要人工干预。

### 添加账号（三种方式）

1. **OAuth 免密登录**（推荐）：面板「自动化 → Z.AI」→ OAuth 免密登录 → 浏览器完成登录 → 自动入池（同时兑换回退 Key）
2. **手动粘贴**：Coding Plan JWT（三段点分）或 API Key
3. **旧配置**：`schedule.zai.zai_keys` / `bigmodel_keys` 在账号池为空时自动导入为账号

### 验证码：外部求解器契约

Plan 通道每次调用需携带阿里云无痕验证参数。本仓库**不内嵌求解器实现**（求解需要在模拟浏览器里跑官方 SDK），只约定契约：

```
<command> <solver.js> <sceneId> <region> <prefix>
  → stdout 打印 VERIFY_PARAM=<param>
```

把 `schedule.zai.captcha_solver` 指向任意满足该契约的求解器即可（例如 zcode2api 的 `captcha_node/solver.js`，首次先 `npm install`）。**不配置时 Plan 通道自动降级**，账号自带的 API Key 照常工作。

验证参数有效期约 2 分钟，网关维护一个预解池（水位 `captcha_pool_min/max`）在后台补货，热路径直接从池里取（亚毫秒）；上游返回验证码挑战时整池作废——那批参数很可能已被风控标记，复用只会连环失败。

### 账号状态机

```
ACTIVE ──额度用完(402/quota)──▶ EXHAUSTED（定期再探，恢复即回 ACTIVE）
  │  ──5xx 重试耗尽──────────▶ COOLING（冷却 300s）
  │  ──401/403(非验证码)─────▶ INVALID（凭证失效，需重新登录）
  └──3012/405 真风控─────────▶ DISABLED（保护资产，须人工确认恢复）
```

**429 不冷却账号**：按 `Retry-After` 原地等待重试，耗尽后换号，账号保持可用——限流不是账号的错。验证码挑战（`code 3007`）同样是**换一枚参数原地重试**，不会把账号判死。

### 额度、领取与指纹

- **额度**：`billing/current` + `billing/balance` + `usage` 三查询，同模型多窗口（日窗 + 一次性赠送）**相加合并**；全窗口归零且无生效赠送 → 标记额度用完，恢复即回可用（冷却期不提前解除、风控禁用绝不因额度数字复活）。后台默认 5 分钟错峰刷新
- **限时套餐领取**：激活上报（`app_launch` / `app_daily_active`）→ `billing/preview` → 逐个 `billing/claim`；**1005「今日名额用完」按上游 `next_at` 退避**（等待期不再打 claim，preview 照常发现新套餐）。默认 10 分钟一轮，面板也可手动「领取套餐」
- **设备指纹**：每个账号独立一套桌面形态（`darwin-arm64` / `win32-x64` 等真实 SKU + 独立 `device_mid`），**一号一台**——多账号共用设备形态是上游的关联信号。面板可「换指纹」重发

> ⚠️ 上游对 billing 族接口的连续查询敏感（WAF 会拦）。额度轮询与领取轮都做了错峰，**不建议把间隔调得过密**。

## 🐙 GitHub Copilot 通道

把 GitHub Copilot 订阅接进同一个 OpenAI 兼容接口——模型名带 `copilot:` 前缀即可（`copilot:gpt-4o`、`copilot:claude-sonnet-4` 等，具体名单由上游按订阅等级下发）。

这条通道的**协议翻译成本为零**：`api.githubcopilot.com` 上游本身就是 OpenAI 格式，网关只做鉴权与透传，不重写请求体、不转换 SSE。

### 三段式协议

| 步骤 | 端点 | 说明 |
|---|---|---|
| 1. 设备流 | `github.com/login/device/code` → `github.com/login/oauth/access_token` | 浏览器授权，拿到长期 GitHub token |
| 2. 兑换 | `api.github.com/copilot_internal/v2/token` | GitHub token → **Copilot token（约 25 分钟过期）** |
| 3. 对话 | `api.githubcopilot.com/chat/completions` | OpenAI 原生协议 + `Copilot-Integration-Id: vscode-chat` |

`client_id` 用的是官方 Copilot 扩展的**公开**应用标识（所有用户相同，非秘密），因此无需自行注册 OAuth App。

### 添加账号

面板「添加账号 → 外部平台 → GitHub Copilot」→ 点「开始授权」→ 把设备码（形如 `ABCD-1234`）输入 `github.com/login/device` → 页面自动轮询完成入池。也可在「自动化 → 外部平台」里添加。

授权成功后凭据落进 `data/ext-accounts.json`（provider `copilot`），账号卡片显示订阅类型与 token 剩余有效期。

### 自动续期与换号

- **到期前 3 分钟自动续期**：用长期 GitHub token 换新的 Copilot token，续期结果写回账号表（下一个请求直接可用）
- **401/403 强制续期重试一次**：token 被上游提前失效时不直接失败
- **失败换号**：多账号时逐个尝试，某账号 429 / 5xx / 凭据失效只跳过该账号，不影响整体可用性

### 网络要求（重要）

设备流的两个端点都在 **`github.com`（网页域）** 上，而 `api.github.com` 是另一个域名。**实测国内多数网络下 `github.com` 直连 100% 超时，走本地代理 2–3 秒稳定成功**——不配代理，「开始授权」会卡在申请设备码这一步。

在 config.json 里给本通道单独配代理：

```json
{ "schedule": { "copilot": { "proxy": "http://127.0.0.1:2080" } } }
```

**只作用于 Copilot 通道**——腾讯 / 讯飞 / 智谱 / 阿里等上游保持直连。刻意不做全局代理：把整条网关的出口绑到一个代理进程上，代理一挂就全站不可用。

留空则跟随环境变量 `HTTPS_PROXY`，再不行直连。地址必须带 scheme（`http://`），写裸 `127.0.0.1:2080` 会被拒绝并提示——否则代理静默不生效，用户以为配了却还是连不上。

想先验证连通性，跑一次真实冒烟（只申请设备码，不授权）：

```bash
COPILOT_LIVE=1 go test ./internal/extprovider/copilot/ -run Live -v
```

> 💡 实测细节：设备码刚下发的一小段窗口内，GitHub 可能回 `incorrect_device_code`（尚未生效），随后才转为 `authorization_pending`。网关把前 3 次当作「暂未生效」容忍，避免用户刚点授权就被判失败。
>
> 另外，拿到 GitHub token 却换不到 Copilot token（该账号没有 Copilot 订阅）是**终态**——网关会直接把原因报出来，不会一直转圈等一个不会发生的结果。

### 与签到型平台的差异

Copilot **不是积分平台**：没有每日签到、没有余额。它在外部账号列表里按「订阅状态 + token 有效期」呈现，卡片上不提供「签到」按钮。

## ⚡ Cline 通道（带免费池）

Cline（cline.bot）接进同一个 OpenAI 兼容接口——模型名带 `cline:` 前缀即可。**免费池不需要订阅**，登录就能用。

### 计费池：前缀就是选择器

Cline 上游按模型名前缀分池，`cline:` 之后**保留**上游的池前缀：

| 模型名 | 池 | 说明 |
|---|---|---|
| `cline:cline-free/deepseek-v4.1-flash` | 免费池 | 无需订阅 |
| `cline:cline-pass/glm-5.3` | ClinePass 订阅池 | 需订阅 |
| `cline:cline-cloud/kimi-k3` | 云端池 | — |

所以出站时**只剥 `cline:`**，池前缀原样带给上游——它正是用来选池的（与其它通道「剥掉前缀」的惯例相反，别想当然）。

### 四步登录，第三步不能省

| 步骤 | 端点 |
|---|---|
| 1. 设备码 | `POST api.workos.com/user_management/authorize/device` |
| 2. 轮询 | `POST api.workos.com/user_management/authenticate` |
| 3. **登记** | `POST api.cline.bot/api/v1/auth/register` —— WorkOS 令牌换 Cline 会话令牌 |
| 4. 对话 | `POST api.cline.bot/api/v1/chat/completions` |

第 3 步省掉的凭据**看起来正常但发请求会被拒**——这是本通道最容易踩空的地方，网关在协议层内完成了它。

面板「添加账号 → 外部平台 → Cline」→ 点「开始授权」→ 拿到设备码 → 在 `authkit.cline.bot/device` 输入确认 → 自动入池。

### 三个「踩空后表现很像没权限」的口径

- **token 必须带 `workos:` 前缀**：续期接口返回的是裸 JWT，网关统一补齐
- **必须带 `X-CLIENT-TYPE: cline-sdk`**：不带时**免费池模型一律 403**（`only available via Cline product surfaces`），看起来像没订阅
- **续期用 `grantType`（camelCase）**，不是 OAuth 标准的 `grant_type`

### 续期单飞

Cline 的 `refresh_token` 是**一次性轮换**语义：并发请求同时发现临期时若各自去打一次续期，后到的那次会拿着已作废的 token → 401 → **用户被踢下线**。网关对同一账号的续期做了单飞，并发请求复用同一次结果。

### 网络

实测 `api.cline.bot`（~4s）与 `api.workos.com`（~0.8s）在国内**可直连**，一般不需要代理。出口受限时可用 `schedule.cline.proxy` 单独放行，不必把整条网关绑到代理上。

## 📚 参考项目（调研记录）

写这个项目时系统看过三个同类网关，记下它们的做法与我们的取舍：

| 项目 | 值得看的地方 | 我们做了什么 |
|---|---|---|
| **[aimod-cc/agent2api](https://github.com/aimod-cc/agent2api)** | 支持的通道最多（WorkBuddy / 小浣熊 / CatPaw / AutoClaw / Qoder / Cline / Accio / CodeArts / Trae）；每家一个 adapter，协议事实写得极细（含「踩空后表现很像没权限」这类口径） | 按它的公开协议**核对并实测**后接入了 **Cline**（含免费池）与 **AutoClaw**（智谱，手机号登录）；续期单飞的做法也来自它的 `refresh_flight` |
| **[wicm84266964/Buddy2api](https://github.com/wicm84266964/Buddy2api)** | 按它的公开协议接入 **QClaw**（腾讯，微信扫码登录）|
| **[wicm84266964/Buddy2api](https://github.com/wicm84266964/Buddy2api)** | QClaw / 千问办公 / TraeWork 三个通道；模型容量发现（`context_window` / `max_output_tokens` + `capacity_source` 标记来源是目录还是兜底）；聚合响应的完整性校验（缺完成标记不当作正常 stop） | 容量发现我们已有（四级查找链 + 探测上限）；`capacity_source` 式「标注数据来源」的思路值得后续补 |
| **[wangliangdong/loomy2api](https://github.com/wangliangdong/loomy2api)** | Loomy **Web 版**（非桌面客户端）；**额度获取与路由解耦**——定时刷新写缓存，选号只读缓存，绝不在请求路径上打上游额度接口；多客户端会话头的兼容顺序 | 额度刷新与选号本就是分离的；会话键提取的兼容顺序我们已有（`conversation_id` + 内容回退） |

**还没做的**：QClaw / 千问办公 / TraeWork / CatPaw / Accio / Trae 这些通道需要各自的桌面客户端登录态（或 DPAPI 解密），本机没有对应客户端、也没有账号，无从验证；盲目照搬会交付不能用的代码。上表第二列记着入口，将来有环境时可以按 Cline / AutoClaw 的方式（读公开协议 → 本机实测核对 → 写实现 + 测试）逐个补。

> 各项目的许可证不同（agent2api 是 MIT + 附加使用声明，loomy2api 声明「仅供个人学习自用」）。这里**只取协议事实**（端点、头、字段名——事实不受版权保护）与设计思路，实现全部为本仓库自写。

## 🦞 AutoClaw 通道（智谱 autoglm）

AutoClaw（智谱的桌面 Agent）接进同一个 OpenAI 兼容接口——模型名带 `autoclaw:` 前缀（`autoclaw:glm-5.3` 等）。

### 两个地区，两套域名

同一套客户端代码的两个构建，协议与客户端指纹**逐字相同**，只有站点不同：

| | userapi（账号 / 刷新 / 目录） | LLM 代理 |
|---|---|---|
| **国内版** | `autoglm-acceleration-api.zhipuai.cn` | `…/autoclaw-proxy/proxy/autoclaw` |
| **国际版** | `autoglm-api.autoglm.ai` | 同上路径 |

地区是**凭据的属性**（一个账号只属于一个站点），随凭据持久化，选号时按各自凭据里的地区打对应域名。

### 手机号登录（国内版，全自动）

面板「添加账号 → 外部平台 → AutoClaw」→ 填手机号 → 发验证码 → 填 6 位码 → 入池。

```
POST {userapi}/userapi/v1/agent-send-code  {"phone","source_id":"autoclaw","device_id"}
POST {userapi}/userapi/v1/agent-login/     {"phone","code","platform":"web","source_id","device_id"}
```

`device_id` 由网关生成并在两步之间透传——上游把设备与登录会话绑定，两步用不同的值会登录失败。

> 国际版**上游已关闭短信入口**（主登录是 Zai/Google OAuth，且被阿里云风控验证码挡着）。国际版账号需从桌面端导入或手工填写凭据。

### 三处「踩空后表现很像没权限」的口径

- **`X-Version` 是模型目录的版本门控**：不带它时上游只下发 3–4 条模型（缺 `glm-5.3-flash` 等），看起来像账号没有这些模型
- **userapi 域要 `X-Harness-Type: zcode`，chat 域不能带**：上游对 `/chat/completions` 上的这个值区别对待（403 pay-view / 406）。两条链路刻意不一致，别「统一」掉
- **签名 `X-Auth-Sign = MD5("{appId}&{ts}&{appKey}")`，ts 是秒**：签名错时上游回 `code 400002`，网关自动降级到 `agent-refresh`

appId/appKey 是**客户端指纹**而非我们的密钥（内嵌在官方客户端里，两地相同），所以硬编码是正确的——做成可配置只会让签名对不上。

### 续期单飞

与 Cline 同源的问题：AutoClaw 服务端每次刷新会**轮换 `refresh_token`**，并发刷新会互相作废并把人踢下线。两条通道现在共用同一份单飞实现（`internal/server/singleflight.go`）。

## 🐧 QClaw 通道（腾讯）⚠️ 上游已宣布停运

> **⚠️ 这条通道正在失效，接之前先看这里。**
>
> 腾讯 2026-09-24 公告：QClaw **即日起停止新用户注册，2026-12-24 00:00 正式停止运营**。
> 实测**扫码登录接口现在返回 `21004 鉴权不通过，请升级最新版本`** —— 新登录入口已被上游关闭
> （存量账号在停运前是否还能用，取决于上游是否保留既有会话）。
>
> 代码按公开协议完整实现并测过（协议层 16 个用例 + 桥接 10 个），但**登录这一步目前打不通**。
> 保留它是为了：① 存量账号可能仍可用；② 协议实现可作为同类腾讯系通道（JPRX 信封 + 微信扫码）的参考。
> 如果你没有存量 QClaw 账号，**这条通道可以忽略**。

QClaw（腾讯的桌面 Agent）接进同一个 OpenAI 兼容接口——模型名带 `qclaw:` 前缀。

### 两条链路，两个域

| | 端点 | 用途 |
|---|---|---|
| **JPRX 业务域** | `jprx.m.qq.com/data/{cmd}/forward` | 登录 / 建 key / 模型列表 |
| **AIZone 对话域** | `mmgrcalltoken.3g.qq.com/aizone/v1/chat/completions` | OpenAI 兼容对话 |

### 微信扫码登录

面板「添加账号 → 外部平台 → QClaw」→ 点「微信扫码登录」→ 面板出二维码 → 微信扫码确认 → **把跳转后地址里的 code 贴回来** → 入池。

```
4050 wx_login_state  {guid}               → {state}
   浏览器打开 open.weixin.qq.com/connect/qrconnect?appid=…&state=…
4026 wx_login        {guid, code, state}  → {token(JWT), user_info, …}
4055 create_api_key  {}                   → {key: "sk-…"}
```

**对话用的是建出来的 sk key（Bearer），不是 JWT** —— 这是本通道最容易搞错的一处。

> 微信把授权码回给腾讯自己的回调域名，网关截不到，所以最后一步必须由用户把 code（或整条回调 URL）贴回来。网关两种都认。

### 四处「踩空后表现很像没权限」的口径

- **`JPrx-Ctx` 是 MD5 拼接**：`rnd=<32位a-z0-9>; date=<秒>; gid=<gid>; sg=md5(body+KEY+rnd+date+gid)`。注意**先拼 body**、且时间戳是**秒**
- **对话必须带 `X-Conversation-Request-ID`**：不带时上游直接 400
- **响应要解两层信封**：`{ret, data:{resp:{common:{code}, data:{…}}}}`，`ret` 与 `common.code` 任一非 0 都是失败
- **`X-New-Token` 响应头会轮换 JWT**：拿到了必须回写

### 顺带把二维码编码器扩到 v10

QClaw 的微信授权链接有 **221 字节**，超出原先编码器的上限（v5 / 106 字节）。既然以后还会有更长的授权链接，把 `web/js/qr.js` 从 v1–5 扩到了 **v1–10**（上限 271 字节），并用 python `qrcode` 库对 **v1–v10 逐版本做了逐像素交叉验证（10/10 一致）**。

扩展时踩到三个只有高版本才会暴露的坑，都写在代码注释里了：

- **v6+ 是多纠错块**，码字必须交织（v1–5 单块才免交织）
- **v7+ 有版本信息**（18 位），左下那份的位序与右上**互为转置**
- **v10+ 的字节模式计数指示符是 16 位**（v1–9 是 8 位）——写死 8 位时前几个码字看着还对，后面全错

## 🆚 与上游的差异

本分支相对 [上游 master](https://github.com/Sliverkiss/workbuddy2api) 的增量（均已在真实多账号环境验证）：

### 新增

| 能力 | 说明 |
|---|---|
| **Web 管理面板** | `internal/panel`，前端为原生 ES 模块 + `go:embed`，零外部依赖、零构建步骤。单色玻璃设计（淡黑/牛奶白），总览 / 账号卡 / 用量与积分 / 自动化工作台（腾讯·Loomy·外部平台）/ 模型档位 / API 密钥 / 配置 / 日志；`⌘K` 命令面板；抽屉可拖拽关闭（弹簧驱动、可中断）；窄屏自适应 |
| **浏览器内 OAuth 添加账号** | 面板「添加账号」按钮完成设备授权 → 凭证落盘 → **热加载进池（免重启）**，替代命令行 `login.sh` 流程 |
| **在线配置编辑（热生效）** | 面板直接改 `config.json`：API 密钥 / `soft_rate` / 脱敏开关 / 池参数 / 任务排程**立即生效**；装配期字段（listen 等）保存后提示需重启。写入采用深合并 + 原子替换，保留未知键 |
| **积分任务体系** | 任务列表 / 接受 / 领取接口 + 面板弹窗；「一键完成」覆盖 **17 个任务**（对话 / 领养 / 桌面行为链 / 模板 / 灵感案例 / 画布 / 专家召唤 / 技能尝鲜 / 主题 / 资料库 / 夜猫子等），推进进度、等待异步计分落定后**自动领奖**，纯 API 零客户端依赖 |
| **首启自动生成配置** | 目录下无 `config.json` 时自动生成推荐配置（含 `crypto/rand` 随机 `api_key`），双击即开 |
| **粘性会话内容回退** | 客户端不发 `conversation_id` 时，用 `system + 首条 user` 哈希派生会话键（`d-` 前缀），通用 OpenAI 客户端也能享受粘性 |
| **余额后台刷新** | `schedule.balance_refresh_minutes`（默认 5）周期查余额并更新池，冷却账号余额恢复自动解冻 |
| **模型能力透出** | `/v1/models` 附带 `supported_efforts` / `default_effort` / 积分倍率 / 输入输出上限等上游真实字段 |
| **安全加固** | 常量时间密钥比较（`internal/httpauth`）、CSP 与安全响应头、UID 白名单防路径穿越、前端属性转义修复 |
| **多平台账号池** | 腾讯之外的通道统一进 `data/ext-accounts.json`：LobsterAI / 小浣熊 / Qoder / 华为云 CodeArts（每日签到 + 余额）· **Z.AI / ZCode**（Coding Plan JWT + API Key 双通道、额度合并、套餐领取、每号独立设备指纹）· **GitHub Copilot**（设备流登录、token 到期前自动续期、OpenAI 原生透传） |
| **统一入池入口** | 「添加账号」抽屉覆盖全部平台（腾讯 · Loomy · Z.AI · 外部平台 · 手动 JSON），每个平台按自己的凭据形态出表单——外部平台逐字段填写，Copilot 走设备码授权，不用手写 JSON |
| **领养前置修复** | 上游 `travelAdopt` 缺 report 前置导致领养恒失败于 `first_buddy task not completed yet`；本分支修正后实测 +300 到账（3/3 账号） |

### 同步上游

**第一轮（fork 基线 `53ee3a1` → `9a87758`，34 个提交）**：四类任务独立排程、pool 文件拆分、12153 连续计数才禁用、429 `code=6004` 模型级限流收窄、11101 不罚号、请求体 413、DeepSeek 思维链、reasoning_content 回填、Codex 指纹脱敏、系统提示词体系、出站 UA 可配等。

**第二轮（`9a87758` → `ea8b1e5`，2026-09-14，只吸收底层）**：

| 上游改动 | 吸收内容 |
|---|---|
| 净化增强 | `tool_calls.arguments` 盲区修复（content=null 的工具调用轮此前完全漏净化）、裸 `11128` 反探测改写、桌面版身份句（逗号形态）漏网修复、反馈句整句改写 |
| 出站头族 | UA 对齐官方三段式 `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<ver>`（默认 5.5.4/2.137.1，可配）；`X-IDE-*` 用量归属四头 + `X-Agent-Purpose`（`client_name` 配 `WorkBuddy` 即对齐官方桌面端）；`X-Device-Token` 设备风控头（auth 每号 / config / 文件三源）；`X-IDE-Version` 补齐 |
| 并发修复 | 客户端 IP 改按请求参数传递（消除共享字段竞态）；billing 单段 UA 形态 |
| 签到幂等 | `IsAlreadyCheckin` 识别"今天已签到"（code=10001/14001），调度日志不再把重复签到当失败 |
| 粘性按模型判活 | 会话绑定的账号被 6004 模型级限额后，换模型请求自动解绑重分配（治"限额后换不动号"）；`/healthz` 探活计入模型豁免形态（治"全号被单模型限流探活误报 503"） |
| report 增强 | `ReportChatActivity` 支持独立 `requestID`（同会话多轮上报各条可区分） |

未吸收（明确不做）：脚本体系（task_runner/school 脚本—我们已有更完整的纯 API 实现）、governance/CI workflow、成本账本选号（依赖 usage.credit 观测，收益待验证）。

### 未做 / 待办

| 状态 | 事项 | 说明 |
|---|---|---|
| ✅ 已修复 | ~~single 类任务奖励领取~~ | **领奖已打通**：正确端点是 Web 域 `POST https://www.workbuddy.cn/activity/growth/tasks/<task_code>/claim`（任务码在路径、无 body、`x-client-platform: web`）。此前误用 CLI 域 `copilot.tencent.com/v2/.../reward/claim` 导致长期 400。「一键完成」现已**达标即自动领奖**（含异步计分等待），面板也可手动领取。实测 +100 分 +5 能到账、重复领取幂等 |
| ✅ 已破解 | ~~桌面端 / 交互类任务~~ | 通过客户端指纹逆向（`/v2/report` 三通道 + 判据事件载荷），**17/18 任务可纯 API 一键完成**：`RichMeow_Chat`（桌面 6 事件链）、`Buddy_App(_QQ)`、`automation_1`、`Library_read`、`template_5`、`playbook_prompt`、`create_canvas`、`expert_5`、`Expert_team_use_3`、`Expert_lighthouse`、`Hp_Appearance`、`skill_1`、`black_cat`（夜间窗口自动补足）等，多账号实测点亮 |
| ⚠️ 不支持 | **剩余 1 个任务** | `Expert_Philanthropy`（需真实捐款：服务端领奖时校验捐赠回执，已实测无法绕过）；面板展示指引 |
| ❌ 未做 | **面板侧 Upstash / 凭证目录配置** | 涉及启动期装配，需手工编辑 `config.json`（面板会提示为重启项） |
| ❌ 未做 | **HTTPS / 内置限流** | 设计上交给反向代理（Nginx / Caddy）。服务本身只提供明文 HTTP，公网部署**必须**置于 HTTPS 反代之后 |

## 架构总览

```mermaid
flowchart LR
    Client["客户端 / SDK\nOpenAI 兼容请求"] --> H

    subgraph GWI["WorkBuddy2API 网关 :7863"]
        H["HTTP Handler\n鉴权 · 请求体上限 · 提示词改写 · 按前缀路由"] --> P
        H --> S
        H -. "loomy: / zai: / copilot: / qoder: / codex: / free:" .-> B
        P["腾讯账号池\n三因子加权 · 熔断 · 冷却 · 租约"] --> U
        S["会话粘性路由"] -.绑定镜像.-> REDIS
        T["定时调度\n签到 09/21 · 旅行 09/21 · 活跃 10 · 保活 22"] --> P
        U["上游 Client\nChatHTTP 流式 · 短 RPC"]
        B["多平台通道桥接\n各自账号池 · 自动续期 · 失败换号"]
    end

    P -. "读凭证 (0600)" .-> AUTH[("auths/*.json")]
    P -. "状态镜像" .-> REDIS[("Upstash Redis\n可选")]
    B -. "外部平台账号" .-> EXT[("data/ext-accounts.json\ndata/zai-accounts.json")]
    U -->|"chat/completions (SSE)"| CB["CodeBuddy\ncopilot.tencent.com"]
    U -->|"billing / auth / growth"| CB
    B -->|"loomy / GLM / Copilot / Qoder"| EXTUP["各平台上游\n讯飞 · 智谱 · GitHub · 阿里"]
```

模型名的**前缀就是路由协议**：无前缀走腾讯池，其余前缀分派给对应通道（见[模型路由](#模型路由前缀即协议)），各通道的凭据、续期与换号逻辑彼此独立——某个平台挂了不会波及其它平台。

上游请求在出站前经历统一的改写管线（`internal/upstream/payload.go`）：强制 `stream:true`、`developer` 角色归一、tool_choice 归一、`image_url` 字符串兼容为 OpenAI 对象形态、DeepSeek 思维链注入、`reasoning_effort` 档位降级、`reasoning_content` 回填、指纹脱敏。

## 快速开始

### 环境要求

- **Docker + Docker Compose**（服务端部署方式，镜像内已含低权限用户与全部工具脚本）——或
- **Windows / macOS / Linux 直接跑单文件二进制**（无需 Docker，见下方「Windows 单文件运行」）
- 一个或多个已注册的 CodeBuddy 账号，用于 OAuth 登录
- 宿主机 Go ≥ 1.22（仅从源码构建时需要）

### 方式〇：GHCR 镜像（免克隆免构建）

CI 会自动构建多架构镜像（`amd64` / `arm64`）并发布到 GHCR，`git clone` 之外的部署路径：

```bash
# 1. 准备配置与数据目录
mkdir -p auths data && cp config.example.json config.json
#    建议编辑 config.json 设置 api_key（或留空由程序自动生成随机密钥）

# 2. 拉取并运行
docker run -d --name workbuddy2api \
  -p 7863:7863 -e TZ=Asia/Shanghai \
  -v ./auths:/app/auths -v ./data:/app/data -v ./config.json:/app/config.json \
  ghcr.io/linguo2625469/workbuddy2api-panel:latest

# 3. 健康检查（无可用账号时返回 503）
curl -s http://localhost:7863/healthz
```

> **首次发布后须将包设为公开**：GitHub 仓库页 → Packages → `workbuddy2api-panel` →
> Package settings → Change visibility → Public，否则拉取需要 `docker login ghcr.io`。
>
> 镜像 tag 规则：`main` 分支推送 `latest` / `main` / `sha-xxxxxx`；打 `v*` tag 额外发布
> `1.2.3` / `1.2` / `1` 语义化版本；PR 仅构建验证、不推送。

### 方式一：Docker Compose（推荐服务器部署）

```bash
# 1. 克隆
git clone https://github.com/linguo2625469/workbuddy2api-panel.git
cd workbuddy2api-panel

# 2. 准备配置（compose 挂载此文件，缺失会导致容器启动失败）
cp config.example.json config.json
#    建议编辑 config.json 设置 api_key（或留空由程序自动生成随机密钥）

# 3. 启动（首次会构建镜像，约 1-2 分钟）
docker compose up -d --build

# 4. 健康检查（无可用账号时返回 503）
curl -s http://localhost:7863/healthz
# {"healthy":0,"total":0,"service":"workbuddy2api"}
```

启动后打开 **`http://localhost:7863/panel/`**，用面板「添加账号」完成登录（见下节）。

常用运维命令：

```bash
docker compose logs -f          # 跟踪日志
docker compose restart          # 重启
docker compose down             # 停止并移除容器（数据在 ./auths 与 ./data，不受影响）
```

### 方式二：Windows 单文件运行（无需 Docker）

```powershell
# 1) 下载 Release 中的 wb2api.exe，或从源码构建
go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server

# 2) 直接运行：首次启动自动生成 config.json（含随机 api_key，日志打印一次）
.\wb2api.exe -config config.json

# 3) 浏览器打开面板添加账号
#    http://127.0.0.1:7863/panel/
```

exe 为**单文件自包含**（前端资源已 embed 进二进制），拷到任意 Windows 机器即可运行，只需保证 `auths/`（凭证）与 `data/`（状态）目录可写。

### 方式三：源码运行（开发调试）

```bash
go build ./...
go vet ./...
go test ./...                      # 完整测试套件
go run ./cmd/server -config config.json
```

> 前端没有独立的 dev server：`internal/panel/web/` 下的 ES 模块与样式在构建时 embed 进二进制，**改完重新 `go build` 即可**（浏览器刷新时记得 Ctrl+F5，模块有缓存）。有 node 时 `go test ./internal/panel/` 会额外跑模块语法校验与顶层求值冒烟，无 node 自动跳过。

构建全部二进制：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o signin_bin ./cmd/signin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o login ./cmd/login
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o credit ./cmd/credit
```

### 添加账号（登录）

**方式 A：Web 面板（推荐，各平台通用，免命令行）**

打开 `http://127.0.0.1:7863/panel/`，点右上角「**添加账号**」：面板展示授权链接 → 浏览器完成登录 → 自动检测并落盘凭证 → **热加载进池（无需重启）**，顺带完成首次签到。

**方式 B：命令行脚本（仅 Linux / macOS，依赖 bash + python3）**

```bash
./login.sh
# 按提示在浏览器打开授权链接 → 回到终端确认 → 凭证落盘 auths/workbuddy-<uid>.json
```

`login.sh` 内置授权 URL 获取 + 浏览器登录 + token 轮询 + 首次签到 + 凭证落盘 + 容器重启，全程无 PKCE（state 由服务端签发）。账号池在容器启动时用 `auths/` 目录自动对齐，新增凭证文件即自动发现。

> Windows 用户请用方式 A（或 WSL）；`login.sh` 需要 python3。

### 验证

```bash
# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

# 账号状态（汇总 + 每账号详情，disabled 账号透出 disabled_reason）
curl -s http://localhost:7863/status -H "Authorization: Bearer your-api-key"

# 流式聊天
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 非流式聊天（本地聚合）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 配置说明

**`config.example.json` 是配置项最完整的参考**：每个字段、默认值与结构都能在其中找到，示例值一律是 `test_key` 之类占位符，**不含任何真实密钥**。下表为字段含义速查。

### 字段速查

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7863` | HTTP 监听地址 |
| `api_key` | 空 | 网关鉴权密钥；**空 = 不鉴权直接放行**（公网必须设置） |
| `auth_dir` | `./auths` | 账号凭证目录 |
| `state_file` | `./data/state.json` | 账号池状态持久化文件 |
| `cooldown.soft_rate` | `600s` | 软限流（429 / 限流文案）冷却基数；同一账号连续触发按 2 倍指数退避 |
| `cooldown.soft_rate_max` | `2h` | 软冷却指数退避封顶 |
| `schedule.checkin_hours` | `[9, 21]` | 每日本地时区整点签到 + 余额查询解冻。空数组 / `null` = 未配置回落默认（不是禁用） |
| `schedule.travel_hours` | `[9, 21]` | 每日本地时区整点推进猫猫旅行状态机（领养 / 派出 / 领奖） |
| `schedule.activity_hours` | `[10]` | 每日本地时区整点对话活跃上报（点亮连登 + 解锁 `first_buddy`） |
| `schedule.keepalive_hours` | `[22]` | 每日本地时区整点刷新 token 保活 |
| `schedule.blackcat_hours` | `[23]` | 每日本地时区整点夜猫子补足（23:00–08:00 计数窗口） |
| `schedule.checkin_enabled` | `true` | 签到总开关；`false` 真正关闭 |
| `schedule.travel_enabled` | `true` | 猫猫旅行总开关（独立于签到） |
| `schedule.activity_enabled` | `true` | 活跃上报总开关 |
| `schedule.keepalive_enabled` | `true` | token 保活总开关 |
| `schedule.blackcat_enabled` | `true` | 夜猫子总开关 |
| `upstream.timeout_seconds` | `120` | 短 RPC（刷新 / 签到 / 余额 / 模型列表）总时长上限 |
| `upstream.header_timeout_seconds` | 回落 `timeout_seconds` | 聊天首字节前（响应头）上限 |
| `upstream.idle_timeout_seconds` | `300` | 聊天流中空闲上限（活跃续命，静默断流） |
| `upstream.user_agent` | 空 | 出站 User-Agent 覆盖（空 = 现状 `CLI/2.63.2 CodeBuddy/2.63.2`）。官网「使用端」列按出站 UA 服务端归因；官方 WorkBuddy 桌面 UA 为 `WorkBuddy/<version>`，需要时可配 |
| `features.sanitize_blacklist_fingerprints` | `true` | 出站请求体黑名单指纹脱敏 |
| `prompt.mode` | `custom` | 系统提示词模式：`custom` = 网关用自有提示词替换客户端 system；`append` = 开头连续 system/developer 块后插网关提示词（既有消息逐字不动）；`passthrough` = 透传客户端原始 system（降级重试仍切中性提示词） |
| `prompt.file` | 空 | 提示词文件路径；空 = 内置默认（约 2KB）；路径非空但不可读 → 启动报错 |
| `upstash.url` / `upstash.token` | 空 | 空 = 纯内存模式（Noop 降级，功能照常） |
| `pool.max_in_flight` | `3` | 单账号最大在途请求数（`0` = 不限） |
| `pool.max_in_flight_global` | `2` | global 域单账号在途上限（国际版 WAF 风控更紧，压低并发） |
| `pool.degrade_threshold` | `5` | 连败降权阈值：未知错误（ErrClient/传输层）连败 N 次临时出池 |
| `pool.degrade_cooldown` / `pool.degrade_cooldown_max` | `10m` / `2h` | 连败降权时长与上限钳制 |
| `pool.cost_explore_interval` | `30m` | costTier 条件探索窗口：免费层垄断且存在未知号时，每窗口把一个真实请求搭车改道给未知号（零新增上游请求；成功即毕业，失败走既有错误策略）。`0` = 关停 |
| `pool.breaker_threshold` | `3` | 连续失败触发熔断阈值 |
| `pool.breaker_cooldown` | `30m` | 熔断基础退避时长 |
| `pool.breaker_cooldown_max` | `6h` | 熔断指数退避封顶 |
| `pool.idle_weight_per_hour` | `0.5` | 闲置补偿：每小时未使用 +0.5 权重 |
| `pool.idle_weight_max` | `5.0` | 闲置补偿权重封顶 |
| `session_sticky.enabled` | `true` | 会话粘性路由开关 |
| `session_sticky.ttl` | `30m` | 会话绑定 TTL（滚动续期） |
| `session_sticky.gc_interval` | `5m` | 过期绑定 GC 周期 |
| `schedule.zai.zai_keys` | 空 | **旧版兼容入口**：账号池为空时导入为账号；此后以账号池为准（面板管理），见 [Z.AI 账号池](#-zai--zcode-账号池) |
| `schedule.zai.bigmodel_keys` | 空 | 同上，智谱开放平台（open.bigmodel.cn）的 Key |
| `schedule.zai.accounts_file` | 空 | 账号池落盘路径；空 = 与 `state_file` 同目录的 `zai-accounts.json` |
| `schedule.zai.max_concurrency` | `2` | Z.AI 单账号并发上限（`0` = 不限；满并发账号智能跳过不排队） |
| `schedule.zai.captcha_solver` | 空 | 验证码求解器脚本路径；**空 = Plan（JWT）通道不可用**，仅走 API Key 回退 |
| `schedule.zai.captcha_command` | `node` | 求解器解释器 |
| `schedule.zai.captcha_timeout_sec` | `40` | 单次求解超时（秒） |
| `schedule.zai.captcha_pool_min` / `_max` | `4` / `12` | 预解池水位（低于 min 后台补货） |
| `schedule.zai.system_file` | 空 | Plan 通道要求的身份块 JSON；缺失时 JWT 请求可能被上游拒为 3012 |
| `schedule.zai.quota_refresh_minutes` | `5` | 后台额度刷新间隔（分钟，`0` = 关闭）。⚠️ 上游 WAF 对 billing 族敏感 |
| `schedule.zai.claim_round_minutes` | `10` | 后台套餐领取轮间隔（分钟，`0` = 关闭） |

### 上游超时语义（三段各归其位）

| 字段 | 作用对象 | 默认 | 行为 |
|---|---|---|---|
| `timeout_seconds` | 短 RPC（token 刷新 / 签到 / 余额 / 模型列表） | `120` | 总时长硬上限，到期报错走换号 / 熔断 |
| `header_timeout_seconds` | 聊天 SSE **首字节前** | `120` | 由 `Transport.ResponseHeaderTimeout` 约束；超时 = 换号重发 |
| `idle_timeout_seconds` | 聊天 SSE **流中空闲** | `300` | 活跃吐数据续命不掐；静默超时才断流释放租约 |

聊天流（`stream` true / false 均同）**没有总时长上限**：聊天使用 `Timeout=0` 的专用 client，长思考 / 长输出不会被掐断。

### 环境变量覆盖

加载顺序：JSON 文件 → `WB2A_*` 环境变量（变量非空才覆盖）：

`WB2A_LISTEN` · `WB2A_API_KEY` · `WB2A_AUTH_DIR` · `WB2A_STATE_FILE` · `WB2A_MAX_BODY_MB` · `WB2A_SOFT_RATE`(duration) · `WB2A_SOFT_RATE_MAX`(duration) · `WB2A_TIMEOUT_SECONDS` · `WB2A_HEADER_TIMEOUT_SECONDS` · `WB2A_IDLE_TIMEOUT_SECONDS` · `WB2A_USER_AGENT` · `WB2A_SANITIZE_FINGERPRINTS`(bool) · `WB2A_PROMPT_MODE` · `WB2A_PROMPT_FILE`

标准代理变量同样生效（Go 的 `ProxyFromEnvironment`，不走 `WB2A_` 前缀）：`HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY`。但**更推荐用 `schedule.copilot.proxy`** 给 Copilot 通道单独配——全局代理会把整条网关的可用性绑到代理进程上，详见 [GitHub Copilot 通道](#-github-copilot-通道)。

## 核心行为语义

### 系统提示词体系

客户端（Claude Code / Codex 等 CLI）会在 system prompt 注入固定模板句，上游内容审核按**逐字精确匹配**误杀合法流量（HTTP 400 + 审核文案）。网关提供两层防护，互不替代：

1. **提示词体系**（解决 **system / developer 来源**的误报）：由 `prompt.mode` 控制
2. **指纹脱敏**（兜底 **用户 / assistant 消息**里的指纹串）：由 `features.sanitize_blacklist_fingerprints` 控制

| 模式 | 语义 |
|---|---|
| `custom`（默认） | 出站前用网关自有提示词**替换**客户端 system / developer 消息（删除全部 system / developer，头部插入单条 system）；user / assistant / tool 消息逐字不动 |
| `append` | 开头连续 system / developer 块之后**插入**一条网关自有 system，既有消息（含客户端项目规范/工具约定）逐字不动——两者并用；降级期退化为 replace（带指纹原文重试只会确定性再撞 400） |
| `passthrough` | 透传客户端原始 system，不做改写 |

内置默认提示词约 2KB（`internal/prompt/defaultprompt.md`，嵌入二进制）。`prompt.file` 指向自定义提示词文件（自定义人格 / 人设）即整体替换内置默认；**留空 = 内置默认**，路径非空但不可读 → **启动报错**（fail fast，不会静默回落到内置默认）。

### 内容拦截误报与降级重试

`passthrough` 模式请求被上游内容策略拦截（HTTP 400 + `blocked by security policy` / `unapproved channel` / `illegal api invocation` 文案）时，判定为 system 指纹误报：**同请求内**换 Degraded 中性提示词重试一次；第二次仍被拦（用户内容本身触发审核）→ 走既有错误路径返回客户端，并如实报给调用方。

- 触发降级后持续到**次日 00:00 CST**（Asia/Shanghai）重置；降级期内 `passthrough` 请求直达中性提示词，不再先撞 400
- 降级状态是**进程内存态**，重启清零
- 内容问题非账号问题：`ErrContentBlocked` 不罚账号（无冷却 / 熔断 / 计错），由网关降级重试消化

### 错误分类与账号处置

上游错误由 `Classify` 统一分类（判定优先级：余额耗尽 → session 失效 → 限流文案 → 状态码兜底），账号处置如下：

| 分类 | 触发条件 | 账号处置 | 恢复 |
|---|---|---|---|
| 余额不足 | HTTP 402 / body 含余额关键词 | 硬冷却到**次日 04:00**（本地时区） | 签到（09/21 点）余额恢复自动解冻 |
| 频控 | HTTP 429 / 限流文案（不限状态码） | 软冷却 `soft_rate`（600s 起，连续触发指数退避，封顶 `soft_rate_max`）。**`code 6004`（模型级）带「将在 … 重置」时**冷却到上游重置墙钟并豁免切模型（见[常见问题](#429-code6004模型级限流的冷却语义)） | 到期自动恢复 / 成功清零退避 |
| Session 失效 | body 含 `Offline user session not found` / `12153` | **连续 3 次**才永久禁用（一次 12153 多为临时抖动：网络 / 闪断 / refresh 竞态）；刷新成功 / 任意成功 / 手工复活清计数 | 人工重新登录（`login.sh`）或 `ReviveDisabled` 复活 |
| 上游 404 | HTTP 404 | 软冷却固定 60s（不随 `soft_rate`、不单独退避） | 到期自动恢复 |
| 服务端错误 | HTTP ≥500 | 喂连续失败计数，达阈值熔断 | 熔断到期 / 成功清零 |
| 请求体解析失败 | HTTP 400 + `Unmarshal chat params failed` / code `11101` | **不罚账号，但仍轮转**（客户端畸形 JSON，换号照样 400） | 即时 |
| 内容拦截 | HTTP 400 + 审核文案 | **不罚账号**，`passthrough` 模式走降级重试 | 即时 |
| 客户端错误 | 其余 4xx / 业务 `code≠0` | 不处罚，换号重试 | 即时 |

请求体解析失败（`11101`）与内容拦截一样**不罚账号**：问题在请求内容而非账号健康。网关不做请求体截断与预拦截，`11101` 均为客户端发来的畸形 JSON。

**熔断器**：所有冷却入口与 5xx 共用唯一连续失败计数器 `fails`；累计达 `breaker_threshold`（默认 3）触发熔断，退避 `breaker_cooldown × 2^retryCount`，封顶 `6h`；成功清零。

**软冷却指数退避**（与熔断器并存的第二条升级线）：软限流的**冷却时长**本身也按连续次数退避——同一账号连续触发软冷却时 `soft_rate × 2^(连续次数-1)`，封顶 `soft_rate_max`。计数 `soft_streak` 独立于熔断器的 `fails`，只在**成功**或**签到解冻**时清零，随 `state.json` 持久化。

### 选号策略

1. 过滤：禁用 / 冷却 / 熔断 / 在途占满账号不参与
2. 取 **Top-5** 候选（按三因子权重降序，积分只是因子之一）
3. 三因子加权随机：

   `weight = credits 比例 ×10 + idleWeight + successRate ×3`

   - `credits 比例` = 该号积分 / 候选集最大积分
   - `idleWeight` = `min(闲置小时 × idle_weight_per_hour, idle_weight_max)`，从未使用给满分
   - `successRate` = `successCount/(successCount+errTotal)`，无记录给中性 1.5
4. 防惊群：跳过 100ms 内刚被选中的账号；全冷却时从非禁用、非余额耗尽的软冷却 / 熔断账号中选最早到期者顶班

### 会话粘性

同一会话尽量复用同一账号，多轮对话不跳号：

- 会话键提取顺序：`metadata.conversation_id` → `metadata.conversationId` → `metadata.user_id` → 顶层 `conversation_id` → 顶层 `conversationId`（snake_case 优先于 camelCase）
- TTL 滚动续期（默认 30m），GC 周期 5m；绑定可镜像到 Redis（7 天 TTL）防重启丢失
- 请求失败自动解绑；成功后绑定跟随最终成功账号

### 定时任务

五类任务各自独立排程、各有开关，互不影响。容器时区由 `TZ` 控制（compose 默认 `Asia/Shanghai`）。

| 任务 | 开关（默认 true） | 时刻（默认） | 行为 |
|---|---|---|---|
| 签到 | `schedule.checkin_enabled` | `checkin_hours` `[9, 21]` 整点 | 签到 + 余额查询；余额恢复则解冻冷却账号。**末尾追加连登管家**（见下） |
| 活跃上报 | `schedule.activity_enabled` | `activity_hours` `[10]` 整点 | 对话活跃上报（`chat_request_send` 事件，必须含 `userId`）；点亮连登 + 解锁 `first_buddy`；每号每天 1 次 |
| 猫猫旅行 | `schedule.travel_enabled` | `travel_hours` `[9, 21]` 整点 | 独立排程：无猫领养 / `idle` 派出 / `arrived` 领奖 |
| 保活 | `schedule.keepalive_enabled` | `keepalive_hours` `[22]` 整点 | 全账号刷新 token；session 失效**连续 3 次**才自动禁用 |
| 夜猫子 | `schedule.blackcat_enabled` | `blackcat_hours` `[23]` 整点 | **先查任务进度再决定**：`black_cat` 未达标才在 23:00–08:00 计数窗口内补足 glm-5.2 短对话（每天 1 次累计 3 天，漏跑次日窗口自动补） |

#### 连登管家（签到排程末尾自动执行）

成长中心的连登档位（连续登录 7/14/28 天）兑换后发放积分 / 能量 / 补签卡 / **抽奖次数**，抽奖次数只能从兑换获得。管家在每日签到后自动跑一遍闭环（幂等，未解锁静默跳过）：

1. 查连登档位状态 → 已解锁（非 locked / 非 claimed）的档位自动**兑换**
2. 查抽奖次数 → **有次数自动全部抽完**，奖品记日志（`streak-bonus <uid>: 🎲 …`）

无需配置，跟随签到排程；到天数那天自动完成「兑换 → 抽奖」，无需人工盯。

**关闭定时任务**：用 `schedule.*_enabled: false` 显式关闭（四个都设 `false` 则调度器不空转，直接阻塞等待退出信号）。注意两点语义：

- **空数组与 `null` 表示「未配置 → 回落默认」**，不是「禁用」；真正关闭请用 `*_enabled: false`
- **禁用不会擦除小时配置**：`*_hours` 原样保留，改回 `true` 即恢复原时点；小时值必须是 0-23，非法值启动即报错
- 关签到会把「余额恢复即解冻」一起关掉，被硬冷却的账号只能等次日 04:00 自然到期

#### 活跃上报（独立排程）

对池内每个可用账号在 `activity_hours`（默认 `[10]` 整点）发送一条对话活跃上报（事件 `chat_request_send`，body 为数组，事件必须含 `userId`）：

- 一条上报同时点亮 growth 连登 + 解锁 `first_buddy` 任务（领养前置）
- 每号每天 1 次即可（单时点）：日活跃奖励按天去重，重复上报无额外收益
- `conversationId` 由网关生成（`wb2api-<ms>`），无需真实会话
- 限速：账号间间隔 800ms（与旅行同口径）
- **streak 自检**：上报成功后回读连登天数（只读 oracle），日志每号一行可 grep：`activity <uid>: streak days=N`。`days=0` 记 **warn**（`report OK but streak.days=0 (silent drop?)`，对应上游「200 但静默丢弃」）；回读失败记 warn 但不影响主流程（上报按天幂等，不重试，只观测）
- 手动诊断 / 补跑用 `python3 scripts/probe_active.py`（只读探测；写操作默认 dry-run，需 `--yes`）

#### 猫猫旅行（独立排程）

对池内每个可用账号在 `travel_hours`（默认 `[9, 21]` 整点）单趟推进一次，每趟只做一个动作，不轮询不等待。默认两趟闭环：9 点领昨日到站奖励并派出，21 点领当日到站奖励（`daily_limit_reached` 自动挡住二次派出）。

| 探测结果 | 动作 |
|---|---|
| 无猫（`buddy` 为 `null`） | 先同意协议（幂等），再尝试领养；过门槛则 +300 积分并获得猫 |
| `state=idle` 且今日未派出 | 派出 `location_id=4`（古镇客栈；4 个地点收益 / 时长区间相同，无最优解） |
| `state=arrived` | 领取到站奖励（带 `record_id`） |
| `state=traveling` / 今日已达上限 / 未知状态 | 跳过 |

- 领养门槛未达标时上游返回 HTTP 400，每账号每自然日只尝试一次（跨日重试，记录仅存内存）；门槛可用活跃上报解除
- 限速：账号间间隔 800ms
- 每自然日 1 次派出：按 CST（Asia/Shanghai）自然日重置，与容器 `TZ` 无关
- 失败隔离：单账号失败只跳过该账号当趟；401 不强刷（token 刷新交保活时点）

## API 端点

**余额后台刷新**（`schedule.balance_refresh_enabled`，缺省开启）：每 `balance_refresh_minutes`（缺省 5）分钟并发查询全部账号余额并更新池内积分——两次签到时点之间 credits 保持新鲜，余额恢复的冷却账号也会自动解冻（语义同签到，但不做签到不刷 token）。面板「立即刷新」按钮也是全量刷余额；5 秒自动轮询只读内存，不打上游。

## 🖥️ Web 管理面板

内嵌式管理面板（`internal/panel`），服务启动后访问：

```
http://127.0.0.1:7863/panel/
```

鉴权与 API 同口径：`api_key` 非空时面板要求输入一次密钥（浏览器 localStorage 记住）；为空则直接可用。

### 前端形态

**原生 ES 模块，无构建步骤**——源码 `internal/panel/web/` 经 `go:embed` 打进二进制，改前端只需重新 `go build`，不需要 node / npm / 打包器：

```
web/
  index.html          壳层（唯一的 <script type="module"> 入口）
  css/tokens.css      设计令牌：单色色板 / 动效曲线 / 字号阶梯 / 可访问性媒体查询
  css/glass.css       玻璃基元：壳层 · 按钮 · 卡片 · 控件 · 抽屉 · 弹层 · 状态条
  css/views.css       视图专属样式
  js/kernel.js        响应式微内核：signal/effect · DOM 构建 · 弹簧 · 抽屉 · Toast
  js/shell.js         侧栏 / 顶栏 / 状态条 / 命令面板 / 路由
  js/store.js         共享状态与主题
  js/qr.js            离线 QR 编码器（券码二维码，CSP 下不引外链服务）
  js/drawers.js       抽屉：添加账号（五个平台分段）/ 账号任务 / 券码
  js/views/*.js       八个视图 + 分段模块（zai-segment / ext-add），各自声明 render()
```

内核约 250 行、零依赖：`signal()` 是可读写响应式值，`effect()` 自动追踪依赖——**视图就是一个返回 DOM 节点的函数**，渲染期间读到的信号变化时框架自动换掉旧节点；DOM 由 `h()` 声明式构建而非字符串拼接，注入类问题在结构上不存在。

### 设计语言

**单色玻璃**：全站只有「淡黑」与「牛奶白」两个色相，靠明度、透明度与材质分层；状态语义用**形状**而非颜色表达（实心点 = 可用 · 空心环 = 冷却 · 虚线环 = 停用）。面板与抽屉以 `backdrop-filter` 近似 Apple 的材质层，按钮是透明玻璃 + 一道扫过的流光。

交互遵循 Apple 的流体界面原则（弹簧用 damping/response 两个参数，而不是时长）：

- **可中断**：抽屉在关闭动画飞行中可被抓住——从当前屏幕值冻结、带着速度反转
- **1:1 跟手**：拖动与指针同步并尊重抓取偏移；横向占优才接管手势，否则放行给内容滚动
- **速度交接**：释放速度交给弹簧，拖动与动画之间没有接缝
- **动量投影**：轻甩即关（由速度投影落点决定去留，而非拖过半屏）；边界处阻尼渐阻而非硬停
- **键盘动作不设动画**：`⌘K / Ctrl+K` 命令面板零动画——高频动作上任何动效都只会显得迟钝

### 视图

侧栏分四组八个视图；`⌘K / Ctrl+K` 可搜索跳转，或直接执行批量动作（全部签到 / 保活 / 旅行巡检 / 活跃上报 / 刷新余额 / 切换主题）：

| 视图 | 功能 |
|---|---|
| **总览** | 池健康瓷贴（总数 / 可用 / 冷却 / 禁用 / 积分剩余与总额 / 粘性会话）+ 批量操作 + 平台分布 + 最近动态 |
| **账号** | 两个视角：**账号池**（账号卡：状态、积分量条、成功失败、上次用量；单号操作签到 / 余额 / 任务 / 解冻 / 禁用 / 移除）与**全部平台**目录（跨平台只读总览，可跳各平台管理页） |
| **用量与积分** | Token 用量（时序堆叠图 + 按账号 / 模型 / 域三张表，支持窗口与平台筛选）与积分构成（逐包对比：来源、面额、剩余、发放与到期） |
| **自动化** | 四段工作台：**腾讯任务**（扫描待办 → 排队执行，账号内串行、账号间并发 1–3；开学季券码查询）· **Loomy**（新手之旅 8 项一键完成、每日签到、积分余额）· **外部平台**（LobsterAI / 小浣熊 / Qoder / 华为云 CodeArts / GitHub Copilot 账号卡 + 一键签到 + **逐字段添加账号**）· **Z.AI**（Coding Plan 账号池：OAuth 免密登录 / 手动 JWT / API Key、额度与用量、套餐领取、设备指纹换发，见 [Z.AI 账号池](#-zai--zcode-账号池)） |
| **模型与档位** | 实时查询上游：积分倍率（牌价 vs 生效价）、默认思考档、支持档位（含「off（可关）」）、上下文长度与最大输出；有探测数据时显示**实测上限与钳制告警**（见「探测模型真实输出上限」） |
| **API 密钥** | 生成 / 删除多把 Key，每把可授权平台子集（留空 = 全平台）；列表展示密钥与授权范围 |
| **配置** | 在线编辑 config.json：API 密钥、定时任务（四类任务时点与开关、余额刷新间隔）、账号池与流量治理参数、上游超时与 UA、提示词模式、脱敏 / 粘性开关 |
| **运行日志** | 最近 500 行服务日志 + 请求表格日志；按「任务 / 对话 / 系统」频道筛选，自动滚动可开关 |

**添加账号**（顶栏按钮）是右侧抽屉，**覆盖全部平台**——五个分段：腾讯 OAuth 设备授权（显示授权链接 + 自动轮询，凭证落盘后**热加载进池，免重启**）· Loomy（客户端自动检测 / 手机号密码 / 短信验证码 / 手动 Token）· Z.AI（OAuth 免密登录 / JWT / API Key）· 外部平台（**小浣熊微信扫码 · Qoder 设备授权 · Copilot 设备码** 三种一键入池，LobsterAI / CodeArts 逐字段填写，有登录方式的平台保留「手动填写凭据」折叠区兜底）· 手动 JSON 批量导入。账号卡上的「任务」同样以抽屉展开：全部任务的进度、奖励与状态，支持「全部接受」与**一键完成**（覆盖 17 个任务，推进进度 + 异步计分等待 + 自动领奖，幂等可重复点）。

**移动端**：窗口窄于 900px 时侧栏收成顶部横滑玻璃条；`100dvh` 与安全区（`env(safe-area-inset-*)`）避免地址栏 / 刘海裁切；触屏输入框字号 16px 防 iOS 聚焦缩放；悬停效果一律限定在精确指针设备上。

**可访问性**：响应三个独立信号——`prefers-reduced-motion`（去掉位移与缩放，保留 160ms 交叉淡入，而非一刀切禁用全部动效）、`prefers-reduced-transparency`（玻璃转实底并去掉模糊）、`prefers-contrast: more`（描边与次级文字对比度提升）。

**前端测试**（Go 测试内，无 node 时自动跳过）：每个模块的语法校验、以 DOM 桩真实 import 入口模块的顶层求值冒烟（抓 TDZ / 引用错误这类语法检查看不见的问题）、静态资源的 MIME 与目录穿越防护。

**配置热生效**：保存配置后，`api_key`、`cooldown.soft_rate`、`features.sanitize_blacklist_fingerprints`、
`pool.*`（熔断/在途/权重）、`schedule.*`（时点/开关/余额刷新间隔）**立即生效，无需重启**；
涉及进程装配期依赖的字段（`listen`、`auth_dir`、`state_file`、`upstream.*`、`upstash.*`、`session_sticky.ttl`）
保存后会提示"需重启进程生效"。配置写入采用「深合并且原子替换」：只更新面板表单覆盖的键，
用户手写的未知键与其余字段原样保留。

顶部「刷新」按钮 = 向上游全量查询真实余额并回写（5 秒自动轮询只读内存，不打上游）。

面板后端接口挂在 `/panel/api/*`（同一 Bearer 鉴权），可脚本化调用；账号运维操作均落到池既有入口（`Revive`/`Disable`/`Remove` 等），与 `/status` 观测口径一致。

**安全响应头**：面板页面与全部 `/panel/api/*` 响应统一带 `Content-Security-Policy`（`default-src 'none'`，脚本仅同源、样式仅同源，`frame-ancestors 'none'` 禁嵌套）、`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer` 等；前端脚本是同源 ES 模块，页面**不含内联脚本与内联事件处理器**。静态资源按扩展名白名单直出（`.html/.css/.js/.svg/.json`），路径归一后拒绝目录穿越。

**鉴权实现**：`internal/httpauth` 统一 server 与 panel 的 Bearer 校验，使用 SHA-256 摘要 + `subtle.ConstantTimeCompare` 常量时间比较（避免逐字节比较泄露密钥信息）；上游返回的 `uid` 经白名单校验（`[A-Za-z0-9_-]`，长度 ≤64）后才用于拼凭证文件名，防止路径穿越。

> ⚠️ 公网部署提示：服务自身只提供明文 HTTP，**请务必置于 HTTPS 反向代理之后**（Nginx/Caddy 等）并配置访问限流；仅本机或私有网络使用时可直接运行。

## 🔌 API 端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer（`api_key` 非空时） | OpenAI 兼容补全；流式/非流式；请求体上限 8 MiB |
| `GET /v1/models` | Bearer（`api_key` 非空时） | 模型列表（纯动态拉取，缓存 1h；失败返回空列表 + 5min 负缓存）；每模型带 `context_length`/`max_output_tokens`（四级查找链：上游目录 → 内置知识表 → model.json 缓存 → models.dev）、`reasoning_supported_efforts`/`reasoning_default_effort` 思考档位及描述/标签/倍率等全字段（上游有返回时） |
| `GET /status` | Bearer（`api_key` 非空时） | 账号状态汇总 + 每账号详情（积分/冷却/熔断/在途/粘性） |
| `GET /healthz` | 无 | 健康检查：有 healthy 且未占满账号返回 200，否则 503；响应带身份标识（见下） |

> 鉴权规则：仅当 `api_key` 非空才校验 `Authorization: Bearer <api_key>`；**`api_key` 为空时上述端点直接放行**；`/healthz` 恒无鉴权。

`/healthz` 响应示例（200 / 503 同结构，仅状态码与计数变化）：

```json
{"healthy": 2, "total": 3, "service": "workbuddy2api"}
```

响应同时带 `X-Service: workbuddy2api` 头。这两个身份标识用于区分**本网关**与同端口上可能残留的其他服务——对方即使返回 2xx 也不会带该字段 / 头，宿主探测据此避免"假成功"。

**宿主健康探测指引**：强校验（推荐）用 `/status` + `api_key`——只有持有正确 `api_key` 的本网关返回 200，其他服务返回 401 / 404；弱校验（不适合持 key 的负载均衡器）用 `/healthz` + `service` 字段判据（`/healthz` 恒无鉴权，`service == "workbuddy2api"` 才算命中本网关）。容器自带 `HEALTHCHECK` 用的就是弱校验（仅进程内自检，够用）。

### 模型路由（前缀即协议）

一个端点背后挂多条上游通道，**模型名的前缀决定走哪条**。前缀是网关侧的路由协议，出站前会被剥掉（上游只认裸模型名）：

| 模型名前缀 | 通道 | 账号来源 | 文档 |
|---|---|---|---|
| *(无前缀)* | 腾讯 WorkBuddy 账号池 | `auths/workbuddy-*.json` | [快速开始](#快速开始) |
| `cn:` / `global:` | 腾讯池的域选择（国内 / 国际） | 同上 | — |
| `loomy:` | 讯飞 Loomy 模型网关 | 客户端 session / `loomy-cli` 账号 | — |
| `zai:` | Z.AI / 智谱 GLM（Plan JWT + API Key 双通道） | `data/zai-accounts.json` | [Z.AI 账号池](#-zai--zcode-账号池) |
| `copilot:` | GitHub Copilot | `data/ext-accounts.json`（provider `copilot`） | [GitHub Copilot 通道](#-github-copilot-通道) |
| `cline:` | Cline（**带免费池**，池前缀保留在模型名里） | `data/ext-accounts.json`（provider `cline`） | [Cline 通道](#-cline-通道带免费池) |
| `autoclaw:` | AutoClaw（智谱，国内 / 国际两地区） | `data/ext-accounts.json`（provider `autoclaw`） | [AutoClaw 通道](#-autoclaw-通道智谱-autoglm) |
| `qclaw:` | QClaw（腾讯） | `data/ext-accounts.json`（provider `qclaw`） | [QClaw 通道](#-qclaw-通道腾讯) |
| `qoder:` | Qoder（阿里） | `data/ext-accounts.json`（provider `qoder`） | — |
| `codex:` | Codex 订阅池 | 本机 `~/.codex*` 凭据 | — |
| `free:` | 免费 key 池（`free:<provider>/<model>`） | 配置的免费 Key | — |

找不到可用账号时**直接回 503 并说明原因**（含最近一次失败原因），不静默回落腾讯池——前缀就是路由协议，回落会把语义搞乱。

### 流式行为细节

- 出站请求强制 `stream:true`；SSE 帧按 OpenAI 规范**白名单重建**（`reasoning_content` 保留、工具调用按 index 合并、未知字段剥离）
- 保证恰好一个 `data: [DONE]`（上游漏发时兜底补写）；空流先写一帧 `error` 再补 `[DONE]`
- 非流式请求由本地聚合完整 SSE 流为单 `chat.completion` 响应（含 `reasoning_content` / `tool_calls`）

### 上游端点

上游接口均为 CodeBuddy 官方 CLI / 插件使用的**非公开 / 逆向接口**，未见公开 API 文档；路径及 Host 以代码内常量为准（见文末出处表）。两类 base：

- **`copilot.tencent.com`**：聊天补全（SSE）、token 刷新、OAuth、模型列表、growth 域（旅行 / streak）
- **`www.codebuddy.cn`**：每日签到、余额查询、活跃上报

| 相对路径（绝对路径见出处表） | 方法 | 用途 |
|---|---|---|
| `chat/completions` | POST | 聊天补全（SSE） |
| `console/enterprises/personal/models` | GET | 动态模型列表 |
| `plugin/auth/token/refresh` | POST | token 刷新 |
| `billing/meter/daily-checkin` | POST | 每日签到 |
| `billing/meter/get-user-resource` | POST | 余额查询 |
| `report` | POST | 对话活跃上报（`chat_request_send` 事件数组，必须含 `userId`；点亮连登 / 解锁领养） |
| `plugin/auth/state?platform=CLI` | POST | OAuth 取授权 URL |
| `plugin/auth/token?state=` | GET | OAuth 轮询取 token |
| `plugin/login/account?state=` | GET | OAuth 取账号信息 |
| `activity/growth/buddy/agreement` | POST | 猫猫旅行：同意协议（幂等） |
| `activity/growth/buddy/first` | POST | 猫猫旅行：首次领养 |
| `activity/growth/buddy/info` | GET | 猫猫旅行：查询猫档案 |
| `activity/growth/buddy/travel/status` | GET | 猫猫旅行：旅行状态 |
| `activity/growth/buddy/travel/depart` | POST | 猫猫旅行：派出 |
| `activity/growth/buddy/travel/claim` | POST | 猫猫旅行：领奖 |
| `activity/growth/streak` | GET | 连登天数 + 兑换档位状态（活跃自检 / 连登管家） |
| `activity/growth/redeem` | POST | 连登档位兑换（`{tier, client_token}`；未解锁 403） |
| `activity/growth/lottery/summary` | GET | 抽奖次数查询 |
| `activity/growth/lottery/draw` | POST | 抽奖一次（`{client_token}`，消耗 1 次） |
| `activity/growth/tasks` | GET | 任务列表（含 reward_credit/reward_energy/progress） |
| `activity/growth/tasks/accept` | POST | 接受任务（`{"task_codes":[...]}`） |
| `activity/growth/tasks/<task_code>/claim` | POST | **领取任务奖励**（任务码在路径、无 body；**Web 域 `www.workbuddy.cn`**，非 CLI 域——这是领奖能成功的关键） |

出站请求统一携带 `CLI/2.63.2 CodeBuddy/2.63.2` UA（可被 `upstream.user_agent` 覆盖）；聊天请求带账号头（`X-User-Id` 等），**永不携带 `X-Refresh-Token`**（该头只出现在 token 刷新请求）。领奖请求额外带 `x-client-platform: web` 与 workbuddy.cn 的 Origin/Referer。

## 请求级日志

每个 `/v1/chat/completions` 请求结束时输出一行表格日志（stdout）：

```text
| #001 | 18:31:31 | deepseek-v4 | stream | 200 | uid=0851ce35 | TTFB=801ms | tok=60 | 23.5tok/s | total=2.6s |
```

| 字段 | 说明 |
|---|---|
| `#001` | 进程级请求序号 |
| `18:31:31` | 结束时刻 |
| `deepseek-v4` | 模型名（超 11 字符截断） |
| `stream` / `sync` | 请求模式 |
| `200` | 状态码 |
| `uid=0851ce35` | 账号 UID 前 8 位 |
| `TTFB` | 流式首帧耗时（非流式为 `-`） |
| `tok` / `tok/s` / `total` | 输出 token 数 / 速率 / 总时长 |

**敏感度**：日志不含任何 token 明文（详见[安全与合规](#安全与合规)），无落盘日志文件。

## 部署运维

### Docker 镜像

多阶段镜像（`golang:1.23-alpine` 构建 → `alpine:3.20` 运行）一次编译全部四个二进制并随镜像分发：

- **wb2api**（主服务）、**signin_bin**、**login**、**credit** + 脚本（`login.sh` / `signin.sh` / `credit.sh` / `scripts/probe_active.py`）
- 以 `app` 用户（uid 10001）运行，`app/auths` 与 `app/data` 预建
- 镜像内默认落 `config.example.json` 作为空配置（不含密钥），生产用挂载卷覆盖 `/app/config.json`
- 内置 `HEALTHCHECK`（`wget /healthz`，30s 间隔）

账号 / 数据通过 `docker-compose.yml` 卷挂载持久化：`./auths`、`./data`、`./config.json`。

### 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | OAuth 登录 → 落盘 auth → 重启容器 |
| `./signin.sh [auths_dir]` | 批量签到（过期先刷新） |
| `./credit.sh` / `./credit.sh -json` | 积分日报（美化 / 原始 JSON） |
| `python3 scripts/probe_active.py` | 活跃上报手动诊断 / 补跑（probe=只读 / report=单号上报 / unlock=单号领猫 / ALL=全池；写操作默认 dry-run，需 `--yes`） |
| `python3 scripts/probe_max_tokens.py` | 探测各模型**真实输出上限**（区分静默钳制与模型主动收尾），`--panel-out` 结果可直接进面板展示（见下节） |

二进制不在 git 中：脚本首次使用自动 `go build` 对应 `cmd/*`（Docker 镜像内已预编译）。

### 探测模型真实输出上限

上游 `/v3/config` 里的 `max_output_tokens` 是**声称值**，普遍虚高：实测 16 个 CN 模型中 8 个被
**静默钳制**（请求 `max_tokens` 更大也不报错，输出到真实上限即截断），最狠的声称 1M 实际 32K。
「模型与档位」视图因此支持在最大输出列叠加**实测标注**：

- 🔴 `32K ⚠ 钳制 12×` —— 实测被截断于 32K，声称值的 1/12（`finish=length` 判据，可信）
- 🟢 `48K ✓ / 64K ↑` —— 实测与声称一致 / 实际比声称更大
- ⚪ `≥40K` / `?` —— 满额未触顶（下界）/ 模型主动收尾未测出

实测值**不写死在代码里**——它来自探测工具写入的数据文件，上游调整后重跑一次即自动刷新：

```bash
# 在网关所在机器上（探测会真实消耗积分；单模型预算默认 600s，并行 4）
python3 scripts/probe_max_tokens.py   --base http://127.0.0.1:7863/v1 --key sk-xxx   --panel-out data/output_probes.json

# 断点续测 / 只测指定模型 / 预览计划
... --resume
... --models cn:glm-5.2 --panel-out data/output_probes.json
... --dry-run
```

文件落在 state 文件同目录（默认 `data/output_probes.json`，`data/` 已被 gitignore），面板
`GET /panel/api/model_probes` 只读透传，写入后**下次查询即生效，无需重启网关**；未探测的
模型不受影响。探测判据（两种停止的区分 / 提示词量级匹配 / 并行与时间预算）的设计细节
见脚本头部注释。

### 账号管理

- 多账号复制 `auths/workbuddy-<uid>.json` 即可，池启动时自动对齐目录
- Session 失效账号被禁用（`disabled_reason` 透出在 `/status`）后，可用 `./login.sh` 重新登录覆盖凭证；已持久化 `disabled=true` 的账号可在源码侧调用 `Pool.ReviveDisabled(uid)` 复活（`state.json` 中清除 `disabled` 标志）
- 备份 = `auths/`（凭证）+ `data/state.json`（池状态：积分 / 冷却 / 计数）；配置 Upstash 后状态另镜像至 Redis（7 天 TTL）

## 安全与合规

### 1. 凭据管理（auths）

- **位置**：`./auths`（`auth_dir` 可配），文件名 `workbuddy-<uid>.json`
- **内容**：明文 `accessToken` / `refreshToken` + 账号元信息（`account.uid` / `enterpriseId` / `nickname`）
- **权限**：容器内以 `app` 用户（uid 10001）运行；token 刷新由 `SaveAtomic` 以 `0600` 原子写回（tmp + rename）；`login.sh` 首次落盘遵循登录 umask，建议手动 `chmod 600 auths/*.json`
- **切勿提交 git**：`.gitignore` 已排除 `auths/`、`data/`、`backups/`、`config.json`、`*.key`、`*.pem`、`*.env`、`docs/` 及除 README 外的全部 `*.md` 工作文档

### 2. 网络暴露与日志敏感度

- 默认监听 `:7863`，compose 暴露 `0.0.0.0:7863`，**无内置 TLS**；公网部署必须设置 `api_key`，建议前置反代 / 内网
- 请求日志字段：序号 / 模型 / 模式 / 状态码 / **uid 前 8 位** / TTFB / token 数——**不含** `accessToken` / `refreshToken` / `api_key` 明文（不读取 `Authorization` 头）
- 日志写 **stdout / stderr**（容器内进入 `docker logs`），代码无任何落盘日志文件

### 3. 发布来源与合规边界

- **无预编译 release**：仓库无 Release / tag，产物 = 源码自构建（Dockerfile 多阶段在本地构建时完成）
- 登录 / 签到 / 积分工具：`./login.sh` / `./signin.sh` / `./credit.sh`
- **无产物校验和**：`go.sum` 仅约束 Go 模块依赖；Docker 镜像由本地 `docker compose build` 生成，未引用第三方镜像
- 上游 CodeBuddy 属腾讯系商业产品，本项目是其**非官方 OpenAI 兼容网关**；使用其账号做 API 网关涉及目标平台服务条款与账号风险，作者不对账号封禁、条款违约或使用结果负责

### 4. 授权使用边界

- 仅限**本人授权账号**、本机 / 私有环境测试
- 不得共享、转售、违规分发，或用于违反目标平台条款的用途
- 遵守 CodeBuddy 平台服务条款与所在地法律
- 妥善保管 `auths/`（明文凭证）与网关端口

## 常见问题

### 429 code=6004（模型级限流）的冷却语义？

上游 `429` + `code 6004` 是**该模型的使用量超限**（msg 通常带「将在 YYYY-MM-DD HH:MM:SS UTC+8 重置」），**不是账号整体被限流**。网关的处理：

- **冷却到上游重置时间**：msg 带「将在 … 重置」时，账号冷却 `until` 精确等于该墙钟（按 UTC+8 解释），并封顶 `soft_rate_max`（默认 2h）
- **切模型立即可用**：冷却由 6004 触发时会记录触发模型；同一账号改用**其他模型**请求时视为可用。同模型或未记录模型的冷却回到现状
- **退回指数退避**：6004 无「将在 … 重置」文案，或非 6004 的普通软限流 → 仍是 `soft_rate`（600s 起，连续触发指数退避，封顶 `soft_rate_max`）

### 多图会话请求体超限怎么办？

网关**不再设请求体上限**（`server.max_body_mb` 已移除，对齐上游）：任意大小的请求体都会完整读入并转发上游，超限类问题由上游自然返回错误——其响应信息量更大（能看到上游的真实策略），网关不再以 413 提前拦截。

- 多图/长上下文会话（历史图片每轮以 base64 重发，编码再膨胀约 37%）不会再撞网关侧 413
- 若上游真的返回 413/超限错误，网关按既有错误分类链路如实透传（不打码、不罚号——超限是请求侧问题）
- 客户端中途断流导致的半截 body 在读入阶段即报 `400 invalid_request`，不会把截断 JSON 喂给上游（issue #41 语义保留在读错误路径）

### Docker 部署登录后报「写入 auths/…json.tmp 失败： permission denied」？

容器以 `app` 用户（uid 10001）运行，而宿主机挂载的 `./auths`、`./data` 目录属主不是它——写凭证 tmp 文件被拒。三种解法任选（前两种均**无需 root 容器**）：

```bash
# 方案 1（推荐，非 root）：让容器以你自己的 uid 运行——挂载目录本来就是你建的
PUID=$(id -u) PGID=$(id -g) docker compose up -d --force-recreate
# 或写进 .env 文件长期生效（.env 已被 .gitignore 忽略）：
#   echo "PUID=1000" > .env && echo "PGID=1000" >> .env

# 方案 2：把挂载目录属主交给容器默认用户（需要 sudo）
sudo chown -R 10001:10001 ./auths ./data ./config.json

# 方案 3：compose 设 user: "0:0" 以 root 运行（NAS/群晖不便 chown 时用）
```

报错信息里自带这条指引；compose 的 `user` 已参数化为 `${PUID:-10001}:${PGID:-10001}`。

### 账号被 Disable 后如何恢复？

- **用 `./login.sh` 重新登录**覆盖凭证，重启后自动回池；
- 或源码侧调用 `Pool.ReviveDisabled(uid)` 清除 `disabled` 状态（`state.json` 同步刷新）。

### 系统提示词被内容策略误杀怎么办？

默认 `prompt.mode=custom` 已用网关自有提示词替换客户端 system，从源头消除大部分误报；用户 / assistant 消息中的指纹串由 `features.sanitize_blacklist_fingerprints` 清洗，两层叠加。`passthrough` 模式下首遇拦截会自动换 Degraded 中性提示词同请求重试一次。

### 如何让官网「使用端」列显示为 WorkBuddy？

官网「使用端」列按出站请求 UA 服务端归因。配置 `upstream.user_agent: "WorkBuddy/2.x.x"`（或环境变量 `WB2A_USER_AGENT`）即可改写全部出站请求的 UA；默认保持 `CLI/2.63.2 CodeBuddy/2.63.2` 现状（指纹净化考虑，可配而非改死）。

## 关键断言 ↔ 代码出处

| 断言 | 出处 |
|---|---|
| `prompt.mode` 默认 `custom` | `cmd/server/config.go:148` |
| 请求体无网关侧上限（max_body_mb 已移除） | `internal/server/handler.go` chatCompletions 读 body 段 |
| 出站强制 `stream:true` | `internal/upstream/payload.go:28` |
| DeepSeek 思维链注入（`thinking.type=enabled`） | `internal/upstream/thinking.go:110` |
| 默认 `reasoning_effort` 档位 = `high` | `internal/upstream/thinking.go:32` |
| `reasoning_content` 多轮回填（assistant 消息） | `internal/upstream/thinking.go:54` |
| Degraded 中性提示词常量 | `internal/prompt/prompt.go:25` |
| 降级触发与次日 00:00 CST 重置 | `internal/server/degrade.go:30`（Trigger）、`:46`（nextMidnightCST） |
| 6004 模型级限流 code 与重置时间解析 | `internal/upstream/client.go:127`、`internal/upstream/client.go:147` |
| `11101` / Unmarshal 失败不罚号 | `internal/upstream/client.go:114-115`；处理分支 `internal/server/handler.go:489` |
| 出站 UA 覆盖（空 = 现状 `CLI/2.63.2 CodeBuddy/2.63.2`） | `cmd/server/config.go:73`；接线 `cmd/server/main.go:96` |
| session-dead 连续阈值 3 才禁用 | `internal/pool/pool.go:249-253`（`sessionDeadThreshold`） |
| `ReviveDisabled` 人工复活 | `internal/pool/pool.go:951` |
| disabled 账号透出 `disabled_reason` | `internal/pool/pool.go:1162-1165` |
| 硬冷却至次日 04:00 | `internal/pool/pool.go:882`（`CooldownUntilTomorrow4AM`） |
| 软冷却退避封顶 2h | `internal/pool/pool.go:247`（`defaultSoftRateMax`） |
| Top-5 候选短名单 | `internal/pool/pool.go:584` |
| `activity_hours` 默认 `[10]` | `cmd/server/config.go:135` |
| 活跃自检回读 streak | `internal/scheduler/scheduler.go:227`（`checkActivityStreak`） |
| streak 端点 `activity/growth/streak` | `internal/upstream/travel.go:24`（常量）、`:139`（`GrowthStreak`） |
| Redis 粘性镜像 7 天 TTL | `internal/redisstore/redisstore.go:21` |
| 静态模型表含 `deepseek-v4-flash` 等 | `internal/server/handler.go:146` |

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 CodeBuddy 服务条款，自行承担使用风险（包括账号封禁、条款违约等）。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

本项目采用 [MIT License](LICENSE) 开源协议。

- 允许任意使用、复制、修改、合并、发布、分发、再授权及销售
- 再分发（源码或二进制形式）时，请保留原仓库的 MIT 版权声明与许可声明（如在 NOTICE 或 README 中注明原始出处 `https://github.com/Sliverkiss/workbuddy2api`）
- 本项目不授予任何上游（CodeBuddy / 腾讯）接口或服务的权利；使用者仍需自行遵守上游服务条款
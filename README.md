# go-prometheus-aiops-poc

最小可跑的 **AIOps 监控闭环 POC**：一个零依赖 Go HTTP 服务暴露 Prometheus 指标 →
Prometheus 按 **P99 延迟 / 5xx 错误率**触发告警 → Alertmanager 通过 **Webhook**
把告警推给接收器 → 接收器自动创建 **Devin Session** 定位并修复问题 → 修复合并、
指标恢复、告警 Resolved —— 形成完整的 **"监控 → 告警 → 自动修复"** 闭环。

> 这是 POC/教学项目，不是生产方案。每个组件都刻意保持最小，方便逐层理解。
> 生产化改造清单见文末 Roadmap。

## 架构总览

```mermaid
flowchart LR
    subgraph svc["demo-service :8080"]
        W["/work · /healthz"] --> M["/metrics (Prometheus text format)"]
    end
    M -- "scrape every 15s" --> P["Prometheus :9090"]
    P -- "alert rules: P99 > 400ms / 5xx > 5%" --> A["Alertmanager :9093"]
    A -- "HTTP webhook (grouped)" --> R["webhook-receiver :9000"]
    R -- "POST api.devin.ai/v1/sessions" --> D["Devin Session"]
    D -- "root-cause fix + PR" --> GH["GitHub"]
    GH -- "merge & redeploy" --> svc
    P -- "datasource (provisioned)" --> G["Grafana :3000"]
```

数据流两个方向：**下行**是指标采集与告警（拉模型 + 推模型），**上行**是修复闭环
（事件驱动）。Devin Session 是唯一具备写权限的节点，也是最小权限原则的重点约束对象。

## 组件清单

| 组件 | 镜像/构建 | 端口 | 职责 |
|---|---|---|---|
| demo-service | `./Dockerfile`（Go 1.23, 零依赖） | 8080 | 被监控对象：`/work` 模拟业务请求，`/metrics` 暴露指标 |
| Prometheus | `prom/prometheus:v2.55.1` | 9090 | 抓取指标、评估告警规则、推送告警到 Alertmanager |
| Alertmanager | `prom/alertmanager:v0.27.0` | 9093 | 告警分组/去重/路由，Webhook 投递 |
| webhook-receiver | `./Dockerfile`（同一镜像） | 9000 | 接收告警、记录日志、按需创建 Devin Session |
| Grafana | `grafana/grafana:11.3.0` | 3000 | 可视化，datasource + dashboard 全部 provisioning |

## Go 监控指标

demo-service 用 **RED 方法**（Rate / Errors / Duration）暴露三类指标，全部手写
Prometheus text exposition format（v0.0.4），`go.mod` 无任何外部依赖：

| 指标 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `demo_http_requests_total` | counter | `handler`, `code` | 请求总量，按 handler × 状态码细分 |
| `demo_http_request_duration_seconds` | histogram | `handler`, `le` | 延迟分布，bucket 对齐 `prometheus.DefBuckets` |
| `demo_http_requests_in_flight` | gauge | — | 当前并发处理中的请求数 |

设计要点：

- **Histogram 而非 summary**：分位数由 Prometheus 服务端用 `histogram_quantile`
  计算，可跨实例聚合（客户端 summary 的 quantile 不可聚合——这是最常见的坑）。
- **Bucket 对齐 `DefBuckets`**（0.005s ~ 10s）：覆盖从毫秒级健康检查到秒级慢请求。
- **低基数标签**：只用 `handler`/`code` 两个 bounded label；绝不把 path、user_id、
  trace_id 放进标签（unbounded cardinality 会撑爆 TSDB）。
- **中间件埋点**：`instrument()` 包装 handler，延迟统计和业务代码解耦。
- 生产环境不要手搓 exposition——换 `prometheus/client_golang` 的
  `promhttp` + `promauto`，还能免费拿到 exemplars 和 Go runtime metrics。

## P99 延迟告警

核心规则（`deploy/prometheus/alerts.yml`）：

```promql
histogram_quantile(0.99, sum(rate(demo_http_request_duration_seconds_bucket{handler="/work"}[5m])) by (le)) > 0.4
```

逐层拆解：

1. `rate(..._bucket[5m])` — 对每个 `le` bucket 求每秒增长率。5 分钟窗口平衡了
   灵敏度和平滑度。
2. `sum(...) by (le)` — 把所有非 `le` 标签聚合掉。`histogram_quantile` 要求
   输入只剩 `le` 这一个维度，否则结果错乱。
3. `histogram_quantile(0.99, ...)` — 在 bucket 间线性插值，估出 P99。
4. `> 0.4` + `for: 1m` — P99 超过 400ms 且持续 1 分钟才告警，避免毛刺误报；
   `keep_firing_for: 1m` 防止告警在阈值附近抖动 flap。

为什么盯 **P99 而不是平均值**：平均值会被大量快请求稀释——1% 的用户等了 10 秒，
avg 可能毫无变化，但那一小撮人已经在骂了。尾部延迟才是真实体验。

配套还有一条 `DemoServiceHighErrorRate`（5xx 占比 > 5%），severity=warning，
展示同一条管道同时承载不同严重度的告警。

## Webhook 报警管道

```
Prometheus ──(pending→firing)──▶ Alertmanager ──(group_wait 10s→grouped POST)──▶ webhook-receiver
```

- **group_by: `alertname` + `service`**：同一条规则的多个实例告警合并成一条通知，
  Devin 收到的是"这个服务 P99 高了"，而不是 N 条重复事件。
- **group_wait 10s / group_interval 1m / repeat_interval 1h**：首个告警等 10s
  看有没有同伴；组内更新间隔 1m；未恢复的告警每小时重复提醒一次。
- **send_resolved: true**：恢复通知也会投递——闭环的"合上"就靠它确认。
- Payload 是 Alertmanager v4 格式（`alerts[]` 带 `status/labels/annotations/
  fingerprint`），receiver 原样落日志并保留最近 50 条供 `GET /alerts` 查询。

## Devin 自动化闭环

当告警 `status=firing` 且 `DEVIN_API_KEY` 已配置，receiver 调用
`POST https://api.devin.ai/v1/sessions` 创建一个 Devin Session，prompt 携带
告警名、severity、summary/description、起始时间和 generator URL：

```text
A Prometheus alert is firing: DemoServiceHighP99Latency (severity: critical).
Summary: demo-service /work P99 latency > 400ms for 1m
...
Investigate the latency/error regression behind this alert, find the root cause
in the repository, fix it with a minimal change, and open a PR.
```

完整闭环时序：

```mermaid
sequenceDiagram
    participant U as User/Traffic
    participant S as demo-service
    participant P as Prometheus
    participant A as Alertmanager
    participant R as webhook-receiver
    participant D as Devin
    participant G as GitHub

    U->>S: /work (normal traffic)
    Note over S: 慢查询/bug 上线, tail latency ↑
    S-->>P: scrape /metrics
    P->>P: rule eval: P99 > 400ms for 1m
    P->>A: alert firing
    A->>R: webhook POST (grouped)
    R->>D: POST /v1/sessions (prompt=alert context)
    D->>D: 定位根因, 写最小修复
    D->>G: open PR → CI → review → merge
    G-->>S: redeploy fixed build
    S-->>P: P99 回落 < 400ms
    P->>A: alert resolved
    A->>R: resolved notification (send_resolved)
    Note over D: 闭环完成: 检测→修复→验证全自动
```

关键设计：告警 **fingerprint** 天然可做幂等——同一告警重复投递时应按
fingerprint 去重，避免一个告警开出 N 个 Devin session（当前 POC 未实现，
见 Roadmap）。

## 防误判与防死循环

自动触发一个有代码写权限的 Agent，最大的两个风险是**误报**（告警不值得修）
和**死循环**（修了没好 → 再触发 → 再修）。防护分两段：

### 防误判（dispatch 之前）

- **持续期门控**：`for` + `keep_firing_for` 保证只有持续越线的告警才到达
  receiver，瞬时毛刺在 Prometheus 层就被滤掉。
- **severity 分级路由**：只有 `severity=critical` 自动派 Devin；`warning`
  只落日志/进工单队列，由人工决定是否立案（对应 Roadmap 的多 receiver 路由）。
- **prompt 自带验证指令**：发给 Devin 的 prompt 明确要求先复现指标回归再动
  代码——指标如果已经健康，session 输出 false-positive 报告而不是硬开一个 PR。
- **fingerprint 幂等去重**：同一 fingerprint 在冷却窗口内只开一个 session，
  Alertmanager 的 `group_interval`/`repeat_interval` 重投不产生重复工单。

### 防死循环（dispatch 之后）

- **熔断器**：同一 fingerprint 每小时限 N 个 session；修复合并后告警仍
  firing → 不再自动派单，升级人工处理。
- **PR 不自动合并**：Devin 的产出是 PR 而不是 hotfix——CI + human review 是
  硬门，烂修复根本进不了 prod，循环链路被物理剪断。
- **明确终态**：`send_resolved` 通知回到 receiver 确认闭环收口；未恢复的
  告警靠 `repeat_interval` 提醒人工，而不是无限重复创建 session。

### 一个真实故障场景（这套东西修什么）

某次上线把列表查询改成 N+1（ORM eager load 丢了）：p50 纹丝不动，P99 从
80ms 涨到 600ms，1% 的请求卡到秒级——avg 告警完全瞎掉。本栈的链路是：
`histogram_quantile` 告警 firing → webhook 把 alertname + fingerprint +
summary 交给 Devin → session 对照最近 diff 定位到查询改动 → 恢复 eager
load 开 PR → 人工合并 → P99 回落 → resolved 通知收口。
**这套系统的价值不在"自动修"，在于把 P99 回归从"用户投诉驱动"变成
"指标驱动 + 自动立案"。**

## 常驻 Devin Bot 协同模式

上面的闭环用的是一次性 session（每告警一个）。更进一步是把 receiver 对接
一个**持久在线的 Devin Bot**——它有自己的记忆，常驻 Slack 频道，在闭环里
扮演"值班 SRE"而不是一次性 worker：

```mermaid
flowchart LR
    A["Alertmanager"] --> R["webhook-receiver"]
    R --> B["Devin Bot（常驻 Slack #aiops-alerts）"]
    B <-->|"在线程里追加线索、拍板"| H["人类工程师"]
    B --> M[("长期记忆：故障指纹→根因→修复手法")]
    B --> PR["GitHub PR"]
    PR --> H
    B --> RB["runbook / 告警阈值调整 PR"]
```

**Bot 在闭环里的分工：**

- **接警与分诊**：receiver 不再直接 `POST /v1/sessions`，而是把告警事件
  发给常驻 Bot；Bot 先查自己的记忆——相同 fingerprint / 相似签名（同
  handler、同分位数）的故障处理过没有，直接把历史根因写进调查 prompt，
  跳过重复排查。
- **与人沟通**：告警 firing 时 Bot 在频道发卡片（alert 摘要 + Grafana
  面板链接 + 初步假设），调查过程在同一个 Slack 线程更新；工程师可以
  @Bot 补线索（"昨晚刚上了 XX 变更"），线索直接进入会话上下文。
- **审批接口**：PR 出来后 Bot @相关 CODEOWNERS 请求 review；遇到阻塞
  （缺权限、缺上下文、拿不准的设计取舍）直接在频道里问，而不是静默卡住。
- **经验沉淀**：每次闭环 resolved 后，Bot 把"故障指纹 → 根因模式 → 修复
  手法 → MTTR"写成记忆条目；新故障类型则把 triage 步骤追加到仓库
  runbook，以 PR 形式提交（人审）。告警反复误报时，Bot 还会提告警阈值
  调整 PR——闭环从"修代码"扩展到"修监控本身"。
- **复盘输出**：周期性汇总 firing 次数、MTTR、误报率，发周报到频道。

分工一句话：**Prometheus/Alertmanager 是眼睛，webhook-receiver 是末梢
神经，常驻 Bot 是有记忆的大脑，人类保留审批和最终裁决权。**

## 极端边界与容灾

最棘手的情况是**级联故障**：上游依赖挂了 → demo-service 的 P99、5xx、
in-flight 同时 firing，告警风暴打到 receiver。分三层应对：

### 入口：风暴聚合（receiver）

- **incident 归并**：不只按 fingerprint 去重——同一 `groupKey`/时间窗内
  到达的多条告警归并为**一个 incident**，一次只派一个 Bot 任务并附上全部
  告警清单，而不是每条告警开一个 session。
- **全局风暴限流**：告警速率 > N 条/分钟时进入风暴模式，停止逐条派单，
  只发聚合摘要 + "系统级故障，等待人工裁决"。Bot 的排障上下文永远不会
  被告警数量线性冲垮——上下文上限是 incident 数，不是告警条数。

### 定位：识别"根因在外部依赖"（Bot）

外部依赖超时引发的告警有明确的指标签名，Bot 按这组信号做归因：

| 信号 | 说明 |
|---|---|
| 错误集中在同一外部 host | 日志/追踪里全是指向某下游的 `dial timeout`、`i/o timeout` |
| 延迟↑但资源指标正常 | CPU/mem/goroutine 平稳 + 尾延迟飙升 = 典型下游堆积（连接池打满、retry 放大） |
| 依赖探活同步 firing | 外部依赖自身的 health 告警也在 firing |

识别出外部根因后，Bot **不改业务代码**，而是转成韧性修复或纯报告：

- **韧性修复 PR**：收紧下游 timeout、加 circuit breaker（fail-fast 而非
  无限 retry）、bulkhead 隔离——把"外部挂了"的爆炸半径限住，这就是
  优雅降级。
- **降级开关建议**：依赖持续不可用时给出临时 feature-flag/config 降级
  方案，人类在频道里一键裁决。
- **incident 线程**：风暴期所有告警合并成一条 Slack 线程——人类看到的
  是"1 个 incident"，而不是"20 个独立故障"。

### 兜底：管道自身挂了

- **receiver 挂**：Alertmanager 配 `Watchdog` 死信告警（dead man's
  switch），投递失败有重试与告警。
- **Devin API 挂**：receiver 本地队列重试 + 超时落盘，告警不丢；恢复后
  补派并标注 `backfilled`。
- **Slack 挂**：Bot 降级为仅落 incident 记录，恢复后补发摘要。

## 安全与合规红线

常驻 Bot 在生产排障的最大风险是**凭证泄露**和**数据出境**——DB schema
敏感字段、业务日志里的 PII、生产环境变量，都不能因为"开 PR / 复现 /
Slack 汇报"流出边界。四层控制：

### 数据访问边界

- **网络隔离**：Bot session 的 net policy 只放行 GitHub、Slack、内部监控
  域名——生产 DB 直连不可达，Bot 只能从代码侧产物（migration、ORM model、
  schema 定义）推断结构，永远摸不到真实数据行。
- **Repo 路径禁入**：fine-grained PAT + playbook 双重约束，`.env*`、
  `secrets/`、`credentials*` 类路径既不可读也不可写。
- **环境变量隔离**：生产环境变量绝不注入 Bot session；复现只用本地容器
  + 合成数据，session 里能看到的全是 stub。

### 复现与测试的数据卫生

- **合成数据**：排障复现一律用 faker/fixture 生成的脱敏数据；禁止把生产
  日志、DB dump 拉进 session。
- **脱敏前置**：日志进 Bot 上下文之前先过脱敏管道（正则/DLP 扫手机号、
  身份证、邮箱、token 模式），脏数据根本到不了模型眼前。

### 出站过滤（PR / Slack 汇报）

- **secret-scanner 拦截**：Bot 的 PR 描述、评论、Slack 卡片在发出前过
  gitleaks/trufflehog 风格扫描，命中凭据/连接串/schema 敏感字段值即拦截
  并告警。
- **汇报只发元数据**：Slack 卡片只含指标值、告警标签、fingerprint、
  Grafana 链接——绝不贴日志原文、环境变量值、查询结果行。

### 红线清单（明确"永不"）

| 永不 | 兜底机制 |
|---|---|
| 直连生产数据库 | 网络策略物理隔离 |
| 生产凭据进任何制品 | repo 禁入路径 + secret-scanner 出站拦截 |
| PII 出脱敏管道 | 日志脱敏前置 + 出站二次扫描 |
| Slack/PR 贴日志原文、env 值 | 汇报模板只含指标元数据 |
| 外部依赖故障时碰数据面 | 只做韧性改动（timeout/breaker），不改业务逻辑 |

## 最小权限原则

闭环里最危险的一步是"告警自动触发一个有代码写权限的 Agent"。权限收敛：

| 凭据/端点 | 授予 | 不授予 |
|---|---|---|
| Devin API key | 仅创建 session（自定义 role 只勾 `UseDevinSessions`）；存 org secret，不写进 compose/git | 组织管理、计费、secret 管理权限 |
| GitHub PAT（Devin 提 PR 用） | fine-grained PAT，仅目标仓库：`Contents: RW` + `Pull requests: RW`（Metadata 自动附带只读） | `Administration`、Actions 写、其他仓库、`repo` 全权 scope |
| webhook-receiver | 只暴露 `/alerts` `/healthz`；生产加 HMAC 签名验证 + 内网 only | 公网暴露、无鉴权接收（否则任何人可伪造告警狂刷 API 额度） |
| Grafana | anonymous = `Viewer`（只读），admin 密码不入库 | Editor/Admin 匿名角色 |
| 容器 | `USER app`（非 root），配置文件 `:ro` 只读挂载 | root 运行、可写挂载 |
| compose | 无硬编码 secret，`DEVIN_API_KEY` 仅运行时注入 | `.env`/`docker-compose.yml` 里存真 key |

一句话：**每个凭据只做它该做的一件事，泄漏后的爆炸半径最小。**

## 快速开始

```bash
# 1. 拉起整个栈
docker compose up -d --build

# 2. 打开面板
open http://localhost:3000      # Grafana（匿名 Viewer，免登录）
open http://localhost:9090      # Prometheus
open http://localhost:9093      # Alertmanager
curl http://localhost:8080/metrics   # 原始指标

# 3. （可选）接入 Devin 自动修复
DEVIN_API_KEY=<your-key> docker compose up -d webhook-receiver
```

**手动触发告警**：内置 loadgen 已经混入 ~4% 慢请求，P99 告警通常 2-3 分钟内
自己 firing。想立刻触发：

```bash
for i in $(seq 1 50); do curl -s "http://localhost:8080/work?mode=slow" & done; wait
```

观察链路：Prometheus `Alerts` 页 → `docker compose logs -f webhook-receiver` →
（如配置了 key）Devin 控制台出现新 session → 修复合并 → 告警 Resolved。

## 验证闭环 checklist

- [ ] `curl localhost:8080/metrics` 有 `demo_http_request_duration_seconds_bucket`
- [ ] Grafana dashboard 有流量曲线（loadgen 自动产生）
- [ ] `Alerts` 页 `DemoServiceHighP99Latency` 状态 firing
- [ ] `docker compose logs webhook-receiver` 出现 `alert status=firing`
- [ ] Devin session 被创建并产出 PR
- [ ] 修复部署后 P99 回落，收到 `status=resolved` 通知

## 目录结构

```
├── cmd/
│   ├── demo-service/main.go       # 被监控的 Go 服务 + /metrics exposition
│   └── webhook-receiver/main.go   # Alertmanager webhook → Devin session
├── deploy/
│   ├── prometheus/                # scrape 配置 + 告警规则
│   ├── alertmanager/              # 路由 + webhook receiver 配置
│   └── grafana/                   # provisioning: datasource + dashboard
├── docker-compose.yml
├── Dockerfile                     # 一个镜像构建两个二进制
└── go.mod                         # 零依赖
```

## 为什么是常驻 Bot，而不是一次性 LLM 脚本

审核可能会问：为什么不直接调个 LLM API 生成 diff？因为单次调用优化的是
**"产出一个 diff"**，常驻 Bot 运营的是**"让 MTTR 归零"**——前者交付产物，
后者承担职责。闭环需要的不是一个函数，是一个值班员：

| 维度 | 一次性 LLM 脚本 | 常驻 Devin Bot |
|---|---|---|
| 状态 | 无状态：发 prompt 拿 diff | 跨小时/天的有状态流程：告警→调查→PR→等 CI→等 review→resolved 收口→复盘 |
| 记忆 | 每次从零排查 | fingerprint 记忆复利：同类故障第二次 MTTR 直接砍半 |
| 协同 | 产出文本，不参与对话 | Slack 频道里的参与者：被 @ 收线索、向人提问、@CODEOWNERS 请求 review、协商取舍 |
| 职责边界 | 只修代码 | 修系统本身：误报反复时提告警阈值 PR、把 triage 步骤沉淀成 runbook PR——监控自我进化 |
| 兜底 | 没有后续概念 | resolved 不回就升级人工、风暴期聚合、管道故障降级——闭环有明确的运营终态 |

一句话：**LLM 脚本生成 diff，常驻 Bot 运营故障死亡率**。告警闭环考核的是
运营目标（MTTR、误报率、复发率），不是单次产物质量——所以必须是长期驻留、
有记忆、跟人协同的队友，而不是一次性工具调用。

## 48 小时落地计划

不谈宏大架构——第一个小切口就是**用本仓库 dogfooding**：

- **Day 1 下午**：Bot 拉进 `go-prometheus-aiops-poc` 仓库本身（它就是真实
  代码库，`cmd/demo-service` 有可改的 Go 代码），docker compose 起栈。
- **第一条测试告警**：`curl /work?mode=slow` 手动触发
  `DemoServiceHighP99Latency` → webhook → Bot 接单。告警上下文里带上
  "最近 commits + 慢路径线索"。
- **预期的第一个成功 PR**：Bot 定位到 demo-service 里注入的慢路径（写死
  的 500ms sleep），给出最小修复 diff 开 PR——一条可 review、可 merge、
  合并后 P99 回落告警 resolved 的真实修复，验证全链路不是纸上谈兵。
- **Day 2**：换一个团队真实 Go 服务仓库，接 `/metrics` + P99 告警规则，
  跑真实流量观察误报率，调阈值。

**实证——上面这条链路已经真实发生过一次**：

- 告警 `DemoServiceHighP99Latency` 真实 firing（fingerprint
  `b604179c4b51d393`），Alertmanager webhook 真实投递到 receiver。
- [Devin session](https://app.devin.ai/sessions/45e36486926d40f184643bbfb59aef59)
  按生产 prompt 格式接单，git log 定位到回归提交，开出
  [PR #1](https://github.com/liziji555/go-prometheus-aiops-poc/pull/1)：
  把 30 次串行 `fetchOrderDetail` 改为并发，p50 ≈ 44ms、p95 ≈ 66ms，
  3% 自然长尾不变。
- 合并后 P99 回落、`send_resolved` 收口——闭环全程无人工编码。

## 生产化 Roadmap

- [ ] `client_golang` 替换手搓 exposition（exemplars、runtime metrics）
- [ ] 按 alert `fingerprint` 幂等去重，防止重复创建 Devin session
- [ ] Webhook HMAC 签名验证 + Alertmanager `http_config` 带 Authorization
- [ ] Dead man's switch：Receiver 挂了也要有人知道（`Watchdog` 告警）
- [ ] Alert 分级路由：critical→Devin，warning→Slack/工单
- [ ] SLO 多窗口燃烧率告警（multi-burn-rate）替代单阈值
- [ ] e2e 测试：kind/compose CI job 验证告警→webhook 投递链路

## License

MIT — 拿去改，拿去学。

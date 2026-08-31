# 架构设计与决策记录

> 决策定稿于 2026-08-31。本文是 ADR（架构决策记录）+ 架构说明的合体。
> 规则依据见 [okx-rules.md](./okx-rules.md)。

## 1. 项目定位

用 Go 实现 OKX 的**仓位管理内核**，目标是尽可能贴近 100% 还原真实行为，
并保持良好的外部可引入性，供策略回测引擎等第三方使用。

一句话概括职责：**「成交事件 → 仓位/账户状态」的记账机。**

## 2. 已定决策（ADR）

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | 职责边界 | ~~核算内核 + 可选简易撮合器~~ → **核算内核 + 内置撮合**（见下方修订） | — |
| 2 | 数值精度 | `shopspring/decimal` | 十进制任意精度，与交易所口径天然一致；且其默认 JSON 序列化为**带引号字符串**，恰好与 OKX 的 JSON 形态吻合 |
| 3 | v1.0 覆盖范围 | 永续合约全覆盖（USDT本位 + 币本位，逐仓 + 全仓，单向 + 双向，acctLv=2 现货合约模式） | 覆盖绝大多数回测场景，排期可控。跨币种(acctLv=3)/组合保证金(acctLv=4)/期权 明确排除在 v1.0 外 |
| 4 | 参考数据来源 | Provider 接口 + `go:embed` 内置快照 + 可选运行时拉取 | 零配置可跑、离线可复现、又能取最新。三者兼得 |
| 5 | 强平引擎深度 | 阶梯部分减仓 + 穿仓 | 真实链路 `撤单 → 阶梯减仓(降档重算，仓位可能被救回) → 全平 → 穿仓`。简化成一次性全平会显著高估大仓位的损失 |
| 6 | 驱动模型 | 显式时钟推进 | `SetMark()` 只更新价格；`Advance(ts)` 推进时钟并按序执行风控。时序确定、可复现、好调试 |
| 7 | 一致性验证 | 单测 + OKX 模拟盘对拍 | 手写单测会把我对规则的误解一起固化。模拟盘逐字段 diff 是唯一能真正证伪的办法 |
| 8 | 集成形态 | 仅 Go 原生 API | 不做「假 OKX HTTP 服务端」，专注把库本身做扎实 |

### 决策 1 的修订：撮合由「可选子包」改为「内置」

初版把撮合定为可选子包，理由是回测引擎自己有撮合。使用者指出这个判断不对：
**撮合是每个引擎都要做的事**，各自重写一遍既浪费，又容易在成交角色判定、
冻结释放这类细节上出错——而这些细节直接影响手续费与可用余额。

据此改为内置，但保留一条底线：**手工灌 Fill 的路径继续完整可用**。
内置撮合必然要做假设（K 线驱动、无盘口深度、全成或不成），有自己撮合逻辑的
引擎不该被这些假设绑架。两条路并存，撮合是内置但非强制。

内置撮合最实在的价值是**成交角色由模拟器判定**：挂住后被价格触发是 maker，
下单即可成交是 taker。这一项直接决定手续费，交给调用方填写是常见的错误来源。

明确不建模的：盘口深度、部分成交、排队位置。没有深度数据就无从建模这三者，
强行假设只会给出看似精确实则杜撰的结果。

补充两条按常规判断直接定的：

- **状态即纯数据**：账户状态是可 JSON 序列化 + 可深拷贝的结构体，内部不藏 channel / goroutine / 闭包。
  回测引擎做参数扫描、walk-forward、事件重放时可直接快照回滚。**必须第一天定，事后补代价极大。**
- **默认不加锁**：回测是单线程热路径，decimal 本身已有开销。并发需求用可选的 `SyncSimulator` 包装器解决。

## 3. 包结构

模块路径 `github.com/dream-until-dawn/okx-position-simulator-go`，根包名 `okxsim`，`go 1.22`。

```
/                    package okxsim   核心：账户·仓位·保证金·强平·资金费·费率·事件
  ├─ types/          枚举与值类型 (InstType/TdMode/PosSide/Side/OrdType/MgnMode/...)
  ├─ refdata/        Provider 接口 + 数据模型 + go:embed 快照 + 查档逻辑
  │    └─ live/      ← 独立子包：从 OKX 公共 API 拉取最新参考数据
  ├─ matching/       ← 独立子包：可选简易撮合器
  ├─ internal/       内部工具
  └─ cmd/conformance/  ← 独立对拍工具（不属于库的公开 API）
```

**硬性约束：核心包的依赖树只有 `shopspring/decimal` 一个。**
`refdata/live`（引入 `net/http`）、`matching`、`cmd/conformance`（引入 OKX SDK）全部隔离在子包中。
别人把它塞进回测引擎时，不会被迫带进一堆传递依赖 —— 这是"外部可引入性"最实在的一条。

## 4. API 形态草案

```go
sim := okxsim.New(okxsim.Config{
    AcctLv:  types.SpotAndFutures,   // acctLv = 2
    PosMode: types.NetMode,          // 或 LongShortMode
    FeeTier: fee.Lv1,
    RefData: refdata.Embedded(),     // 或 live.Fetch(ctx)
})

sim.Deposit("USDT", d("10000"))

// 1) 更新行情（只更新，不触发任何风控）
sim.SetMark("BTC-USDT-SWAP", d("77657.4"))

// 2) 灌入成交（撮合由外部负责）
_, err := sim.Fill(okxsim.Fill{
    InstID: "BTC-USDT-SWAP", Side: types.Buy, TdMode: types.Isolated,
    Sz: d("10"), Px: d("77650"), Role: types.Taker, Ts: ts,
})

// 3) 推进时钟：资金费结算 → 风控检查 → 强平，返回事件
events := sim.Advance(ts)

// 4) 查询（结构体与 OKX REST 响应字段级同构）
pos := sim.Positions()   // ≅ GET /api/v5/account/positions
bal := sim.Balance()     // ≅ GET /api/v5/account/balance
```

`Advance` 内部的执行顺序是确定的，且这个顺序本身就是需要与真实行为对齐的一部分：

```
资金费结算 → 重算权益/uPL → 计算 mgnRatio → 若 ≤100%：撤单 → 阶梯减仓 → 全平 → 穿仓
```

## 5. 验收标准

**「100% 模拟」被定义为可自动化验证的东西**，否则它只是一句口号：

1. 所有输出结构体与 OKX v5 REST 响应**字段级同构**，数值字段序列化为字符串（与 OKX 一致）。
2. 错误码与 OKX 错误码对齐。
3. `cmd/conformance` 拿模拟盘 API key 真实下单，把真实返回与模拟器输出**逐字段 diff**。

### 对拍工具选型

使用 `github.com/dream-until-dawn/okx-api-v5-go`（同作者实现，v0.1.0）作为数据源与下单通道：

```go
client, err := okx.NewClient(
    okx.WithCredentials(apiKey, secret, passphrase),
    okx.WithSimulated(true),   // 模拟盘
)
```

该包的数值类型是 `Num string`，**无损保留 OKX 返回的原始字符串**，
与 `decimal.Decimal` 之间通过 `decimal.NewFromString` 无损互转，因此 **diff 可以做到字符级**。
其依赖树只有 `gorilla/websocket`。

⚠️ 模拟盘凭证存放于项目根目录 `.env`（已被 `.gitignore` 忽略），**严禁提交**。
`cmd/conformance` 从环境变量读取，不得硬编码。

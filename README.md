# okx-position-simulator-go

用 Go 实现 OKX 的仓位管理内核，力求贴近 100% 还原真实行为，并保持良好的外部可引入性，
供策略回测引擎等第三方使用。

一句话概括职责：**「成交事件 → 仓位/账户状态」的记账机。**

撮合、行情、订单簿由使用者的回测引擎负责——它们缺的恰恰是这台「OKX 记账机」。

> **状态：v0.2.0 开发中 —— 逐仓永续的仓位核算已可用。**
> 已支持开平加减仓与反手、已实现/未实现盈亏、手续费、保证金与全套风险指标
> （维持保证金、保证金率、强平价、破产价）、逐仓账户资金流转。
> 全仓、双向持仓、资金费与强平引擎在后续版本，详见 [版本排期](docs/roadmap.md)。

## 安装

```
go get github.com/dream-until-dawn/okx-position-simulator-go
```

`go 1.22`。核心包的依赖树**只有 `github.com/shopspring/decimal` 一个**——
`net/http` 被隔离在 `refdata/live` 子包，用不到就不会被引入。

## 快速开始

```go
sim, _ := okxsim.New(okxsim.Config{
    PosMode:      types.NetMode,
    RefData:      refdata.MustEmbedded(),
    DefaultLever: decimal.RequireFromString("5"),
})
sim.Deposit("USDT", decimal.RequireFromString("10000"))

// 灌入一笔成交。撮合由使用者的回测引擎负责，这里只管记账。
r, _ := sim.Fill(okxsim.Fill{
    InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
    Side: types.Buy, PosSide: types.PosNet,
    Sz: d("4"), Px: d("78000"), ExecType: types.Taker, Ts: ts,
})
// r.After.Margin = 624   r.Fee = -1.56

sim.SetMark("BTC-USDT-SWAP", d("77000"))
m, _ := sim.MetricsOf("BTC-USDT-SWAP", types.PosNet)
// m.UPL = -40   m.MMR = 12.32   m.MgnRatio = 42.1356   m.LiqPx / m.BkPx

b, _ := sim.Balance("USDT")
// b.CashBal = 9374.44   b.IsoEq = 584   b.Eq = 9958.44
```

### 预下单计算

回测引擎在下单前需要知道「挂得起吗、能挂多少、成交后会怎样」——这些无法从
成交后的状态倒推：

```go
// 这个价位最多能开多少张
m, _ := sim.MaxSize("BTC-USDT-SWAP", types.TdIsolated, d("78000"))
// m.MaxBuy = 63.94   m.MaxSell = 63.94

// 挂 10 张要冻结多少
cost, _ := sim.OrderCost(okxsim.OrderReq{
    InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
    Side: types.Buy, PosSide: types.PosNet, Sz: d("10"), Px: d("78000"),
})
// cost.Frozen = 1563.9  （保证金 1560 + 手续费 3.9）
// cost.Affordable(availBal) 判断挂不挂得起

// 成交后仓位会变成什么样——预演不改变任何状态
pv, _ := sim.PreviewFill(fill)
// pv.After.Pos / AvgPx / Margin，反手时还有 pv.Reversed / ClosedSz / OpenedSz

// 引擎真挂单后告知模拟器，可用余额随之冻结；撤单或成交后释放
sim.PlaceOrder("ord-1", req)
sim.CancelOrder("ord-1")
```

平仓方向的挂单不产生冻结，反手委托只对开仓那一段冻结——两条都经实测确认。

### 资金费

**模拟器不产生资金费率**——它由标的溢价与利率决定，属于市场结果，回测不该去
预测它，正如不该预测价格。费率由调用方给出：

```go
step, _ := sim.Advance(okxsim.Bar{
    InstID: "BTC-USDT-SWAP", Last: d("78000"), Ts: ts,
    Funding: &okxsim.Funding{Rate: d("0.0001")},   // 到结算时刻才给
})
for _, f := range step.Fundings { /* 本步结算的资金费 */ }
```

> ⚠️ **数据窗口限制：OKX 的历史资金费率只保留约 3 个月。**
>
> 实测 `funding-rate-history` 翻到底：BTC-USDT-SWAP / ETH-USDT-SWAP /
> DOGE-USDT-SWAP / BTC-USD-SWAP **四者全部截止在同一天**，距今 97 天，
> 各约 294 条——这是平台级的硬窗口，不是个别合约的问题。
>
> **超出该窗口的回测取不到真实费率。** 结算机制本身已按真实账单验证无误，
> 但它只在你能提供费率时才有意义。长周期回测请自行决定：接第三方数据源、
> 用常数近似（观测到的费率多在 `0.0001`，即 OKX 的基础利率），或干脆
> 不计资金费——后者会系统性高估多头持仓的收益，请知悉后再选。

窗口内的历史费率可从 `refdata/live` 免鉴权拉取：

```go
rs, _ := fetcher.FundingRateHistory(ctx, "BTC-USDT-SWAP", 0, after, 100)
// 用 RealizedRate，那是实际结算所用的费率
```

结算周期随合约而异（实测 49 个永续：16 个 4 小时、33 个 8 小时），由数据决定
而非模拟器推算。**逐仓的资金费从仓位保证金扣，不动现金余额**——这一点由真实
账单确证，记错地方会让逐仓权益与强平价一起算错。

### 内置撮合

撮合是每个回测引擎都要做的事，各自重写一遍容易在成交角色、冻结释放这类细节上
出错，因此内置：

```go
// 推进一根 K 线：更新价格、撮合挂单
step, _ := sim.Advance(okxsim.Bar{
    InstID: "BTC-USDT-SWAP", Last: d("77200"),
    High: d("78100"), Low: d("76800"), Ts: ts,
})
for _, f := range step.Fills { /* 本步产生的成交 */ }
```

| 项 | 模型 |
|---|---|
| 驱动 | 买单在 `Low ≤ 委托价` 时成交，卖单在 `High ≥ 委托价` 时成交 |
| 成交价 | 挂住的限价单按委托价；下单即可成交的按最新价，且不劣于限价 |
| **成交角色** | **由模拟器判定**——挂住后被触发是 maker，下单即成交是 taker。这直接决定手续费 |
| 委托类型 | `limit` / `market` / `post_only` / `ioc` / `fok`，含 `reduceOnly` |
| 时间优先 | 同一步内多笔可成交时按下单先后处理 |

**明确不建模**：盘口深度、部分成交、排队位置。没有深度数据就无从建模这三者，
强行假设只会给出看似精确实则杜撰的结果。委托在被触及时全额成交。

内置撮合**不是强制的**：引擎若有自己的撮合逻辑，仍可直接灌 `Fill`，
在其中带上 `OrdID` 即可让模拟器代为解除该委托的冻结。

`SetMark` 只更新价格、不触发任何风控——资金费结算与强平检查由 `Advance` 按时钟
推进（v0.4.0），这样多个合约在同一时刻的更新顺序就不会影响结果。

## 规则数据

零配置：内置快照随库分发，不联网即可使用。

```go
import (
    "github.com/dream-until-dawn/okx-position-simulator-go/refdata"
    "github.com/dream-until-dawn/okx-position-simulator-go/types"
    "github.com/shopspring/decimal"
)

rd := refdata.MustEmbedded()

inst, _ := rd.Instrument("BTC-USDT-SWAP")
tbl, _ := refdata.TierTableFor(rd, inst, types.MgnCross)

tier, _ := tbl.Lookup(decimal.RequireFromString("3000")) // 3000 张
// tier.MMR      该档维持保证金率
// tier.MaxLever 该档最大杠杆

rate, _ := rd.FeeSchedule().Rate(inst)
// rate.Taker = -0.0005（负数表示收取，可直接与余额相加）
```

档位表按 `instFamily` 而非 `instId` 聚合——全仓模式下同一品种的多个合约持仓
必须合并后再查档。这一点在类型层面就已固定，用 `TierTableFor` 可避免拼错键。

### 跟随 OKX 的规则变更

规则会变——档位表调整、合约上下架、面值变更。要跟进就用 `refdata/live`：

```go
p := live.NewProvider(live.NewFetcher(),
    live.WithInitialSnapshot(refdata.MustEmbedded()), // 兜底，首次拉取失败也可用
    live.WithFamilies("BTC-USDT", "ETH-USDT"),
    live.WithRefreshInterval(time.Hour),
    live.WithOnChange(func(c live.Changes) {
        log.Printf("OKX 规则有变:\n%s", c)
    }),
)
defer p.Stop()
p.Start(ctx)
```

拉取失败时**保留原有数据**——陈旧的规则远好过没有规则。
单张档位表失败时沿用上一份，不会让它凭空消失，也就不会被误读成品种下架。

### 自己的费率

费率是账户相关的，无法从免鉴权接口取得。内置的是 Lv1，实际不同请覆盖：

```go
fees := refdata.DefaultFeeSchedule().WithRate(types.InstSwap, refdata.FeeRate{
    Maker: decimal.RequireFromString("-0.00005"),
    Taker: decimal.RequireFromString("-0.0002"),
})
```

### 更大的快照

内置快照收录成交额头部 30 个正向品种 + 全部 15 个反向品种。需要更多：

```
go run ./cmd/refdata-sync -top 100 -out my-snapshot.json.gz
```

回测请务必用固定快照（`Snapshot` 不可变、`Version` 恒定），
否则规则在回测途中变动会让结果不可复现。

## 设计原则

**规则以实测为准，不以推断为准。** 公式与错误码都在 OKX 模拟盘上跑过真实操作核对。
已验证的四条核心公式差值在 `1e-13 ~ 1e-19` 量级（纯 decimal 精度残差）。
详见 [规则调研](docs/okx-rules.md)。

**无法证实的事不臆造。** 实测确认 OKX 从不因价格超精度而拒单，只做按方向取整，
因此模拟器复刻其取整而非强制校验——强制拒单会拒掉真实 OKX 会接受的订单，
回测里凭空少成交，与还原真实行为正好相反。费率组与结算币种变体的优先级
在现有数据上不可观测，采用的是使用者拍板的选择，并留有守卫测试在两条路径
开始分歧时报警。

**模拟盘 ≠ 生产环境。** 两者的 tickSz、杠杆上限与档位表均有实测差异
（BTC-USDT 档位区间相差 10 倍）。内置快照取自生产环境，仅供回测；
对拍模拟盘须用 `live.WithSimulated(true)` 从同一环境取数据。

**验收标准可自动化。**「100% 模拟」若不能被自动验证，就只是一句口号。
`cmd/conformance` 是那个验证：在模拟盘上真实下单，把同一批成交灌进模拟器，
逐字段比对仓位与账户。当前 66 个字段全部一致，差值在 `1e-15 ~ 1e-11`（decimal 精度残差）。

```
cd cmd/conformance && go run . -inst BTC-USDT-SWAP -lever 5
```

它是独立的嵌套模块：需要 OKX SDK 才能发起已签名请求，而主模块要保持
「依赖树只有 decimal 一个」。代价是根目录的 `go build ./...` 不含它，需单独进入执行。

## 文档

- [规则调研](docs/okx-rules.md) —— 公式、字段陷阱、实测记录
- [架构设计与决策记录](docs/design.md)
- [版本排期](docs/roadmap.md)

## 许可

MIT

# okx-position-simulator-go

用 Go 实现 OKX 的仓位管理内核，力求贴近 100% 还原真实行为，并保持良好的外部
可引入性，供策略回测引擎等第三方使用。

一句话概括职责：**「行情与成交 → 仓位/账户状态」的记账机。**

> **状态：v0.7.0。**
> 已可用：**正向与币本位** × **逐仓与全仓**四个象限的完整核算（开平加减仓与反手、
> 盈亏、手续费、全套风险指标）、内置撮合、**算法委托**、预下单计算与挂单冻结、
> 资金费结算、强平。剩余排期见 [版本排期](docs/roadmap.md)。

## 安装

```
go get github.com/dream-until-dawn/okx-position-simulator-go
```

`go 1.22`。核心包的依赖树**只有 `github.com/shopspring/decimal` 一个**——
`net/http` 隔离在 `refdata/live` 子包，对拍工具是独立的嵌套模块，用不到就不会引入。

---

## 快速开始

```go
sim, _ := okxsim.New(okxsim.Config{
    PosMode:      types.NetMode,
    RefData:      refdata.MustEmbedded(),   // 内置快照，零配置可用
    DefaultLever: d("5"),
})
sim.Deposit("USDT", d("10000"))

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

### 全仓

换个 `TdMode` 就是全仓，但记账方式完全不同——保证金不离开现金余额，
风险按**结算币种**整体核算：

```go
sim.Fill(okxsim.Fill{InstID: "ETH-USDT-SWAP", TdMode: types.TdCross, /* ... */})
// 现金只少了手续费；仓位的 Margin 恒为 0

cm, _ := sim.CrossMetricsOf("USDT")
// cm.Equity = 现金 + 全仓浮盈   cm.IMR / cm.MMR / cm.MgnRatio / cm.LiqPx
// cm.IsLiquidatable() 是全仓的强平判据——同币种的全仓仓位共担一份权益
```

| | 逐仓 | 全仓 |
|---|---|---|
| 开仓时的现金 | 划走「保证金 + 手续费」 | **只扣手续费** |
| 亏损吃掉的是 | 仓位保证金，与现金隔离 | 现金余额，同币种全仓仓位共担 |
| 强平层级 | 仓位级，各爆各的 | **结算币种级** |
| 查档 | 每个仓位单独查 | 同家族**绝对值**合计后查一次 |

最后一行最容易搞错，且只在多仓位时才暴露。实测两个交割期各持 7000 张，
先开的那条腿**一张未动**，仅因另一期开了仓，其维持保证金率就从 0.01 跳到 0.015。
多空对冲也一样按绝对值相加——净额为零不代表回到低档。

### 币本位

换个合约就是币本位，账户层不用改一行——只是计价单位从 USDT 变成标的币：

```go
sim.Deposit("BTC", d("1"))
sim.Fill(okxsim.Fill{InstID: "BTC-USD-SWAP", TdMode: types.TdIsolated, /* ... */})

b, _ := sim.Balance("BTC")     // 一切以 BTC 计
m, _ := sim.MetricsOf("BTC-USD-SWAP", types.PosLong)
```

差别只在名义价值：`Q` 以**计价币**计（`BTC-USD-SWAP` 是 100 USD 一张），
除以价格才得到标的币金额。于是所有随名义变化的量都变成价格的反比，
强平价也从正向的形态变成它的**倒数对偶**。

⚠️ **币本位空头的亏损有上界。** `Q(1/P − 1/avgPx)` 在价格涨到无穷时收敛于
`−Q/avgPx`，保证金若已覆盖它就**永远爆不掉**——此时本库返回「无强平价」而不是
一个负数。正向空头没有这个性质，照搬正向公式会在这里给出荒谬的值。

---

## 回测循环

`Advance` 推进一步行情，一步之内的执行顺序是确定的：
**资金费结算 → 撮合 → 强平检查**。

```go
step, _ := sim.Advance(okxsim.Bar{
    InstID: "BTC-USDT-SWAP",
    Last: d("77200"), High: d("78100"), Low: d("76800"), Ts: ts,
    Funding: &okxsim.Funding{Rate: d("0.0001")},   // 到结算时刻才给，留空即不计
})

step.Fundings      // 本步结算的资金费
step.Fills         // 本步产生的成交
step.Liquidations  // 本步发生的强平
step.Canceled      // 本步被撤销的委托及其原因
```

`SetMark` 只更新价格、不触发任何风控——这样多个合约在同一时刻的更新顺序就不会
影响结果。

### 算法委托

计划委托、止盈止损、OCO、移动止盈止损，接在 `Advance` 里：

```go
sim.PlaceAlgoOrder(okxsim.AlgoOrder{
    AlgoID: "sl-1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
    Side: types.Sell, PosSide: types.PosLong, OrdType: types.AlgoMoveStop,
    Sz: d("2"), ReduceOnly: true, CallbackRatio: d("0.05"),   // 5% 移动止损
})

for _, a := range step.AlgoTriggers {
    log.Println(a)     // 算法委托 sl-1（BTC-USDT-SWAP move_order_stop）移动止损 于 76000 触发，当场成交
    a.Leg              // trigger / tp / sl / move
    a.Fill             // 当场成交的结果；转成限价挂单时为 nil
    a.Reason           // 触发了却下不出单时说明原因（资金不足、缺指数价…）
}
```

**算法委托不占用任何资金**——不进订单簿、不冻结保证金、不计入 imr/mmr。实测挂四张
计划委托，`availBal`/`ordFrozen`/`imr`/`mmr` 全部纹丝不动。触发后才生成一笔**普通
委托**，随后走与手工下单完全相同的路径。

| 类型 | 说明 |
|---|---|
| `trigger` | 计划委托 |
| `conditional` | 止盈止损，**只认一条腿**（与 OKX 一致：同时给两组参数只保留止损） |
| `oco` | 双腿，任一触发则整笔作废 |
| `move_order_stop` | 移动止损，触发价跟极值棘轮、只进不退 |

触发价类型 `last` / `index` / `mark` 都支持。用 `index` 时须在 `Bar.IdxPx` 给出指数
价——本库**不拿别的价格顶替**，缺了就在 `Reason` 里说明，而不是悄悄跳过。

> ⚠️ 一根 K 线只给区间不给路径。`last` 用 `[Low, High]` 整段判，`mark`/`index`
> 只有单点值、分辨率更粗。移动止损**先按本步的有利极值棘轮再判触发**，等于假定
> 有利的那一段先发生——这是个明确的取舍，方向上偏保守（止损触发得更频繁）。

### 内置撮合

| 项 | 模型 |
|---|---|
| 驱动 | 买单在 `Low ≤ 委托价` 时成交，卖单在 `High ≥ 委托价` 时成交 |
| 成交价 | 挂住的限价单按委托价；下单即可成交的按最新价，且不劣于限价 |
| **成交角色** | **由模拟器判定**——挂住后被触发是 maker，下单即成交是 taker。这直接决定手续费 |
| 委托类型 | `limit` / `market` / `post_only` / `ioc` / `fok`，含 `reduceOnly` |
| 时间优先 | 同一步内多笔可成交时按下单先后处理 |

```go
pr, _ := sim.PlaceOrder(okxsim.Order{
    OrdID: "o1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
    Side: types.Buy, PosSide: types.PosNet, OrdType: types.OrdLimit,
    Sz: d("4"), Px: d("77000"), Ts: ts,
})
// pr.State = live，pr.Cost.Frozen = 617.54
// 若被撤销，pr.Reason 说明原因（只挂单会立即成交 / 立即成交类够不着 / …）
```

**明确不建模**：盘口深度、部分成交、排队位置。没有深度数据就无从建模这三者，
强行假设只会给出看似精确、实则杜撰的结果。委托在被触及时全额成交。

内置撮合**不是强制的**：引擎若有自己的撮合逻辑，仍可直接灌 `Fill`，
在其中带上 `OrdID` 即可让模拟器代为解除该委托的冻结。

---

## 预下单计算

回测引擎在下单前需要知道「挂得起吗、能挂多少、成交后会怎样」——这些无法从
成交后的状态倒推：

```go
m, _ := sim.MaxSize("BTC-USDT-SWAP", types.TdIsolated, d("78000"))
// m.MaxBuy = 63.94   m.MaxSell = 63.94

cost, _ := sim.OrderCost(req)
// cost.Frozen = 1563.9（保证金 1560 + 手续费 3.9）
// cost.Affordable(availBal) / cost.Shortfall(availBal) 判断挂不挂得起、差多少

pv, _ := sim.PreviewFill(fill)   // 成交预演，不改变任何状态
// pv.After.Pos / AvgPx / Margin；反手时还有 pv.Reversed / ClosedSz / OpenedSz
```

平仓方向的挂单不产生冻结，反手委托只对开仓那一段冻结——两条都经实测确认。

`MaxSize` 在全仓下还要扣掉**开仓瞬间就会产生的盯市浮亏**：全仓的浮亏直接减可用
余额，逐仓的落在仓位保证金上、不动可用余额。差距是数量级的——同一账户同一杠杆、
标记价 2446，以 1712 挂卖单，逐仓能开 122.49 张，全仓只有 30.74 张。

它还有一侧要在委托价与标记价之间取更保守的那个，**而是哪一侧取决于合约类型**：
正向是卖出、币本位是买入。统一的解释是「谁的保证金随不利行情变大，谁取保守价」
——正向名义 `Q×px` 空头怕涨，反向名义 `Q/px` 多头怕跌。

`PreviewFill` 与 `Fill` 走**同一套核算**，因此预演结果与真实成交必然一致。

---

## 资金费与强平

### 资金费

**模拟器不产生资金费率**——它由标的溢价与利率决定，属于市场结果，回测不该去
预测它，正如不该预测价格。费率由调用方经 `Bar.Funding` 给出，**留空即不计**。

> **默认不计资金费**，而这也是绝大多数长周期回测的实际状态：
>
> ⚠️ OKX 的历史资金费率**只保留约 3 个月**。实测四个合约翻到底，
> **全部截止在同一天**，距今 97 天——这是平台级硬窗口。
>
> 超出该窗口取不到真实费率，须自行接第三方数据源、用常数近似（观测到的费率
> 多为 `0.0001`，即 OKX 的基础利率），或不计资金费。
> **不计资金费会系统性高估多头持仓的收益**，请知悉后再选。

窗口内的历史费率可从 `refdata/live` 免鉴权拉取：

```go
rs, _ := fetcher.FundingRateHistory(ctx, "BTC-USDT-SWAP", 0, after, 100)
// 用 RealizedRate，那是实际结算所用的费率
```

**逐仓的资金费从仓位保证金扣，不动现金余额**——由真实账单确证，
记错地方会让逐仓权益与强平价一起算错。

### 强平

链路是 `撤单 → 阶梯减仓 → 全部强平`，接在 `Advance` 的最后一步。
触发判据用**标记价**而非最新成交价——用最新价会让插针扫掉本不该爆的仓位。

```go
for _, l := range step.Liquidations {
    log.Println(l)   // BTC-USDT-SWAP net 全部强平：平掉 10 张 @ …，损失保证金 …
    l.MgnMode        // isolated 是仓位级，cross 是结算币种级
    l.Kind           // full / partial（阶梯减仓时带降档前后的档位）
    l.Loss           // 持仓方实际损失，封顶为该仓位的保证金
    l.Penalty        // 爆仓罚金 = 名义价值 × 维持保证金率
    l.Excess         // 超出保证金、由风险准备金承担的部分
}
```

逐仓的结算方式取自一次**真实强平**的逐字段核对：现金余额 `balChg` 恒为 0，
保证金不多不少全额损失。

全仓是**结算币种级**的：同币种的全仓仓位共担一份权益，一旦触发就一并了结——
因此推进某个合约的 K 线后，必须把该结算币种整体再看一眼。触发判据经 13 个真实
快照核对，但**触发之后如何了结尚未实测**（真实全仓爆仓需要裸仓位吃掉整个币种的
现金，模拟盘资金规模不足以复现），当前是从逐仓平移过来的，且**全仓的阶梯减仓
未建模**、一律全平——偏保守，回测里表现为高估爆仓的杀伤力而非低估。

详见 [规则调研 §7](docs/okx-rules.md)。

---

## 事件与错误：把「为什么」讲清楚

引擎最难排查的不是「结果不对」，而是「什么都没发生却不知道为什么」。
凡是拒绝或撤销了什么，都同时给出可判定的原因：

```go
// 撤单原因：user / post_only_would_take / immediate_unfilled /
//           insufficient_funds / liquidation
for _, c := range step.Canceled {
    log.Println(c)          // 委托 o1（BTC-USDT-SWAP）被撤销：资金不足以承接该成交——…
    c.Reason.Describe()     // 中文说明
}

// 资金缺口：不必解析错误文本
if sf, ok := okxsim.ShortfallOf(err); ok {
    log.Printf("差 %s %s", sf.Missing, sf.Ccy)   // 可用 / 需要 / 缺口
}

log.Println(step.Describe())   // 一步之内全部事件的摘要
```

错误码与 OKX 对齐，可用 `okxerr.HasCode(err, okxerr.CodeInsufficientBal)` 判定。
收录的错误码**全部实测取得**，未经证实的不予收录。

---

## 规则数据

零配置：内置快照随库分发，不联网即可使用。

```go
rd := refdata.MustEmbedded()
inst, _ := rd.Instrument("BTC-USDT-SWAP")
tbl, _ := refdata.TierTableFor(rd, inst, types.MgnCross)
tier, _ := tbl.Lookup(d("3000"))   // tier.MMR / tier.MaxLever
```

档位表的键是 `(instType, instFamily, mgnMode)`。不是 `instId`——全仓下同品种
同产品类型的多个合约持仓要合并后再查档；也不能省掉 `instType`——同一 `instFamily`
下的永续与交割**不共用一张表**，实测 GRASS-USDT 的一档上限折合标的币差 8 倍。
这一点在类型层面就已固定，用 `TierTableFor` 可避免拼错键。

### 跟随 OKX 的规则变更

```go
p := live.NewProvider(live.NewFetcher(),
    live.WithInitialSnapshot(refdata.MustEmbedded()),  // 兜底
    live.WithRefreshInterval(time.Hour),
    live.WithOnChange(func(c live.Changes) { log.Printf("规则有变:\n%s", c) }),
)
defer p.Stop()
p.Start(ctx)
```

拉取失败时**保留原有数据**——陈旧的规则远好过没有规则。单张档位表失败时沿用
上一份，不会让它凭空消失、被误读成品种下架。

回测请务必用固定快照（`Snapshot` 不可变、`Version` 恒定），否则规则在回测途中
变动会让结果不可复现。

### 自己的费率

费率是账户相关的，无法从免鉴权接口取得。内置的是 Lv1，实际不同请覆盖：

```go
fees := refdata.DefaultFeeSchedule().WithRate(types.InstSwap, refdata.FeeRate{
    Maker: d("-0.00005"), Taker: d("-0.0002"),   // 负数表示收取
})
```

### 更大的快照

内置快照收录成交额头部 30 个正向品种 + 全部 15 个反向品种。需要更多：

```
go run ./cmd/refdata-sync -top 100 -out my-snapshot.json.gz
```

---

## 设计原则

**规则以实测为准，不以推断为准。** 公式与错误码都在 OKX 模拟盘上跑过真实操作
核对，包括真实触发一次强平。文档里查不到、只能实测出来的规则有十余条，
逐条记在 [规则调研](docs/okx-rules.md) 里并标注了证据等级。

**无法证实的事不臆造。** 证据不足的地方（费率组优先级、阶梯减仓路径、全仓强平的
结算方式）在代码与文档中明确标注，并留有守卫测试，在前提悄悄失效时报警。

反过来，**看似合理的数也不能凭空给**：全仓强平价在同币种持有多个合约时算得出一个
数，但那两个合约的标记价并不同步，这个数没有意义——OKX 此时返回空串，本库同样
留空。给一个看似合理的错数，比留空更有害。

**净效果相同不等于字段相同。** 强平的结算曾按另一种方式建模，净效果一致但四个
字段全对不上；照实测重写后才对齐。字段级同构是本项目的验收标准之一。

**标定时要盯住数据的可信边界。** 全仓的公式一开始怎么都对不齐，残差 `1e-4`；
根因是余额响应与持仓响应**不在同一瞬时**，而全仓浮盈本身只有 0.07 的量级，
漂移盖过了要验的东西。改成只用单次响应内部的字段互验，公式立刻收敛到 `1e-14`。

**验收标准可自动化。**「100% 模拟」若不能被自动验证，就只是一句口号。
`cmd/conformance` 是那个验证：

```
cd cmd/conformance
go run . -inst BTC-USDT-SWAP -lever 5   # 真实下单后逐字段对拍
go run . -mode check                     # 核对现有仓位的风险指标，不交易
go run . -mode adjust -sz 0.2            # 在现有仓位上加减仓，含累计量
```

三种模式互补。`check` 能覆盖下单流程难以构造的情形，且四个象限都走得通——
实测一次同时核对币本位的全仓空头与逐仓多头、外加两个距强平 1.2% 的 40 倍杠杆
正向逐仓多空仓位，**27 个字段全部一致**。

> ⚠️ **模拟盘 ≠ 生产环境。** 两者的 `tickSz`、杠杆上限与档位表均有实测差异
> （BTC-USDT 档位区间相差 10 倍）。内置快照取自生产环境，仅供回测；
> 对拍模拟盘须用 `live.WithSimulated(true)` 从同一环境取数据。

---

## 文档

- [规则调研](docs/okx-rules.md) —— 公式、字段陷阱、实测记录、尚未定论的部分
- [架构设计与决策记录](docs/design.md)
- [版本排期](docs/roadmap.md) —— 含排期变更记录与覆盖缺口

## 许可

MIT

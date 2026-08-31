# okx-position-simulator-go

用 Go 实现 OKX 的仓位管理内核，力求贴近 100% 还原真实行为，并保持良好的外部可引入性，
供策略回测引擎等第三方使用。

一句话概括职责：**「成交事件 → 仓位/账户状态」的记账机。**

撮合、行情、订单簿由使用者的回测引擎负责——它们缺的恰恰是这台「OKX 记账机」。

> **状态：v0.1.0 —— 规则数据层已就绪，仓位核算尚未开始。**
> 当前可用的是合约规格、档位表、费率与自动拉取；`Simulator` 门面将在 v0.2.0 落地。
> 详见 [版本排期](docs/roadmap.md)。

## 安装

```
go get github.com/dream-until-dawn/okx-position-simulator-go
```

`go 1.22`。核心包的依赖树**只有 `github.com/shopspring/decimal` 一个**——
`net/http` 被隔离在 `refdata/live` 子包，用不到就不会被引入。

## 快速开始

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

**无法证实的事不臆造。** tickSz 是否强制校验、费率组与结算币种变体孰先——
这两处证据不足以定论，代码中不编造规则，也不为其虚构错误码，
并留有守卫测试防止前提悄悄失效。

**验收标准可自动化。**「100% 模拟」被定义为：输出结构体与 OKX REST 响应字段级同构、
错误码与 OKX 对齐、模拟盘真实下单逐字段 diff。否则它只是一句口号。

## 文档

- [规则调研](docs/okx-rules.md) —— 公式、字段陷阱、实测记录
- [架构设计与决策记录](docs/design.md)
- [版本排期](docs/roadmap.md)

## 许可

MIT

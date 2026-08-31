# 内置快照

`embedded.json.gz` 是随库分发的规则数据快照，由 `cmd/refdata-sync` 从 OKX 公共接口生成。

## 重新生成

```
go run ./cmd/refdata-sync -out refdata/snapshot/embedded.json.gz
```

## 收录范围

不是全量。实测 459 个永续合约，每个品种的逐仓与全仓档位表各约 20 KB 且互不相同，
全量约 18 MB，压缩后仍有一两兆，不适合嵌入二进制。

因此按 24 小时**成交额**取正向合约的头部 30 个品种，并收录全部反向合约（15 个）。

> 排序必须用成交额（`volCcy24h × last`）而非 `volCcy24h`。后者是标的币的**数量**，
> 不同币种相差若干数量级——直接排序会让 SATS、PEPE、SHIB 占满榜首，BTC 连前三十都进不去。

需要更多品种：调大 `-top`，或用 `-all`，或改用 `refdata/live` 在运行时拉取。

## 费率

快照中的费率是 `refdata.DefaultFeeSchedule()`，即 Lv1 等级，取值经模拟盘实测确认。

费率是账户相关的，`/api/v5/account/trade-fee` 需要鉴权且返回的是「当前账户」的费率，
无法作为公共规则拉取，故内置的只能是默认等级。实际费率不同请用 `FeeSchedule.WithRate` 覆盖。

## 确定性

快照按 instId 与档位表键排序后写出，同样的数据产生逐字节相同的文件，
因此可以纳入版本控制并做有意义的 diff。只有 `ts` 字段每次生成都会变。

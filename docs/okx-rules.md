# OKX 规则调研

> 本文是实现的事实依据。所有公式均与 OKX 官方帮助中心核对过，参考数据为直接调用公共 REST 接口取得的一手数据。
> 调研时间：2026-08-31

## 1. 参考数据来源（免鉴权公共接口，全部实测可达）

| 用途 | 接口 |
|---|---|
| 合约规格 | `GET /api/v5/public/instruments?instType=SWAP` |
| 档位 / MMR / IMR 表 | `GET /api/v5/public/position-tiers?instType=SWAP&tdMode={cross\|isolated}&instFamily=...` |
| 折算率（跨币种保证金用） | `GET /api/v5/public/discount-rate-interest-free-quota?ccy=...` |
| 资金费率 | `GET /api/v5/public/funding-rate?instId=...` |
| 标记价格 | `GET /api/v5/public/mark-price?instType=SWAP&instId=...` |

实测样本：

```
BTC-USDT-SWAP  ctType=linear   ctVal=0.01 BTC  ctMult=1  settleCcy=USDT  lotSz=minSz=0.01  tickSz=0.1  maxLever=100
BTC-USD-SWAP   ctType=inverse  ctVal=100 USD   ctMult=1  settleCcy=BTC   lotSz=minSz=0.1   tickSz=0.1  maxLever=100
```

档位表（BTC-USDT / BTC-USD 各 **99 档**，cross 与 isolated 是两张独立的表）：

```
tier1  sz[0,       1000 ]  mmr=0.004   imr=0.01   maxLever=100
tier2  sz[1000.01, 5000 ]  mmr=0.005   imr=0.015  maxLever=66.66
tier3  sz[5000.01, 20000]  mmr=0.0075  imr=0.02   maxLever=50
tier4  sz[20000.01,40000]  mmr=0.0125  imr=0.025  maxLever=40
tier5  sz[40000.01,60000]  mmr=0.0175  imr=0.03   maxLever=33.33
```

⚠️ 档位下界的"递增步长"跟随 `lotSz`：USDT 本位是 `1000.01`，币本位是 `2000.1`。**这种边界不可推导，只能读表。**

## 2. 核心公式

约定：`Q = ctVal × |sz| × ctMult`（合约名义数量），价格一律用**标记价格 markPx**，已实现盈亏才用成交价。

### 2.1 未实现盈亏 uPL

```
linear  多: Q × (markPx − avgPx)
linear  空: Q × (avgPx − markPx)
inverse 多: Q × (1/avgPx − 1/markPx)
inverse 空: Q × (1/markPx − 1/avgPx)
```

收益率 = uPL / 开仓保证金

### 2.2 初始保证金 IMR

```
linear :  Q × markPx / lever
inverse:  Q / (markPx × lever)
```

### 2.3 维持保证金 MMR

**单档整体适用，不是分层累进。** 按仓位规模落入的那一档的 `mmr` 整体乘以仓位价值。

> 这是第三方实现最常见的错误来源——很多实现照搬了阶梯税式的累进算法。

```
MMR = 仓位价值 × mmr(tier)
多仓位时各仓位分别算，再求和。
```

### 2.4 保证金率与强平触发

```
mgnRatio = 有效保证金 / (维持保证金 + 强平手续费)
```

其中有效保证金（全仓、单币种口径）：

```
该币种全仓余额 + 全仓收益 − 挂单卖出占用 − 期权买单占用 − 逐仓开仓占用 − 挂单手续费
```

**`mgnRatio ≤ 100%` 时触发：撤单 → 强制平仓。**

### 2.5 强平价 / 破产价（逐仓 linear，由 `equity = MM + closeFee` 解出）

```
多: liqPx = (avgPx × Q − margin) / (Q × (1 − mmr − takerFee))
空: liqPx = (avgPx × Q + margin) / (Q × (1 + mmr + takerFee))

破产价 多: avgPx − margin / Q
破产价 空: avgPx + margin / Q
```

### 2.6 手续费

- Lv1：永续 maker `0.020%` / taker `0.050%`
- 计费币种跟随 `settleCcy`：linear 收 USDT，inverse 收 BTC
- 开仓与平仓均按成交名义价值计费

### 2.7 资金费

```
资金费 = 持仓价值(按标记价) × fundingRate
fundingRate > 0：多头付空头
```

⚠️ **结算周期必须从接口的 `fundingTime` / `nextFundingTime` 读取，不要硬编码 8h。** 现在有大量合约是 4h 或 1h。

## 3. 已识别的高风险点

按踩坑代价排序。

1. **档位按 `instFamily` 合并，不是按 `instId`。**
   全仓模式下 `BTC-USDT-SWAP` 与 `BTC-USDT-260327` 的持仓要**合并**后再查档位。
   若按 instId 建模，后期必须推倒重来 —— 因此数据模型第一天就要以 instFamily 为聚合键。

2. **净持仓模式(net)的反手。**
   卖出量 > 多头持仓时，会平掉多头并**反向开空**，均价重置。边界逻辑 bug 高发区。

3. **部分平仓不改变开仓均价**，只结算已实现盈亏；只有加仓才做加权平均。

4. **强平是多阶段的**，不是一步到位：
   `撤单 → 阶梯部分减仓(降档后重算 MMR，仓位可能被救回) → 全平 → 穿仓`

5. **全仓强平是账户级的**（现货合约模式按 `settleCcy` 分币种），逐仓才是仓位级。
   两者是完全不同的代码路径，不要试图复用。

6. **组合保证金(PM, acctLv=4) 的 MMR 是压力测试矩阵算出来的**，不是查表。
   工作量约等于本项目其余部分之和 —— 已明确排除在 v1.0 范围外。

## 4. 来源

- [合约盈亏计算规则](https://www.okx.com/zh-hans/help/futures-pnl-calculation-rules)
- [阶梯维持保证金率规则](https://www.okx.com/zh-hans/help/v-tiered-maintenance-margin-ratio-rules)
- [强平价格计算](https://www.okx.com/zh-hans/help/how-to-calculate-leverage-forced-liquidation-prices-and-how-to-avoid-being-forced-to-close)
- [手续费规则 FAQ](https://www.okx.com/en-us/help/trading-fee-rules-faq)
- [OKX API v5 文档](https://www.okx.com/docs-v5/zh/)

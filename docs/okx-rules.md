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

## 4. 实测验证记录

以下结论**不是从文档抄来的**，而是在 OKX 模拟盘上执行真实操作后逐字段核算得出。
数据原文保存在 `testdata/conformance/close-long-isolated-linear.json`。

样本：一笔 ETH-USDT-SWAP 逐仓多头（49.18 张，开仓均价 1805.5767…）的市价全平。

### 4.1 公式验证（差值为纯 decimal 精度残差）

| 验证项 | 公式 | 差值 |
|---|---|---|
| 已实现盈亏 | `ctVal × sz × ctMult × (closeAvgPx − openAvgPx)` | `1.6e-15` |
| 吃单手续费 | `ctVal × sz × ctMult × closeAvgPx × 0.0005` | `6.9e-19` |
| 未实现盈亏 | `ctVal × pos × ctMult × (markPx − avgPx)` | `4.6e-13` |
| 维持保证金率 | 由 `position.mmr ÷ 名义价值` 反推得 `0.004`，正是 ETH-USDT 首档 mmr | 精确命中 |

### 4.2 逐仓账务模型

```
平仓后 cashBal = 平仓前 cashBal + margin + pnl + fee      差值 -1.1e-14
isoEq          = margin + upl
eq(币种权益)   = cashBal + isoEq
frozenBal      = isoEq
```

第一条确立了一个关键事实：**逐仓保证金是从 cashBal 划走的，不是冻结**。
「保证金仅冻结、cashBal 不变」的假设与实测差整整一个 margin，已被排除。

逐笔 fill 的 fee 之和**精确等于**订单级 fee（差值为 0）。

### 4.3 字段语义陷阱

这几个字段的名字与实际含义不符，是「字段级同构」最容易翻车的地方：

| 字段 | 实际含义 |
|---|---|
| `position.mmr` | 维持保证金**金额**（如 47.91 USDT），**不是比率** |
| `order.fillSz` | **最新一笔**成交的数量，不是累计量 |
| `order.accFillSz` | **累计**成交数量，全部成交后才等于 sz |
| `balance.frozenBal` | 逐仓占用体现在此，数值等于 isoEq |

一笔市价单会被拆成多笔成交（样本中为 15 笔），每笔独立计费、`execType` 均为 `T`。

### 4.4 错误码（全部实测取得）

| 码 | 含义 | 实测触发条件 |
|---|---|---|
| `50014` | Parameter {x} can not be empty | 公共接口缺必填参数 |
| `50015` | Either parameter {a} or {b} is required | 缺 instFamily/uly |
| `51000` | Parameter {x} error | posSide / tdMode / sz 格式非法 |
| `51001` | Instrument ID doesn't exist | 不存在的 instId |
| `51005` | Order amount exceeds the max order amount | 张数超档位上限 |
| `51006` | Order price is not within the price limit | 超出价格带 |
| `51008` | Available balance is insufficient | 可用余额不足 |
| `51121` | Order quantity must be a multiple of the lot size | 非 lotSz 整数倍 |

实测 459 个永续合约的 `minSz` 与 `lotSz` **全部相等**，因此「低于最小下单量」与
「非数量精度整数倍」对永续而言是同一件事，OKX 统一返回 `51121`。

### 4.5 tickSz：OKX 从不拒绝，只做按方向取整

在三个 tickSz 量级不同的合约上各试了对齐与超精度的价格，结论一致：

| 合约 | tickSz | 对齐位数 | 超出位数的处理 |
|---|---|---|---|
| BTC-USDT-SWAP | 0.01 | 2 位保留 | 3+ 位截断到 2 位 |
| YFI-USDT-SWAP | 0.1 | 1 位保留 | 2+ 位截断到 1 位 |
| DOGE-USDT-SWAP | 1e-7 | 7 位保留 | 8+ 位截断到 7 位 |

**没有任何一笔被拒绝。** 取整按买卖方向进行（YFI，tickSz=0.1）：

```
买单  2150.51 / 2150.55 / 2150.59  ->  2150.5   向下截断
卖单  2376.91 / 2376.95 / 2376.99  ->  2377     向上进位
```

两个方向都是朝远离市价的一侧移动，即让委托变得更不激进，不会凭空提高成交概率。

**因此模拟器不应强制校验价格精度。** 强制拒单会拒掉真实 OKX 会接受的订单，
回测中凭空少掉成交——那与还原真实行为的目标正好相反。正确做法是复刻其取整
（`Instrument.RoundPrice`），并以取整后的价格记账。`ValidatePriceTick` 保留为
显式调用的**诊断工具**，供使用者自查策略是否在下超精度价格，不参与主路径，
也不为其虚构 OKX 错误码——因为 OKX 根本不报错。

### 4.6 模拟盘与生产环境的规则数据并不相同

这是对拍策略中最容易埋雷的一点。实测差异：

| 合约 | 字段 | 生产 | 模拟盘 |
|---|---|---|---|
| BTC-USDT-SWAP | tickSz | 0.1 | 0.01 |
| DOGE-USDT-SWAP | tickSz | 0.00001 | 0.0000001 |
| YFI-USDT-SWAP | tickSz | 1 | 0.1 |
| YFI-USDT-SWAP | lever | 20 | 50 |
| BTC-USD-SWAP | lever | 100 | 125 |
| ETH/DOGE/YFI/BTC-USD | maxMktSz | 均不同 | |

档位表差异更彻底：

| 品种 | 差异 |
|---|---|
| BTC-USDT / ETH-USDT | 档位区间上界是生产的 **10 倍**（BTC 首档 1000 → 10000），99 档全部不同 |
| BTC-USD | 档位数 **99 vs 100**，且 `imr` 0.01 → 0.008、`maxLever` 100 → 125 |

**对拍工具必须从它所交易的同一环境拉取规则数据。** 拿生产快照去对拍模拟盘，
每一个依赖档位、价格精度或杠杆的计算都会偏，而偏差看起来像模拟器自身的缺陷，
会把大量时间浪费在追查根本不存在的问题上。

`live.NewFetcher(live.WithSimulated(true))` 用于指向模拟盘环境。
随库分发的内置快照取自**生产环境**，仅供回测使用，不可用于对拍模拟盘。

### 4.7 尚未定论

**费率组与结算币种变体的优先级。** 实测 459 个永续合约的 `groupId` 均为 4，
且 SWAP 费率表 1..7 组与 `maker/taker/makerU/takerU` 取值完全一致，两条路径
给出同一个数字，无从从数据中判定。现采用**费率组优先**——这是使用者拍板的
决定而非实测结论，依据是现货各费率组差异显著（组 3 taker 0.22%、组 8 是 0.4%、
组 11 免费），说明费率组是 OKX 用于精细区分的维度，精细者优先是合理读法。
`TestSwapFeePathsCurrentlyAgree` 会在两条路径开始分歧时失败，届时需用真实成交
重新验证该选择。

## 5. 来源

- [合约盈亏计算规则](https://www.okx.com/zh-hans/help/futures-pnl-calculation-rules)
- [阶梯维持保证金率规则](https://www.okx.com/zh-hans/help/v-tiered-maintenance-margin-ratio-rules)
- [强平价格计算](https://www.okx.com/zh-hans/help/how-to-calculate-leverage-forced-liquidation-prices-and-how-to-avoid-being-forced-to-close)
- [手续费规则 FAQ](https://www.okx.com/en-us/help/trading-fee-rules-faq)
- [OKX API v5 文档](https://www.okx.com/docs-v5/zh/)

package okxsim

import (
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// Metrics 是一个仓位在给定标记价下的风险指标。
//
// 全部现算，不缓存——仓位状态里不存这些字段，就不存在某个指标还停留在上一个
// 行情快照上的可能。
//
// 字段命名与 OKX /api/v5/account/positions 的响应对应，含其中几个名不副实的：
// MMR 是维持保证金的**金额**而非比率（比率是 MMRRate），这一点已由真实数据确认。
type Metrics struct {
	MarkPx   decimal.Decimal // 标记价
	Qty      decimal.Decimal // 合约名义数量 Q = ctVal × |pos| × ctMult
	Notional decimal.Decimal // 名义价值（结算币计，按标记价）
	UPL      decimal.Decimal // 未实现盈亏
	UPLRatio decimal.Decimal // 收益率
	IMR      decimal.Decimal // 初始保证金（金额）
	MMR      decimal.Decimal // 维持保证金（金额）
	MMRRate  decimal.Decimal // 维持保证金率（来自档位表）
	CloseFee decimal.Decimal // 平仓手续费（正数），参与保证金率的分母
	Equity   decimal.Decimal // 权益：逐仓为 margin + upl，全仓为所属结算币种的全仓权益
	MgnRatio decimal.Decimal // 保证金率，≤ 1 触发强平；全仓取所属结算币种的整体值
	LiqPx    decimal.Decimal // 预估强平价；全仓取所属结算币种的整体值
	BkPx     decimal.Decimal // 破产价（仅逐仓）
	BePx     decimal.Decimal // 盈亏平衡价，对应 OKX 的 bePx
	Tier     int             // 所处档位

	// UplLastPx / UplRatioLastPx 是按【最新成交价】而非标记价算出的浮盈与收益率。
	//
	// OKX 两套都给。强平判据用的是标记价那套（UPL / UPLRatio），但回测通常以成交价
	// 撮合，与回测口径对得上的是这一套。两者在插针时能差出很多——实测同一仓位
	// upl 为 -34.50 而 uplLastPx 为 +0.50，一亏一赚。
	//
	// 未设置过最新价时为零。
	UplLastPx      decimal.Decimal
	UplRatioLastPx decimal.Decimal

	// HasPosition 报告这组指标是否来自一个真实存在的仓位。
	//
	// 不能拿 MgnRatio 是否为零来代替它：权益被亏损或资金费耗尽时保证金率同样是
	// 零，而那恰恰是最该强平的时刻。二者混为一谈会让爆到穿仓的仓位反而爆不掉。
	HasPosition bool
}

// ComputeMetrics 计算逐仓仓位在给定标记价下的风险指标。
//
// takerRate 传入费率表中的吃单费率，其符号沿用 OKX 约定（负数表示收取），
// 本函数内部取绝对值参与计算。
//
// 空仓返回零值。
func ComputeMetrics(pos Position, inst refdata.Instrument, tier refdata.PositionTier,
	markPx, takerRate decimal.Decimal) Metrics {

	m := Metrics{MarkPx: markPx, MMRRate: tier.MMR, Tier: tier.Tier}
	if pos.IsEmpty() || markPx.IsZero() {
		return m
	}
	m.HasPosition = true

	taker := takerRate.Abs()
	signed := pos.SignedPos()
	long := signed.IsPositive()

	m.Qty = inst.ContractQty(pos.AbsPos())
	m.Notional = notional(inst, pos.AbsPos(), markPx)

	m.UPL = unrealizedPnl(inst, signed, pos.AvgPx, markPx)

	// 收益率的分母是【开仓时】的初始保证金，不是当前 margin。
	// 两者在开仓瞬间相等，但逐仓的资金费是从保证金里扣的，持仓久了就会分离。
	// 实测样本：一个持有七周的仓位，margin 比开仓初始保证金少 75.36，
	// 用当前 margin 算出 1.8216，用开仓初始保证金算出 1.7443 —— OKX 给的是后者。
	if openIM := initialMargin(inst, pos.AbsPos(), pos.AvgPx, pos.Lever); !openIM.IsZero() {
		m.UPLRatio = div(m.UPL, openIM)
	}

	m.IMR = div(m.Notional, pos.Lever)
	m.MMR = m.Notional.Mul(tier.MMR)
	m.CloseFee = m.Notional.Mul(taker)

	m.BePx = breakEvenPx(long, pos.AvgPx, taker)

	// 以下三项在全仓下是【结算币种级】的，不属于单个仓位，此处留零，由
	// Simulator.MetricsOf 从 CrossMetricsOf 填入。OKX 的做法与之呼应：全仓仓位的
	// margin 与 liqPx 一律返回空串，而 mgnRatio 返回的正是币种级的那个值。
	if pos.MgnMode == types.MgnCross {
		return m
	}

	// 逐仓权益 = 划入的保证金 + 未实现盈亏
	m.Equity = pos.Margin.Add(m.UPL)

	// 保证金率 = 权益 / (维持保证金 + 平仓手续费)，≤ 1 触发强平。
	// 实测与 OKX 返回值差 2.2e-14。
	if den := m.MMR.Add(m.CloseFee); !den.IsZero() {
		m.MgnRatio = div(m.Equity, den)
	}

	m.LiqPx = liquidationPx(inst, long, pos.AvgPx, m.Qty, pos.Margin, tier.MMR, taker)
	m.BkPx = bankruptcyPx(inst, long, pos.AvgPx, m.Qty, pos.Margin)
	return m
}

// breakEvenPx 计算盈亏平衡价，即平仓后不赚不亏所需的价格。
//
// 由「价差恰好抵掉开仓与平仓两笔手续费」解出：
//
//	多头  avgPx × (1 + taker) / (1 − taker)
//	空头  avgPx × (1 − taker) / (1 + taker)
//
// 平仓那笔手续费按平仓价收取，所以未知数在等式两侧都出现，不能简单地写成
// avgPx × (1 + 2×taker)。实测样本：avgPx 2445.6、taker 0.0005，本式给出
// 2448.0468234117056，与 OKX 的 bePx 逐位相同；而近似式给出 2448.0456，差 1.2。
//
// 手续费一律按吃单费率计——挂单成交实际收的是挂单费率，但 OKX 的 bePx 不区分
// 成交角色。**反向合约同式**，已实测：BTC-USD-SWAP 多空两侧与 OKX 差 8e-11。
func breakEvenPx(long bool, avgPx, taker decimal.Decimal) decimal.Decimal {
	one := decimal.NewFromInt(1)
	if long {
		return div(avgPx.Mul(one.Add(taker)), one.Sub(taker))
	}
	return div(avgPx.Mul(one.Sub(taker)), one.Add(taker))
}

// unrealizedPnl 计算未实现盈亏。用标记价而非最新成交价，与 OKX 一致。
func unrealizedPnl(inst refdata.Instrument, signedPos, avgPx, markPx decimal.Decimal) decimal.Decimal {
	return realizedPnl(inst, signedPos, signedPos.Abs(), avgPx, markPx)
}

// initialMargin 返回按给定价格计算的初始保证金。
//
//	正向合约  Q × px / lever
//	反向合约  Q / (px × lever)
func initialMargin(inst refdata.Instrument, sz, px, lever decimal.Decimal) decimal.Decimal {
	if lever.IsZero() {
		return decimal.Zero
	}
	return div(notional(inst, sz, px), lever)
}

// liquidationPx 计算逐仓正向合约的预估强平价。
//
// 由「权益恰好等于维持保证金加平仓手续费」解出：
//
//	多头  (avgPx×Q − margin) / (Q × (1 − mmr − taker)) + tickSz
//	空头  (avgPx×Q + margin) / (Q × (1 + mmr + taker)) − tickSz
//
// 末尾那一个 tickSz 是 OKX 的安全缓冲，方向朝更早触发强平的一侧。它在任何文档里
// 都没有写，是在真实仓位上标定出来的：四个样本横跨 tickSz 为 0.01 / 0.1 / 1e-7
// 的三个量级、多空两个方向，偏移与 tickSz 的比值全部为 ±1（残差 1e-10 量级，
// 来自反推维持保证金率时的舍入）。
//
// 少了这一项，强平价会系统性地偏乐观一个 tick——数值虽小，却会在对拍时表现为
// 一个始终甩不掉的偏差。
func liquidationPx(inst refdata.Instrument, long bool,
	avgPx, qty, margin, mmrRate, taker decimal.Decimal) decimal.Decimal {

	if qty.IsZero() || !avgPx.IsPositive() {
		return decimal.Zero
	}
	one := decimal.NewFromInt(1)
	if inst.IsInverse() {
		return inverseLiquidationPx(inst, long, avgPx, qty, margin, mmrRate.Add(taker))
	}
	if long {
		den := qty.Mul(one.Sub(mmrRate).Sub(taker))
		if den.IsZero() {
			return decimal.Zero
		}
		return div(avgPx.Mul(qty).Sub(margin), den).Add(inst.TickSz)
	}
	den := qty.Mul(one.Add(mmrRate).Add(taker))
	if den.IsZero() {
		return decimal.Zero
	}
	return div(avgPx.Mul(qty).Add(margin), den).Sub(inst.TickSz)
}

// inverseLiquidationPx 计算反向合约的强平价。
//
// 币本位的名义价值是 Q/px（Q 以计价币计），随价格反比变化，因此解出来的形态与
// 正向合约是倒数对偶的。由「权益 = 维持保证金 + 平仓手续费」解得：
//
//	多  Q × (1 + r) / (margin + Q/avgPx) + tickSz
//	空  Q × (1 − r) / (Q/avgPx − margin) − tickSz     其中 r = mmr率 + taker
//
// 末尾那一个 tickSz 的安全缓冲**反向合约同样有**，方向也一致（朝更早触发的一侧）。
// 实测 BTC-USD-SWAP 多空各一个仓位，「偏移 ÷ tickSz」分别为 -0.9999999997 与
// +0.9999999995，补上后残差降到 5e-11。
//
// 空头在 Q/avgPx ≤ margin 时返回零：反向空头的最大亏损收敛于 Q/avgPx（价格涨到
// 无穷时名义价值趋近于零），保证金若已覆盖它，价格再怎么涨也爆不掉。这是币本位
// 特有的性质，正向合约的空头没有这个上界。
func inverseLiquidationPx(inst refdata.Instrument, long bool,
	avgPx, qty, margin, r decimal.Decimal) decimal.Decimal {

	one := decimal.NewFromInt(1)
	base := div(qty, avgPx)
	if long {
		den := margin.Add(base)
		if !den.IsPositive() {
			return decimal.Zero
		}
		return div(qty.Mul(one.Add(r)), den).Add(inst.TickSz)
	}
	den := base.Sub(margin)
	if !den.IsPositive() {
		return decimal.Zero
	}
	px := div(qty.Mul(one.Sub(r)), den).Sub(inst.TickSz)
	if !px.IsPositive() {
		return decimal.Zero
	}
	return px
}

// bankruptcyPx 计算逐仓正向合约的破产价，即权益恰好归零的价格。
//
//	多头  avgPx − margin/Q
//	空头  avgPx + margin/Q
//
// 它与强平价的区别在于不含维持保证金与手续费，因此比强平价更远。
// 两者之间的差额正是强平时留给风险准备金的缓冲。
func bankruptcyPx(inst refdata.Instrument, long bool, avgPx, qty, margin decimal.Decimal) decimal.Decimal {
	if qty.IsZero() {
		return decimal.Zero
	}
	if inst.IsInverse() {
		// 反向合约同为强平价公式在 r = 0 时的特例
		if !avgPx.IsPositive() {
			return decimal.Zero
		}
		base := div(qty, avgPx)
		if long {
			return div(qty, margin.Add(base))
		}
		den := base.Sub(margin)
		if !den.IsPositive() {
			return decimal.Zero
		}
		return div(qty, den)
	}
	d := div(margin, qty)
	if long {
		return avgPx.Sub(d)
	}
	return avgPx.Add(d)
}

// IsLiquidatable 报告该仓位是否已触及强平线。
//
// OKX 的口径是保证金率 ≤ 100%，而 mgnRatio 字段以倍数表示（1 即 100%）。
// 实测一个健康的五倍杠杆浮盈仓位其 mgnRatio 为 89.03，即 8903%。
//
// 判据里必须带上 HasPosition：保证金率为零既可能是「没有仓位」，也可能是
// 「权益已被耗尽」，后者恰恰最该强平。早先只用 MgnRatio 非零来排除空仓，
// 结果是被资金费耗穿的仓位反而爆不掉。
func (m Metrics) IsLiquidatable() bool {
	return m.HasPosition && m.MgnRatio.LessThanOrEqual(decimal.NewFromInt(1))
}

package okxsim

import (
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
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
	Equity   decimal.Decimal // 逐仓权益 = margin + upl
	MgnRatio decimal.Decimal // 保证金率，≤ 1 触发强平
	LiqPx    decimal.Decimal // 预估强平价（仅逐仓、仅正向合约）
	BkPx     decimal.Decimal // 破产价（仅逐仓、仅正向合约）
	Tier     int             // 所处档位
}

// ComputeMetrics 计算逐仓仓位在给定标记价下的风险指标。
//
// takerRate 传入费率表中的吃单费率，其符号沿用 OKX 约定（负数表示收取），
// 本函数内部取绝对值参与计算。
//
// 空仓返回零值。反向合约的强平价与破产价暂不计算（见 LiqPx 的说明）。
func ComputeMetrics(pos Position, inst refdata.Instrument, tier refdata.PositionTier,
	markPx, takerRate decimal.Decimal) Metrics {

	m := Metrics{MarkPx: markPx, MMRRate: tier.MMR, Tier: tier.Tier}
	if pos.IsEmpty() || markPx.IsZero() {
		return m
	}

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

	// 逐仓权益 = 划入的保证金 + 未实现盈亏
	m.Equity = pos.Margin.Add(m.UPL)

	// 保证金率 = 权益 / (维持保证金 + 平仓手续费)，≤ 1 触发强平。
	// 实测与 OKX 返回值差 2.2e-14。
	if den := m.MMR.Add(m.CloseFee); !den.IsZero() {
		m.MgnRatio = div(m.Equity, den)
	}

	if !inst.IsInverse() {
		m.LiqPx = liquidationPx(inst, long, pos.AvgPx, m.Qty, pos.Margin, tier.MMR, taker)
		m.BkPx = bankruptcyPx(long, pos.AvgPx, m.Qty, pos.Margin)
	}
	return m
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

	if qty.IsZero() {
		return decimal.Zero
	}
	one := decimal.NewFromInt(1)
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

// bankruptcyPx 计算逐仓正向合约的破产价，即权益恰好归零的价格。
//
//	多头  avgPx − margin/Q
//	空头  avgPx + margin/Q
//
// 它与强平价的区别在于不含维持保证金与手续费，因此比强平价更远。
// 两者之间的差额正是强平时留给风险准备金的缓冲。
func bankruptcyPx(long bool, avgPx, qty, margin decimal.Decimal) decimal.Decimal {
	if qty.IsZero() {
		return decimal.Zero
	}
	d := div(margin, qty)
	if long {
		return avgPx.Sub(d)
	}
	return avgPx.Add(d)
}

// IsLiquidatable 报告该保证金率是否已触及强平线。
//
// OKX 的口径是保证金率 ≤ 100%，而 mgnRatio 字段以倍数表示（1 即 100%）。
// 实测一个健康的五倍杠杆浮盈仓位其 mgnRatio 为 89.03，即 8903%。
func (m Metrics) IsLiquidatable() bool {
	return !m.MgnRatio.IsZero() && m.MgnRatio.LessThanOrEqual(decimal.NewFromInt(1))
}

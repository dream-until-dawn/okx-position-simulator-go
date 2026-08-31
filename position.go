// Package okxsim 实现 OKX 的仓位管理内核：把成交事件转换成仓位与账户状态。
//
// 撮合、行情、订单簿由使用者的回测引擎负责——它们缺的是这台「OKX 记账机」。
package okxsim

import (
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// Position 是一个仓位的状态。
//
// 只保存真正的状态；未实现盈亏、保证金率、强平价这类随行情变动的派生指标不在
// 其中，由 Metrics 按当前标记价现算。这样仓位永远不会「陈旧」——不存在某个字段
// 还停留在上一个行情快照上的可能。
//
// Pos 的符号沿用 OKX 的约定，两种持仓方式下含义不同：
//
//	买卖模式(net_mode)        Pos 带符号，正数为多头、负数为空头，PosSide 恒为 net
//	开平仓模式(long_short)    Pos 恒为正数，方向由 PosSide 给出
//
// 需要带符号的张数做计算时用 SignedPos，不要直接读 Pos。
type Position struct {
	InstID  string
	MgnMode types.MgnMode
	PosSide types.PosSide

	Pos    decimal.Decimal // 持仓张数，符号约定见上
	AvgPx  decimal.Decimal // 开仓均价
	Lever  decimal.Decimal // 杠杆
	Margin decimal.Decimal // 逐仓已划入的保证金；全仓恒为零

	// 以下为累计量，供对账与统计，不参与风险计算
	RealizedPnl decimal.Decimal // 累计已实现盈亏
	Fee         decimal.Decimal // 累计手续费，负数表示已收取
	Funding     decimal.Decimal // 累计资金费，负数表示已支付

	CTime int64 // 仓位建立时刻（毫秒）
	UTime int64 // 最后变动时刻（毫秒）
}

// SignedPos 返回带符号的持仓张数：多头为正、空头为负。
//
// 买卖模式下 Pos 本就带符号；开平仓模式下 Pos 恒为正，符号由 PosSide 决定。
func (p Position) SignedPos() decimal.Decimal {
	if p.PosSide == types.PosShort {
		return p.Pos.Neg()
	}
	return p.Pos
}

// IsEmpty 报告仓位是否为空仓。
func (p Position) IsEmpty() bool { return p.Pos.IsZero() }

// IsLong 报告是否为多头仓位；空仓返回 false。
func (p Position) IsLong() bool { return p.SignedPos().IsPositive() }

// IsShort 报告是否为空头仓位；空仓返回 false。
func (p Position) IsShort() bool { return p.SignedPos().IsNegative() }

// AbsPos 返回持仓张数的绝对值，用于查档与计算名义价值。
func (p Position) AbsPos() decimal.Decimal { return p.Pos.Abs() }

// Fill 是一笔成交。
//
// 内置撮合会自行构造它；引擎若有自己的撮合逻辑，也可以直接灌入。
type Fill struct {
	// OrdID 是该成交对应的委托 ID。填写后，模拟器会自动解除这笔委托的资金冻结；
	// 留空则不涉及任何挂单，适用于引擎完全自行管理委托的情形。
	OrdID string

	InstID   string
	TdMode   types.TdMode
	Side     types.Side
	PosSide  types.PosSide   // 买卖模式下填 net 或留空
	Sz       decimal.Decimal // 成交张数，正数
	Px       decimal.Decimal // 成交价
	ExecType types.ExecType  // 挂单方还是吃单方，决定按哪档费率计费
	Ts       int64           // 成交时刻（毫秒）
}

// FillResult 描述一笔成交对仓位的影响。
type FillResult struct {
	OpenedSz decimal.Decimal // 新开或加仓的张数
	ClosedSz decimal.Decimal // 平掉的张数
	Pnl      decimal.Decimal // 本次成交实现的盈亏
	Fee      decimal.Decimal // 本次手续费，负数表示收取
	Reversed bool            // 是否发生了反手：平光原仓位后反向开出新仓
	Before   Position        // 成交前的仓位
	After    Position        // 成交后的仓位
}

// signedDelta 返回一笔成交带来的带符号张数变化。
//
// 买卖模式下由 Side 决定；开平仓模式下由 PosSide 决定仓位方向，
// Side 只区分是开还是平：buy+long 与 sell+short 是开，sell+long 与 buy+short 是平。
func signedDelta(f Fill, mode types.PosMode) decimal.Decimal {
	sz := f.Sz.Abs()
	if mode == types.NetMode {
		if f.Side == types.Buy {
			return sz
		}
		return sz.Neg()
	}
	// 开平仓模式：结果的符号是「该仓位方向」上的增减
	if f.PosSide == types.PosShort {
		if f.Side == types.Sell { // 开空
			return sz.Neg()
		}
		return sz // 平空
	}
	if f.Side == types.Buy { // 开多
		return sz
	}
	return sz.Neg() // 平多
}

// applyFill 把一笔成交作用到仓位上，返回成交后的仓位与影响明细。
//
// 这是仓位核算的核心。三种情形：
//
//	同向或空仓   加仓，按张数加权平均更新开仓均价
//	反向未超量   减仓，结算已实现盈亏，开仓均价不变
//	反向且超量   先平光，再以成交价反向开出剩余部分，均价重置为成交价
//
// 第三种即「反手」，是买卖模式特有的边界，也是本核算最容易出错的地方。
// 开平仓模式下不会发生反手：平仓量超过持仓量属于非法委托，由上层拒绝。
//
// 部分平仓不改变开仓均价——只有加仓才做加权平均。这一点与 OKX 一致。
func applyFill(pos Position, f Fill, inst refdata.Instrument,
	feeRate decimal.Decimal, mode types.PosMode) FillResult {

	res := FillResult{Before: pos}

	delta := signedDelta(f, mode)
	cur := pos.SignedPos()
	next := pos

	// 手续费按成交名义价值计收，与开平方向无关
	res.Fee = notional(inst, f.Sz.Abs(), f.Px).Mul(feeRate)

	switch {
	case cur.IsZero() || sameSign(cur, delta):
		// 加仓：张数加权平均
		res.OpenedSz = delta.Abs()
		if cur.IsZero() {
			next.AvgPx = f.Px
			next.CTime = f.Ts
		} else {
			next.AvgPx = weightedAvg(pos.AvgPx, cur.Abs(), f.Px, delta.Abs())
		}
		next.Pos = signedToOKX(cur.Add(delta), pos.PosSide, mode)

	case delta.Abs().LessThanOrEqual(cur.Abs()):
		// 减仓：结算盈亏，均价不变
		res.ClosedSz = delta.Abs()
		res.Pnl = realizedPnl(inst, cur, delta.Abs(), pos.AvgPx, f.Px)
		remain := cur.Add(delta)
		next.Pos = signedToOKX(remain, pos.PosSide, mode)
		if remain.IsZero() {
			next.AvgPx = decimal.Zero
		}

	default:
		// 反手：先平光原仓位，剩余部分反向开出
		res.Reversed = true
		res.ClosedSz = cur.Abs()
		res.Pnl = realizedPnl(inst, cur, cur.Abs(), pos.AvgPx, f.Px)
		remain := cur.Add(delta) // 与原方向相反
		res.OpenedSz = remain.Abs()
		next.Pos = signedToOKX(remain, pos.PosSide, mode)
		next.AvgPx = f.Px // 新仓位以成交价为均价
		next.CTime = f.Ts
	}

	next.RealizedPnl = pos.RealizedPnl.Add(res.Pnl)
	next.Fee = pos.Fee.Add(res.Fee)
	next.UTime = f.Ts
	if next.Pos.IsZero() {
		next.AvgPx = decimal.Zero
	}

	res.After = next
	return res
}

// divPrecision 是除法保留的小数位数。
//
// shopspring/decimal 的 Div 默认只保留 16 位小数，而 OKX 返回的开仓均价实测可达
// 16 位（样本 1805.5767334669338683）。用默认精度会让均价从一开始就与 OKX 有
// 偏差，且该偏差会经由已实现盈亏一路传导到账户余额，是那种越滚越大、
// 最后无从追查的误差。取 20 位留出余量；确切位数待对拍时校准。
const divPrecision = 20

// div 以 divPrecision 做除法，避免落入 decimal 包的默认精度。
func div(a, b decimal.Decimal) decimal.Decimal {
	if b.IsZero() {
		return decimal.Zero
	}
	return a.DivRound(b, divPrecision)
}

// signedToOKX 把带符号张数转回 OKX 的存储约定。
func signedToOKX(signed decimal.Decimal, posSide types.PosSide, mode types.PosMode) decimal.Decimal {
	if mode == types.NetMode {
		return signed // 买卖模式带符号
	}
	return signed.Abs() // 开平仓模式恒为正，方向在 PosSide
}

// sameSign 报告两个非零数是否同号。
func sameSign(a, b decimal.Decimal) bool {
	return a.IsPositive() == b.IsPositive() && !a.IsZero() && !b.IsZero()
}

// weightedAvg 按张数对两段持仓的均价加权。
func weightedAvg(px1, sz1, px2, sz2 decimal.Decimal) decimal.Decimal {
	total := sz1.Add(sz2)
	if total.IsZero() {
		return decimal.Zero
	}
	return div(px1.Mul(sz1).Add(px2.Mul(sz2)), total)
}

// notional 返回 sz 张合约在价格 px 处的名义价值，单位为结算币。
//
//	正向合约  Q * px      Q 以标的币计，乘价得计价币金额
//	反向合约  Q / px      Q 以计价币计，除价得标的币金额
func notional(inst refdata.Instrument, sz, px decimal.Decimal) decimal.Decimal {
	q := inst.ContractQty(sz)
	if inst.IsInverse() {
		return div(q, px)
	}
	return q.Mul(px)
}

// realizedPnl 计算平掉 closeSz 张仓位实现的盈亏。
//
// cur 为平仓前的带符号持仓，其符号决定是平多还是平空。
//
//	正向多头  Q * (closePx - avgPx)
//	正向空头  Q * (avgPx - closePx)
//	反向多头  Q * (1/avgPx - 1/closePx)
//	反向空头  Q * (1/closePx - 1/avgPx)
//
// 正向合约的公式已用真实成交验证：一笔 ETH-USDT-SWAP 逐仓多头平仓，
// 算出的盈亏与 OKX 返回值差 1.6e-15。
func realizedPnl(inst refdata.Instrument, cur, closeSz, avgPx, closePx decimal.Decimal) decimal.Decimal {
	q := inst.ContractQty(closeSz)
	long := cur.IsPositive()

	if inst.IsInverse() {
		if avgPx.IsZero() || closePx.IsZero() {
			return decimal.Zero
		}
		one := decimal.NewFromInt(1)
		diff := div(one, avgPx).Sub(div(one, closePx))
		if !long {
			diff = diff.Neg()
		}
		return q.Mul(diff)
	}

	diff := closePx.Sub(avgPx)
	if !long {
		diff = diff.Neg()
	}
	return q.Mul(diff)
}

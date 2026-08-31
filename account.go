package okxsim

import (
	"github.com/shopspring/decimal"
)

// Balance 是一个币种的余额快照，字段与 OKX /api/v5/account/balance 的 details 对应。
//
// 只有 CashBal 是账户的真实状态，其余各项都由它与当前持仓现算。这个划分不是
// 美学取舍——逐仓权益随标记价每时每刻都在变，把它存成状态就必然会有陈旧的一刻。
//
// 各项关系经真实账户标定（开 4 张 -> 平 1 张 -> 平 3 张，每步比对，差值均为 0）：
//
//	IsoEq     = Σ(仓位保证金 + 未实现盈亏)
//	FrozenBal = IsoEq
//	Eq        = CashBal + IsoEq
//	AvailBal  = CashBal − OrdFrozen
type Balance struct {
	Ccy       string
	CashBal   decimal.Decimal // 现金余额，账户的真实状态
	IsoEq     decimal.Decimal // 逐仓权益
	Upl       decimal.Decimal // 未实现盈亏合计
	OrdFrozen decimal.Decimal // 挂单占用
	FrozenBal decimal.Decimal // 冻结余额
	AvailBal  decimal.Decimal // 可用余额
	AvailEq   decimal.Decimal // 可用权益
	Eq        decimal.Decimal // 币种权益
}

// marginDelta 描述一笔成交引起的逐仓保证金变化。
type marginDelta struct {
	Add     decimal.Decimal // 开仓部分需划入的保证金
	Release decimal.Decimal // 平仓部分释放的保证金
}

// Net 返回保证金的净流出，正数表示从现金余额中划走。
func (d marginDelta) Net() decimal.Decimal { return d.Add.Sub(d.Release) }

// computeMarginDelta 计算一笔成交的逐仓保证金变化。
//
// 开仓部分按成交价与该仓位的杠杆计算：notional(成交价) / lever。
// 平仓部分按张数比例释放：margin × closedSz / 平仓前持仓量。
//
// 比例释放这一条经真实账户标定：4 张仓位保证金 624.705284，平掉 1 张后释放
// 156.176321，恰为四分之一，差值为 0。
//
// 反手时两部分同时发生，且必须先按【成交前】的保证金算释放、再算追加，
// 否则释放的比例会算在一个已经被改动过的基数上。
func computeMarginDelta(res FillResult, openNotional, lever decimal.Decimal) marginDelta {
	var d marginDelta

	if res.ClosedSz.IsPositive() {
		beforeAbs := res.Before.AbsPos()
		if !beforeAbs.IsZero() {
			d.Release = res.Before.Margin.Mul(div(res.ClosedSz, beforeAbs))
		}
	}
	if res.OpenedSz.IsPositive() && !lever.IsZero() {
		d.Add = div(openNotional, lever)
	}
	return d
}

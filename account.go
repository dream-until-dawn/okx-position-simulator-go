package okxsim

import (
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// Balance 是一个币种的余额快照，字段与 OKX /api/v5/account/balance 的 details 对应。
//
// 只有 CashBal 是账户的真实状态，其余各项都由它与当前持仓现算。这个划分不是
// 美学取舍——权益随标记价每时每刻都在变，把它存成状态就必然会有陈旧的一刻。
//
// 逐仓与全仓在这里合流，两者的记账方式截然不同：
//
//	逐仓  保证金从现金余额划走，进入仓位；亏损先吃仓位保证金，与现金隔离
//	全仓  保证金只是被占用，不离开现金余额；亏损直接吃现金，同币种的全仓仓位共担
//
// 各项关系经真实账户标定。逐仓部分为开 4 张 -> 平 1 张 -> 平 3 张逐步比对；
// 全仓部分为 10 个快照、横跨两个合约、含多仓位与挂单。两边差值均在 1e-12 以内：
//
//	IsoEq     = Σ逐仓(仓位保证金 + 未实现盈亏)
//	Eq        = CashBal + IsoEq + CrossUpl
//	FrozenBal = IsoEq + IMR + 逐仓挂单保证金 + 挂单手续费
//	AvailBal  = CashBal + CrossUpl − IMR − 逐仓挂单保证金 − 挂单手续费
//	MgnRatio  = (CashBal + CrossUpl) / (MMR + Σ全仓仓位平仓手续费)
//
// 只有逐仓持仓时 CrossUpl、IMR、MMR 均为零，上式退化为标定逐仓时所得的形态。
type Balance struct {
	Ccy       string
	CashBal   decimal.Decimal // 现金余额，账户的真实状态
	IsoEq     decimal.Decimal // 逐仓权益
	Upl       decimal.Decimal // 未实现盈亏合计（逐仓 + 全仓）
	IsoUpl    decimal.Decimal // 逐仓仓位的未实现盈亏
	CrossUpl  decimal.Decimal // 全仓仓位的未实现盈亏
	IMR       decimal.Decimal // 全仓初始保证金占用，含全仓挂单
	MMR       decimal.Decimal // 全仓维持保证金占用，含全仓挂单
	MgnRatio  decimal.Decimal // 全仓保证金率，≤ 1 触发全仓强平；无全仓持仓时为零
	OrdFrozen decimal.Decimal // 挂单占用的保证金（逐仓与全仓合计）
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

// computeMarginDelta 计算一笔成交引起的保证金占用变化。
//
// 开仓部分按成交价与该仓位的杠杆计算：notional(成交价) / lever。
// 平仓部分按张数比例释放：占用 × closedSz / 平仓前持仓量。
//
// 比例释放这一条经真实账户标定：4 张仓位保证金 624.705284，平掉 1 张后释放
// 156.176321，恰为四分之一，差值为 0。
//
// 反手时两部分同时发生，且必须先按【成交前】的保证金算释放、再算追加，
// 否则释放的比例会算在一个已经被改动过的基数上。
//
// 全仓的保证金不离开现金余额，仓位上的 Margin 恒为零，因此比例释放的基数改用
// 【按开仓均价折算的初始保证金】。两种模式在此保持同构不是为了省代码——可动用
// 资金的减少量在两种模式下本就相同，差别只在这笔钱落到哪里：逐仓划进仓位，
// 全仓留在现金里但被 IMR 占住。
func computeMarginDelta(res FillResult, inst refdata.Instrument,
	openNotional, lever decimal.Decimal, mgnMode types.MgnMode) marginDelta {

	var d marginDelta

	if res.ClosedSz.IsPositive() {
		beforeAbs := res.Before.AbsPos()
		if !beforeAbs.IsZero() {
			base := res.Before.Margin
			if mgnMode == types.MgnCross {
				base = initialMargin(inst, beforeAbs, res.Before.AvgPx, lever)
			}
			// 先乘后除，不是先除后乘。两点原因，都不是风格问题：
			//
			// 一  div 固定给出 -20 的指数，而 Mul 把两边指数【相加】。写成
			//     base.Mul(div(...)) 的话，Margin 每经历一次部分平仓就多 20 位
			//     小数，无界增长——12 轮之后系数已是 264 位的大整数，且经
			//     Sub/Add 永久污染现金余额。网格做的全是部分平仓，正中靶心：
			//     实测挂单数 16/80/160 时，5 倍挂单换来 22.8 倍耗时。
			// 二  先乘后除只舍入一次。先除后乘是先把比例舍到 20 位、再乘上
			//     一个可能上千的 base，把舍入误差一并放大。
			//
			// weightedAvg 一直是这个写法，这里只是补上同样的做法。
			d.Release = div(base.Mul(res.ClosedSz), beforeAbs)
		}
	}
	if res.OpenedSz.IsPositive() && !lever.IsZero() {
		d.Add = div(openNotional, lever)
	}
	return d
}

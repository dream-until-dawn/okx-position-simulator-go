package okxsim

import (
	"errors"
	"fmt"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/shopspring/decimal"
)

// 本文件让模拟器把「为什么」讲清楚。
//
// 回测引擎最难排查的不是「结果不对」，而是「什么都没发生却不知道为什么」——
// 单没挂上、仓没开成、委托凭空消失。只给一个委托 ID 或一个布尔值，
// 使用者只能去猜。凡是模拟器拒绝或撤销了什么，都应当同时给出可判定的原因。

// CancelReason 说明一笔委托为何被撤销。
//
// **这些取值应当被穷尽地处理。** 漏掉一种在 Go 里没有任何编译期或运行期提示——
// 接口只管方法签名，不管你有没有处理某个枚举值。一个下游正是这么漏掉
// CancelLiquidation 的：策略在被清算之后的空簿上按旧状态继续跑，全程不报错。
//
// 把这条纪律换成工具：`golangci-lint` 的 `exhaustive` 检查器能在 switch 漏掉
// 某个取值时报错。对「撤单原因」这类枚举，那比靠人记得可靠。
//
// **别用 `r != CancelUser` 去判「要不要重新同步状态」**——这些取值里有三种
// 不是使用者撤的、却只影响那一笔委托（只挂单会成交、立即成交类没成交、
// 触发时资金不足）。按那个判据，一个网格会在第一次挂单被拒时就停机，
// 而那是必然发生的事、不是异常。用 AffectsAccountState 代替。
type CancelReason string

const (
	// CancelUser 使用者主动撤单。
	CancelUser CancelReason = "user"
	// CancelPostOnlyWouldTake 只挂单委托在下单时即可成交，OKX 直接撤销而非成交。
	CancelPostOnlyWouldTake CancelReason = "post_only_would_take"
	// CancelImmediateUnfilled 立即成交类委托（market/ioc/fok）无法成交，不挂入簿中。
	CancelImmediateUnfilled CancelReason = "immediate_unfilled"
	// CancelInsufficientFunds 触发时资金不足以承接该成交。
	CancelInsufficientFunds CancelReason = "insufficient_funds"
	// CancelLiquidation 强平前撤单，以释放挂单占用的保证金。
	CancelLiquidation CancelReason = "liquidation"
)

// AffectsAccountState 报告这次撤单是否意味着**账户状态可能已经失效**。
//
// 假为「只影响那一笔委托」，真为「账户级事件，你手上的持仓与挂单快照可能都过期了」。
// 典型用法：
//
//	for _, c := range step.Canceled {
//		if c.Reason.AffectsAccountState() {
//			// 重新读一遍 Positions / PendingOrders，别在旧状态上继续
//		}
//	}
//
// **实现是白名单：列「无害」而不是列「有害」。** 这个方向是刻意的——新增的取值
// 总是落进未列举的那一侧，而我们要它落进「当作账户级」那一侧。理由是代价不对称：
//
//	把账户级事件当成无害的  策略继续在不存在的前提上跑，且【不报错】
//	把无害的当成账户级的    多做一次重新同步 —— 有代价，但看得见
//
// 这条分类由本库提供而不是让调用方自己列，是因为**本库知道哪些原因是账户级的，
// 调用方只能猜**；而且日后本库新增原因时，调用方不改代码也不会漏。
//
// 这个方向是被一个真实缺陷教出来的：本库曾建议「判 r != CancelUser 即可」，
// 而那会让网格在第一次只挂单被拒时就停机——那是必然发生的事。见
// docs/silent-risks.md。
func (r CancelReason) AffectsAccountState() bool {
	switch r {
	case CancelUser, CancelPostOnlyWouldTake, CancelImmediateUnfilled,
		CancelInsufficientFunds:
		// 都只影响这一笔委托：使用者自己撤的、只挂单会当场成交被拒、
		// 立即成交类没成交不入簿、触发时资金不足以承接这一笔。
		return false
	}
	// CancelLiquidation 以及**日后新增的任何取值**都落在这里。强平会撤光该合约
	// （全仓则是该结算币种）下的全部挂单并拿走仓位——策略手上的一切都过期了。
	return true
}

func (r CancelReason) String() string { return string(r) }

// Describe 返回该原因的中文说明。
func (r CancelReason) Describe() string {
	switch r {
	case CancelUser:
		return "使用者主动撤单"
	case CancelPostOnlyWouldTake:
		return "只挂单委托会立即成交，故被撤销而非成交"
	case CancelImmediateUnfilled:
		return "立即成交类委托无法成交，不挂入簿中"
	case CancelInsufficientFunds:
		return "资金不足以承接该成交"
	case CancelLiquidation:
		return "强平前撤单，以释放挂单占用的保证金"
	}
	return string(r)
}

// Cancellation 是一次撤单及其原因。
type Cancellation struct {
	OrdID  string
	InstID string
	Reason CancelReason
	Detail string // 补充说明，如缺口金额
}

func (c Cancellation) String() string {
	s := fmt.Sprintf("委托 %s（%s）被撤销：%s", c.OrdID, c.InstID, c.Reason.Describe())
	if c.Detail != "" {
		s += "——" + c.Detail
	}
	return s
}

// Shortfall 描述一次资金缺口。
//
// 只说「余额不足」，使用者还得自己去算差多少、能开多少。把三个数一并给出，
// 引擎就能直接决定是减小委托量还是放弃这一笔。
type Shortfall struct {
	Ccy       string
	Available decimal.Decimal // 可用余额
	Required  decimal.Decimal // 本次所需
	Missing   decimal.Decimal // 缺口 = 所需 − 可用
}

func (s Shortfall) String() string {
	return fmt.Sprintf("%s 可用 %s，需要 %s，缺口 %s",
		s.Ccy, s.Available, s.Required, s.Missing)
}

// ShortfallError 是携带资金缺口详情的错误，错误码与 OKX 的 51008 一致。
//
// 不用嵌入 *okxerr.Error：那个类型名恰好是 Error，嵌入后字段名会把提升上来的
// Error() 方法挡住，反而不满足 error 接口。
type ShortfallError struct {
	err       *okxerr.Error
	Shortfall Shortfall
}

func (e *ShortfallError) Error() string { return e.err.Error() }

// Unwrap 使 errors.Is 与 okxerr.HasCode 能沿链找到那个带错误码的错误。
func (e *ShortfallError) Unwrap() error { return e.err }

// Code 返回 OKX 错误码。
func (e *ShortfallError) Code() string { return e.err.Code }

func newShortfallError(ccy string, avail, required decimal.Decimal, what string) *ShortfallError {
	sf := Shortfall{
		Ccy: ccy, Available: avail, Required: required,
		Missing: required.Sub(avail),
	}
	return &ShortfallError{
		err:       okxerr.New(okxerr.CodeInsufficientBal, "%s：%s", what, sf),
		Shortfall: sf,
	}
}

// ShortfallOf 从错误链中取出资金缺口详情；没有则第二个返回值为 false。
//
// 供引擎在余额不足时直接读出缺口，而不必去解析错误文本。
func ShortfallOf(err error) (Shortfall, bool) {
	var se *ShortfallError
	if errors.As(err, &se) {
		return se.Shortfall, true
	}
	return Shortfall{}, false
}

// Shortfall 返回挂出该委托还差多少资金；挂得起则各项为零。
func (c OrderCost) Shortfall(availBal decimal.Decimal) Shortfall {
	sf := Shortfall{Ccy: c.Ccy, Available: availBal, Required: c.Frozen}
	if missing := c.Frozen.Sub(availBal); missing.IsPositive() {
		sf.Missing = missing
	}
	return sf
}

// String 便于把强平事件直接写进日志。
func (l Liquidation) String() string {
	var kind string
	switch l.Kind {
	case LiqPartial:
		kind = fmt.Sprintf("阶梯减仓（档位 %d → %d）", l.TierBefore, l.TierAfter)
	case LiqFull:
		kind = "全部强平"
	default:
		kind = string(l.Kind)
	}
	s := fmt.Sprintf("%s %s %s：平掉 %s 张 @ %s，损失保证金 %s",
		l.InstID, l.PosSide, kind, l.Sz, l.Px, l.Loss)
	if !l.Penalty.IsZero() {
		s += fmt.Sprintf("，爆仓罚金 %s", l.Penalty)
	}
	if l.IsBankrupt() {
		s += fmt.Sprintf("，超出保证金 %s（由风险准备金承担）", l.Excess)
	}
	if len(l.CanceledOrders) > 0 {
		s += fmt.Sprintf("，同时撤销 %d 笔挂单", len(l.CanceledOrders))
	}
	return s
}

// Describe 返回本步发生的全部事件的中文摘要，供日志或调试时一览。
//
// 一步之内可能同时发生资金费结算、成交、撤单与强平，散落在各个字段里不便查看。
func (r StepResult) Describe() string {
	if len(r.Fundings) == 0 && len(r.Fills) == 0 && len(r.Liquidations) == 0 &&
		len(r.Canceled) == 0 && len(r.AlgoTriggers) == 0 {
		return fmt.Sprintf("第 %d 步：无事发生", r.Ts)
	}
	s := fmt.Sprintf("第 %d 步：", r.Ts)
	if len(r.Fundings) > 0 {
		s += fmt.Sprintf("\n  结算 %d 笔资金费", len(r.Fundings))
		for _, f := range r.Fundings {
			s += fmt.Sprintf("\n    %s %s 费率 %s，金额 %s",
				f.InstID, f.PosSide, f.Rate, f.Amount)
		}
	}
	// 算法单的触发排在成交之前列出，与它在 Advance 里的位置一致——
	// 先有触发，才有那笔成交。
	for _, a := range r.AlgoTriggers {
		s += "\n  " + a.String()
	}
	if len(r.Fills) > 0 {
		s += fmt.Sprintf("\n  成交 %d 笔", len(r.Fills))
		for _, f := range r.Fills {
			s += fmt.Sprintf("\n    %s 开 %s 张 / 平 %s 张，盈亏 %s，手续费 %s",
				f.After.InstID, f.OpenedSz, f.ClosedSz, f.Pnl, f.Fee)
		}
	}
	for _, l := range r.Liquidations {
		s += "\n  " + l.String()
	}
	for _, c := range r.Canceled {
		s += "\n  " + c.String()
	}
	return s
}

// legName 把内部的腿标识译成中文。
func legName(leg string) string {
	switch leg {
	case "trigger":
		return "计划委托"
	case "tp":
		return "止盈"
	case "sl":
		return "止损"
	case "move":
		return "移动止损"
	}
	return leg
}

// String 描述一次算法委托的触发。
//
// 触发本身与触发之后下单成不成功是两件事：价格确实到了，但资金可能不够。
// 两者混为一谈会让使用者以为「没触发」，实际是触发了却下不出单。
func (t AlgoTrigger) String() string {
	if t.Leg == "" && t.Reason != "" {
		return fmt.Sprintf("算法委托 %s（%s）本步无从判断：%s", t.AlgoID, t.InstID, t.Reason)
	}
	head := fmt.Sprintf("算法委托 %s（%s %s）%s 于 %s 触发",
		t.AlgoID, t.InstID, t.OrdType, legName(t.Leg), t.Px)
	if t.Reason != "" {
		return head + "，但下单失败：" + t.Reason
	}
	if t.Fill != nil {
		return fmt.Sprintf("%s，当场成交（开 %s 张 / 平 %s 张，盈亏 %s）",
			head, t.Fill.OpenedSz, t.Fill.ClosedSz, t.Fill.Pnl)
	}
	return head + "，已转为限价委托 " + t.OrdID
}

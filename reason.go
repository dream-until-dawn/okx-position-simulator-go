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
	if len(r.Fundings) == 0 && len(r.Fills) == 0 &&
		len(r.Liquidations) == 0 && len(r.Canceled) == 0 {
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

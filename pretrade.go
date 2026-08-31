package okxsim

import (
	"sort"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// 本文件提供预下单计算：挂一笔单要冻结多少钱、最多能开多少张、成交后仓位会变成
// 什么样。回测引擎每根 K 线都要问这些，而它们无法从成交后的状态倒推。

// OrderReq 描述一笔待挂出的委托。
type OrderReq struct {
	InstID  string
	TdMode  types.TdMode
	Side    types.Side
	PosSide types.PosSide
	Px      decimal.Decimal // 委托价
	Sz      decimal.Decimal // 委托张数
}

// OrderCost 是挂出一笔委托需要冻结的资金。
//
// 各项经模拟盘标定，差值均为 0：
//
//	开仓方向的挂单  冻结 = 张数 × (每张保证金 + 每张手续费)
//	平仓方向的挂单  不冻结任何资金
//
// 手续费一律按 taker 费率预冻结，即便该委托大概率会作为 maker 成交——
// OKX 取保守值，实测一笔远离市价、必然挂住的限价单也是按 taker 冻结的。
type OrderCost struct {
	Ccy      string          // 冻结所用的币种
	Notional decimal.Decimal // 开仓部分的名义价值
	Margin   decimal.Decimal // 冻结的保证金，对应 OKX 的 ordFrozen
	Fee      decimal.Decimal // 预冻结的手续费，正数
	Frozen   decimal.Decimal // 合计冻结 = Margin + Fee，可用余额将减少此数
	OpenSz   decimal.Decimal // 该委托中属于开仓的张数
	CloseSz  decimal.Decimal // 属于平仓的张数，这部分不冻结
	Lever    decimal.Decimal // 计算所用的杠杆
}

// Affordable 报告当前可用余额是否够挂出这笔委托。
func (c OrderCost) Affordable(availBal decimal.Decimal) bool {
	return availBal.GreaterThanOrEqual(c.Frozen)
}

// OrderCost 计算挂出一笔委托需要冻结多少资金。
//
// 它不改变任何状态，也不要求真的挂单——回测引擎可以先问再决定。
// 委托中与现有持仓反向的部分属于平仓，不产生冻结。
func (s *Simulator) OrderCost(req OrderReq) (OrderCost, error) {
	inst, err := s.cfg.RefData.Instrument(req.InstID)
	if err != nil {
		return OrderCost{}, err
	}
	if err := inst.ValidateSize(req.Sz); err != nil {
		return OrderCost{}, err
	}
	if !req.Px.IsPositive() {
		return OrderCost{}, okxerr.New(okxerr.CodeParamError,
			"px: 委托价须为正数，实为 %s", req.Px)
	}
	if !req.Side.Valid() {
		return OrderCost{}, okxerr.New(okxerr.CodeParamError, "side: 非法方向 %q", req.Side)
	}
	mgnMode, ok := req.TdMode.MgnMode()
	if !ok {
		return OrderCost{}, okxerr.New(okxerr.CodeParamError,
			"tdMode: 衍生品只支持 isolated 与 cross，实为 %q", req.TdMode)
	}
	side, err := s.normalizePosSide(req.PosSide)
	if err != nil {
		return OrderCost{}, err
	}

	rate, err := s.fees.Rate(inst)
	if err != nil {
		return OrderCost{}, err
	}

	c := OrderCost{Ccy: inst.SettleCcy, Lever: s.Leverage(req.InstID, mgnMode, side)}
	if pos, ok := s.pos[positionKey{req.InstID, side}]; ok {
		c.Lever = pos.Lever
	}

	c.OpenSz, c.CloseSz = s.splitOrderSize(req, side)
	if c.OpenSz.IsZero() {
		return c, nil // 纯平仓委托不冻结任何资金
	}

	c.Notional = notional(inst, c.OpenSz, req.Px)
	c.Margin = div(c.Notional, c.Lever)
	// 预冻结一律按 taker 费率，与 OKX 一致
	c.Fee = c.Notional.Mul(rate.Taker.Abs())
	c.Frozen = c.Margin.Add(c.Fee)
	return c, nil
}

// splitOrderSize 把委托张数拆成开仓与平仓两部分。
//
// 与现有持仓反向的部分属于平仓，最多平掉当前持仓量；超出的部分是反向开仓。
func (s *Simulator) splitOrderSize(req OrderReq, side types.PosSide) (open, close decimal.Decimal) {
	sz := req.Sz.Abs()
	pos, ok := s.pos[positionKey{req.InstID, side}]
	if !ok || pos.IsEmpty() {
		return sz, decimal.Zero
	}

	delta := signedDelta(Fill{Side: req.Side, PosSide: side, Sz: sz}, s.cfg.PosMode)
	cur := pos.SignedPos()
	if cur.IsZero() || sameSign(cur, delta) {
		return sz, decimal.Zero // 同向加仓
	}
	if delta.Abs().LessThanOrEqual(cur.Abs()) {
		return decimal.Zero, sz // 纯减仓
	}
	// 反手：先平光，剩余部分反向开仓
	return delta.Abs().Sub(cur.Abs()), cur.Abs()
}

// MaxSize 是给定价格下最多能开的张数。
type MaxSize struct {
	MaxBuy  decimal.Decimal
	MaxSell decimal.Decimal
}

// MaxSize 返回在给定委托价下最多能买入或卖出多少张，对应 OKX 的
// GET /api/v5/account/max-size。
//
// 取价规则经实测标定，七组价格全部命中：
//
//	买入  按委托价计算，无论该价格高于还是低于标记价
//	卖出  在委托价与标记价之间取更保守的一侧，即张数更少的那个
//
// 这个不对称是 OKX 自身的行为而非某条可推导的原理：委托价高于标记价的买单
// 本会立即以标记价附近成交、所需保证金更少，OKX 仍按委托价计算。
// 此处照实编码，不去脑补一个统一的解释。
//
// 结果已按 lotSz 向下取整。未设置标记价时以委托价代替。
func (s *Simulator) MaxSize(instID string, tdMode types.TdMode, px decimal.Decimal) (MaxSize, error) {
	inst, err := s.cfg.RefData.Instrument(instID)
	if err != nil {
		return MaxSize{}, err
	}
	if !px.IsPositive() {
		return MaxSize{}, okxerr.New(okxerr.CodeParamError, "px: 委托价须为正数，实为 %s", px)
	}
	mgnMode, ok := tdMode.MgnMode()
	if !ok {
		return MaxSize{}, okxerr.New(okxerr.CodeParamError,
			"tdMode: 衍生品只支持 isolated 与 cross，实为 %q", tdMode)
	}
	rate, err := s.fees.Rate(inst)
	if err != nil {
		return MaxSize{}, err
	}
	bal, err := s.Balance(inst.SettleCcy)
	if err != nil {
		return MaxSize{}, err
	}

	lever := s.Leverage(instID, mgnMode, types.PosNet)
	if s.cfg.PosMode == types.LongShortMode {
		lever = s.Leverage(instID, mgnMode, types.PosLong)
	}

	byPx := maxSizeAt(inst, bal.AvailBal, px, lever, rate.Taker.Abs())
	m := MaxSize{MaxBuy: byPx, MaxSell: byPx}

	markPx := s.marks[instID]
	if markPx.IsPositive() && !markPx.Equal(px) {
		if byMark := maxSizeAt(inst, bal.AvailBal, markPx, lever, rate.Taker.Abs()); byMark.LessThan(byPx) {
			m.MaxSell = byMark
		}
	}
	return m, nil
}

// maxSizeAt 计算在给定价格下可用余额最多支撑多少张，已按 lotSz 向下取整。
func maxSizeAt(inst refdata.Instrument, avail, px, lever, takerRate decimal.Decimal) decimal.Decimal {
	if !avail.IsPositive() || !lever.IsPositive() {
		return decimal.Zero
	}
	// 每张所需 = 每张保证金 + 每张手续费
	one := inst.ContractQty(decimal.NewFromInt(1))
	perNotional := one.Mul(px)
	if inst.IsInverse() {
		perNotional = div(one, px)
	}
	per := div(perNotional, lever).Add(perNotional.Mul(takerRate))
	if !per.IsPositive() {
		return decimal.Zero
	}
	return refdata.FloorToStep(div(avail, per), inst.LotSz)
}

// PreviewFill 预演一笔成交，返回它会造成的影响，但不改变任何状态。
//
// 供回测引擎在下单前查看「成交后仓位会变成什么样」——均价怎么变、会不会反手、
// 实现多少盈亏。与 Fill 走同一套核算，因此预演结果与真实成交必然一致。
func (s *Simulator) PreviewFill(f Fill) (FillResult, error) {
	inst, err := s.cfg.RefData.Instrument(f.InstID)
	if err != nil {
		return FillResult{}, err
	}
	if err := inst.ValidateSize(f.Sz); err != nil {
		return FillResult{}, err
	}
	if !f.Px.IsPositive() {
		return FillResult{}, okxerr.New(okxerr.CodeParamError,
			"px: 成交价须为正数，实为 %s", f.Px)
	}
	mgnMode, ok := f.TdMode.MgnMode()
	if !ok {
		return FillResult{}, okxerr.New(okxerr.CodeParamError,
			"tdMode: 衍生品只支持 isolated 与 cross，实为 %q", f.TdMode)
	}
	side, err := s.normalizePosSide(f.PosSide)
	if err != nil {
		return FillResult{}, err
	}
	f.PosSide = side

	rate, err := s.fees.Rate(inst)
	if err != nil {
		return FillResult{}, err
	}
	if f.ExecType == "" {
		f.ExecType = types.Taker
	}

	pos, exists := s.pos[positionKey{f.InstID, side}]
	if !exists {
		pos = Position{
			InstID: f.InstID, MgnMode: mgnMode, PosSide: side,
			Lever: s.Leverage(f.InstID, mgnMode, side),
		}
	}
	res := applyFill(pos, f, inst, rate.Of(f.ExecType), s.cfg.PosMode)

	// 补上保证金变化，使预演的仓位与真实成交后一致
	md := computeMarginDelta(res, notional(inst, res.OpenedSz, f.Px), pos.Lever)
	after := res.After
	after.Margin = pos.Margin.Sub(md.Release).Add(md.Add)
	if after.IsEmpty() {
		after.Margin = decimal.Zero
	}
	res.After = after
	return res, nil
}

// PendingOrder 是一笔已挂出、尚未成交的委托及其冻结明细。
type PendingOrder struct {
	OrdID string
	Req   OrderReq
	Cost  OrderCost
}

// PlaceOrder 登记一笔挂单并冻结相应资金，可用余额随之减少。
//
// 模拟器不做撮合——何时成交由使用者的回测引擎判定，成交时调用 Fill 并用
// CancelOrder 解除冻结。本方法的意义在于让 Balance 与 MaxSize 在有未成交委托时
// 仍然正确：OKX 的可开张数会扣掉已冻结的部分，实测挂单后 maxBuy 从 33.76 降到 29.76。
//
// 可用余额不足时返回错误码 51008，与 OKX 一致。
func (s *Simulator) PlaceOrder(ordID string, req OrderReq) (OrderCost, error) {
	if ordID == "" {
		return OrderCost{}, okxerr.New(okxerr.CodeParamEmpty, "ordId 不能为空")
	}
	if _, dup := s.pending[ordID]; dup {
		return OrderCost{}, okxerr.New(okxerr.CodeParamError, "ordId %q 已存在", ordID)
	}
	cost, err := s.OrderCost(req)
	if err != nil {
		return OrderCost{}, err
	}
	bal, err := s.Balance(cost.Ccy)
	if err != nil {
		return OrderCost{}, err
	}
	if !cost.Affordable(bal.AvailBal) {
		return OrderCost{}, okxerr.New(okxerr.CodeInsufficientBal,
			"%s 可用余额 %s 不足以挂出该委托（需冻结 %s）", cost.Ccy, bal.AvailBal, cost.Frozen)
	}
	s.pending[ordID] = PendingOrder{OrdID: ordID, Req: req, Cost: cost}
	return cost, nil
}

// CancelOrder 撤销挂单并释放其冻结。委托成交后同样应当调用它来解除冻结。
func (s *Simulator) CancelOrder(ordID string) error {
	if _, ok := s.pending[ordID]; !ok {
		return okxerr.New(okxerr.CodeParamError, "找不到挂单 %q", ordID)
	}
	delete(s.pending, ordID)
	return nil
}

// PendingOrders 返回全部挂单，按委托 ID 排序。
func (s *Simulator) PendingOrders() []PendingOrder {
	out := make([]PendingOrder, 0, len(s.pending))
	for _, o := range s.pending {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrdID < out[j].OrdID })
	return out
}

// orderFreeze 汇总某币种上全部挂单的冻结，分别返回保证金部分与手续费部分。
//
// OKX 把两者分开呈现：ordFrozen 只是保证金，手续费不在其中，
// 但可用余额是按两者之和扣减的。
func (s *Simulator) orderFreeze(ccy string) (margin, fee decimal.Decimal) {
	for _, o := range s.pending {
		if o.Cost.Ccy != ccy {
			continue
		}
		margin = margin.Add(o.Cost.Margin)
		fee = fee.Add(o.Cost.Fee)
	}
	return margin, fee
}

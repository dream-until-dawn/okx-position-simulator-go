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

	// 与 PlaceOrder 一致地取整。若此处按原价报冻结额、下单时却按取整价扣，
	// 引擎拿到的报价就与实际不符——差额虽小，但会让「按报价刚好挂得起」的
	// 委托在真下单时被拒。
	req.Px = inst.RoundPrice(req.Px, req.Side)

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
// 取价规则经实测标定，两类合约各五至七组价格全部命中：
//
//	保守的一侧  在委托价与标记价之间取张数更少的那个
//	另一侧      直接按委托价算，无论它高于还是低于标记价
//
// 哪一侧保守由合约类型决定，判据见 conservativeSide——正向是卖出，反向是买入。
// 这不是对称的美学取舍，而是「谁的保证金随不利行情变大」的结果。
//
// 全仓还要多扣一项开仓即产生的盯市浮亏，见 crossOpenLoss。
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
	bal, err := s.BalanceOf(inst.SettleCcy)
	if err != nil {
		return MaxSize{}, err
	}

	lever := s.Leverage(instID, mgnMode, types.PosNet)
	if s.cfg.PosMode == types.LongShortMode {
		lever = s.Leverage(instID, mgnMode, types.PosLong)
	}

	taker := rate.Taker.Abs()
	markPx := s.marks[instID]

	var lossBuy, lossSell decimal.Decimal
	if mgnMode == types.MgnCross {
		lossBuy = crossOpenLoss(inst, types.Buy, px, markPx)
		lossSell = crossOpenLoss(inst, types.Sell, px, markPx)
	}

	m := MaxSize{
		MaxBuy:  maxSizeAt(inst, bal.AvailBal, px, lever, taker, lossBuy),
		MaxSell: maxSizeAt(inst, bal.AvailBal, px, lever, taker, lossSell),
	}
	// 保守的那一侧还要与按标记价算的结果取较小者，是买是卖取决于合约类型。
	if markPx.IsPositive() && !markPx.Equal(px) {
		if conservativeSide(inst) == types.Buy {
			if byMark := maxSizeAt(inst, bal.AvailBal, markPx, lever, taker,
				lossBuy); byMark.LessThan(m.MaxBuy) {
				m.MaxBuy = byMark
			}
		} else if byMark := maxSizeAt(inst, bal.AvailBal, markPx, lever, taker,
			lossSell); byMark.LessThan(m.MaxSell) {
			m.MaxSell = byMark
		}
	}
	return m, nil
}

// maxSizeAt 计算在给定价格下可用余额最多支撑多少张，已按 lotSz 向下取整。
// maxSizeAt 按单一价格计算最多能开多少张。
//
// perLoss 是【每张】开仓瞬间就会产生的盯市浮亏，只在全仓下非零，见 MaxSize。
func maxSizeAt(inst refdata.Instrument, avail, px, lever, takerRate,
	perLoss decimal.Decimal) decimal.Decimal {

	if !avail.IsPositive() || !lever.IsPositive() {
		return decimal.Zero
	}
	// 每张所需 = 每张保证金 + 每张手续费 + 每张开仓即产生的浮亏
	one := inst.ContractQty(decimal.NewFromInt(1))
	perNotional := one.Mul(px)
	if inst.IsInverse() {
		perNotional = div(one, px)
	}
	per := div(perNotional, lever).Add(perNotional.Mul(takerRate)).Add(perLoss)
	if !per.IsPositive() {
		return decimal.Zero
	}
	return refdata.FloorToStep(div(avail, per), inst.LotSz)
}

// crossOpenLoss 返回全仓下每张开仓即产生的盯市浮亏。
//
// 逐仓没有这一项：浮亏落在仓位保证金上，可用余额不受影响。全仓的保证金从未离开
// 现金余额，浮亏直接扣减可用余额，因此以劣于标记价的价格开仓，一成交就先亏掉
// 一截可用额度，能开的张数随之减少。
//
// 实测差距很大：标记价 2447、以 3180.83 挂买单时，不计这一项算出 94.23 张，
// OKX 给的是 28.59 —— 高估 3.3 倍。价格越偏离标记价，高估越离谱。
//
// 只算不利的一侧。以优于标记价的价格开仓会立刻产生浮盈，而 OKX 不让这笔浮盈
// 撑起更大的仓位：实测标记价 2446、以 1712.75 挂买单时 OKX 给 175 张，正是
// 不含任何浮盈加成的数。
func crossOpenLoss(inst refdata.Instrument, side types.Side,
	px, markPx decimal.Decimal) decimal.Decimal {

	if !markPx.IsPositive() || !px.IsPositive() {
		return decimal.Zero
	}
	one := inst.ContractQty(decimal.NewFromInt(1))

	// 反向合约的每张名义价值是 Q/px，故浮亏按倒数之差算。不利的方向不变：
	// 多头开在高于标记价、空头开在低于标记价，都是一成交就亏。
	if inst.IsInverse() {
		var diff decimal.Decimal
		if side == types.Sell {
			diff = div(one, px).Sub(div(one, markPx))
		} else {
			diff = div(one, markPx).Sub(div(one, px))
		}
		if !diff.IsPositive() {
			return decimal.Zero
		}
		return diff
	}

	diff := px.Sub(markPx)
	if side == types.Sell {
		diff = diff.Neg()
	}
	if !diff.IsPositive() {
		return decimal.Zero
	}
	return one.Mul(diff)
}

// conservativeSide 返回在 max-size 计算中需要在委托价与标记价之间取保守值的那一侧。
//
// 判据是「谁的保证金需求会随着行情往不利方向走而变大」，实测两类合约恰好相反：
//
//	正向  名义 = Q×px。空头怕涨，涨则名义涨、保证金涨 —— 空头取保守值
//	反向  名义 = Q/px。多头怕跌，跌则名义涨、保证金涨 —— 多头取保守值
//
// 另一侧直接按委托价算。两类合约各五组价格全部命中，其中偏离标记价 30% 的样本
// 上两种取法相差 30% 以上，足以区分。
func conservativeSide(inst refdata.Instrument) types.Side {
	if inst.IsInverse() {
		return types.Buy
	}
	return types.Sell
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

	// 补上保证金变化，使预演的仓位与真实成交后一致。
	// 全仓的保证金不划入仓位，Margin 保持为零——这与 OKX 在全仓仓位上把 margin
	// 字段返回为空串是一致的。
	if mgnMode == types.MgnIsolated {
		md := computeMarginDelta(res, inst, notional(inst, res.OpenedSz, f.Px), pos.Lever, mgnMode)
		after := res.After
		after.Margin = pos.Margin.Sub(md.Release).Add(md.Add)
		if after.IsEmpty() {
			after.Margin = decimal.Zero
		}
		res.After = after
	}
	return res, nil
}

// PendingOrder 是一笔已挂出、尚未成交的委托及其冻结明细。
type PendingOrder struct {
	OrdID string
	Order Order
	Req   OrderReq
	Cost  OrderCost
}

// CancelOrder 撤销挂单并释放其冻结。
//
// 经内置撮合成交的委托会自动解除冻结，无需调用本方法；只有引擎自行撮合、
// 手工灌 Fill 时才需要——或者在 Fill 里带上 OrdID 让模拟器代劳。
func (s *Simulator) CancelOrder(ordID string) error {
	if _, ok := s.pending[ordID]; !ok {
		return okxerr.New(okxerr.CodeParamError, "找不到挂单 %q", ordID)
	}
	delete(s.pending, ordID)
	return nil
}

// PendingOrders 返回尚未成交的委托，instID 为空则返回全部。
//
// 按下单先后排序，同一时刻的按委托 ID——这与撮合的处理顺序一致，使遍历结果
// 与成交顺序对得上，也使输出可复现。
func (s *Simulator) PendingOrders(instID string) []PendingOrder {
	out := make([]PendingOrder, 0, len(s.pending))
	for _, o := range s.pending {
		if instID != "" && o.Order.InstID != instID {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order.Ts != out[j].Order.Ts {
			return out[i].Order.Ts < out[j].Order.Ts
		}
		return out[i].OrdID < out[j].OrdID
	})
	return out
}

// PendingOrderOf 按委托 ID 取一笔挂单；不存在时第二个返回值为 false。
func (s *Simulator) PendingOrderOf(ordID string) (PendingOrder, bool) {
	o, ok := s.pending[ordID]
	return o, ok
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

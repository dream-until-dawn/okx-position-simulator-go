package okxsim

import (
	"sort"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// 本文件实现内置撮合。
//
// 撮合是每个回测引擎都要做的事，各自重写一遍既浪费又容易在成交角色、冻结释放
// 这类细节上出错，因此内置。但它不是强制的：手工灌 Fill 的路径继续完整可用，
// 有自己撮合逻辑的引擎不必迁就这里的模型。
//
// 撮合模型及其边界：
//
//	驱动      按行情步推进，买单在 Low <= 委托价时成交，卖单在 High >= 委托价时成交
//	成交价    挂住的限价单按委托价成交；下单即可成交的按最新价，且不劣于限价
//	成交角色  由模拟器判定——挂住后被价格触发是 maker，下单即可成交是 taker
//	          这一项直接决定手续费，交给调用方填写是常见的错误来源
//	时间优先  同一步内多笔可成交时按下单先后处理，与真实的 FIFO 一致
//
// 明确不建模的：盘口深度、部分成交、排队位置。没有深度数据就无从建模这三者，
// 强行假设只会给出看似精确实则杜撰的结果。委托在被触及时全额成交。

// Order 是一笔委托。
type Order struct {
	OrdID      string
	InstID     string
	TdMode     types.TdMode
	Side       types.Side
	PosSide    types.PosSide
	OrdType    types.OrdType
	Px         decimal.Decimal // 限价类委托必填；市价单忽略
	Sz         decimal.Decimal
	ReduceOnly bool
	Ts         int64
}

// Bar 是一步行情。
//
// High 与 Low 界定该步内价格触及的范围，撮合据此判断挂单是否成交；
// Last 是该步结束时的最新成交价，用于判断新委托是否立即可成交。
// MarkPx 留空时以 Last 代替。
type Bar struct {
	InstID string
	High   decimal.Decimal
	Low    decimal.Decimal
	Last   decimal.Decimal
	MarkPx decimal.Decimal
	Ts     int64

	// Funding 非空时在本步结算一次资金费。
	//
	// 留空即【不计资金费】，等价于零费率。这是默认行为，也是绝大多数长周期
	// 回测的实际状态——OKX 的历史资金费率只保留约 3 个月，超出窗口就取不到
	// 真实费率。不计资金费会系统性高估多头持仓的收益，用之前请知悉。
	//
	// 结算发生在撮合【之前】，作用于带入本步的仓位。理由是资金费的结算时刻
	// 落在整点，也就是一根 K 线的起点；若放在撮合之后，本步内新开的仓位会被
	// 收取一笔它并未持有过的资金费。
	Funding *Funding
}

func (b Bar) markPx() decimal.Decimal {
	if b.MarkPx.IsPositive() {
		return b.MarkPx
	}
	return b.Last
}

// PlaceResult 是下单的结果。
type PlaceResult struct {
	OrdID string
	State types.OrdState
	Cost  OrderCost    // 挂住时的冻结明细；立即成交时为零值
	Fills []FillResult // 立即成交产生的结果
}

// StepResult 是推进一步行情的结果。
type StepResult struct {
	Ts       int64
	Fundings []FundingResult
	Fills    []FillResult
	Canceled []string // 本步被撤销的委托，如资金不足以承接成交
}

// LastPx 返回某合约当前的最新成交价；未推进过行情则返回零值。
func (s *Simulator) LastPx(instID string) decimal.Decimal { return s.last[instID] }

// SetLast 设置最新成交价，供下单时判断委托是否立即可成交。
//
// Advance 会自动更新它；只有在不使用内置撮合、却仍想下市价单时才需要手工设置。
func (s *Simulator) SetLast(instID string, px decimal.Decimal) error {
	if !px.IsPositive() {
		return okxerr.New(okxerr.CodeParamError, "最新价须为正数，实为 %s", px)
	}
	s.last[instID] = px
	return nil
}

// marketable 报告一笔委托在当前最新价下是否立即可成交，并给出成交价。
//
// 买单在委托价不低于最新价时可成交，卖单在委托价不高于最新价时可成交。
// 成交价取最新价——它对下单方不劣于委托价，与真实撮合一致：
// 一笔限价买单不会以高于其限价的价格成交。
func marketable(o Order, last decimal.Decimal) (decimal.Decimal, bool) {
	if !last.IsPositive() {
		return decimal.Zero, false
	}
	if o.OrdType.IsMarketable() {
		return last, true
	}
	if o.Side == types.Buy {
		if o.Px.GreaterThanOrEqual(last) {
			return last, true
		}
		return decimal.Zero, false
	}
	if o.Px.LessThanOrEqual(last) {
		return last, true
	}
	return decimal.Zero, false
}

// PlaceOrder 下单。
//
// 立即可成交的委托当场成交并按 taker 计费；否则挂入簿中并冻结资金，
// 待 Advance 推进行情时按 maker 成交。
//
// 只挂单委托(post_only)若在下单时即可成交，会被直接撤销而非成交——与 OKX 一致。
// 立即成交类委托(ioc/fok/market)不会挂入簿中，无法立即成交时直接撤销。
func (s *Simulator) PlaceOrder(o Order) (PlaceResult, error) {
	if o.OrdID == "" {
		return PlaceResult{}, okxerr.New(okxerr.CodeParamEmpty, "ordId 不能为空")
	}
	if _, dup := s.pending[o.OrdID]; dup {
		return PlaceResult{}, okxerr.New(okxerr.CodeParamError, "ordId %q 已存在", o.OrdID)
	}
	if o.OrdType == "" {
		o.OrdType = types.OrdLimit
	}
	if !o.OrdType.Valid() {
		return PlaceResult{}, okxerr.New(okxerr.CodeParamError,
			"ordType: 非法的委托类型 %q", o.OrdType)
	}
	inst, err := s.cfg.RefData.Instrument(o.InstID)
	if err != nil {
		return PlaceResult{}, err
	}
	if err := inst.ValidateOrderSize(o.Sz, o.OrdType); err != nil {
		return PlaceResult{}, err
	}
	side, err := s.normalizePosSide(o.PosSide)
	if err != nil {
		return PlaceResult{}, err
	}
	o.PosSide = side

	if !o.OrdType.IsMarketable() {
		if !o.Px.IsPositive() {
			return PlaceResult{}, okxerr.New(okxerr.CodeParamError,
				"px: 限价类委托必须给出委托价")
		}
		// OKX 不因价格超精度而拒单，只按方向取整，此处复刻其行为
		o.Px = inst.RoundPrice(o.Px, o.Side)
	}
	if err := s.checkReduceOnly(o, side); err != nil {
		return PlaceResult{}, err
	}

	last := s.last[o.InstID]
	fillPx, canFill := marketable(o, last)

	switch {
	case canFill && o.OrdType.IsPostOnly():
		// 只挂单委托若会立即成交，OKX 直接撤销而不成交
		return PlaceResult{OrdID: o.OrdID, State: types.OrdCanceled}, nil

	case canFill:
		fr, err := s.fillOrder(o, fillPx, types.Taker, o.Ts)
		if err != nil {
			return PlaceResult{}, err
		}
		return PlaceResult{OrdID: o.OrdID, State: types.OrdFilled,
			Fills: []FillResult{fr}}, nil

	case o.OrdType.IsImmediate():
		// 立即成交类委托无法成交时直接撤销，不挂入簿中
		return PlaceResult{OrdID: o.OrdID, State: types.OrdCanceled}, nil
	}

	cost, err := s.freezeOrder(o)
	if err != nil {
		return PlaceResult{}, err
	}
	return PlaceResult{OrdID: o.OrdID, State: types.OrdLive, Cost: cost}, nil
}

// checkReduceOnly 校验只减仓委托确有可减的仓位。
func (s *Simulator) checkReduceOnly(o Order, side types.PosSide) error {
	if !o.ReduceOnly {
		return nil
	}
	open, _ := s.splitOrderSize(OrderReq{
		InstID: o.InstID, TdMode: o.TdMode, Side: o.Side,
		PosSide: side, Px: o.Px, Sz: o.Sz,
	}, side)
	if open.IsPositive() {
		return okxerr.New(okxerr.CodeParamError,
			"reduceOnly: 该委托含 %s 张开仓量，与只减仓冲突", open)
	}
	return nil
}

// freezeOrder 把委托挂入簿中并冻结资金。
func (s *Simulator) freezeOrder(o Order) (OrderCost, error) {
	req := OrderReq{
		InstID: o.InstID, TdMode: o.TdMode, Side: o.Side,
		PosSide: o.PosSide, Px: o.Px, Sz: o.Sz,
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
	s.pending[o.OrdID] = PendingOrder{OrdID: o.OrdID, Req: req, Cost: cost, Order: o}
	return cost, nil
}

// fillOrder 成交一笔委托。
//
// 冻结的解除交给 Fill 统一处理：它带上 OrdID 后会在余额校验中排除该委托自身的
// 冻结，并在校验通过后解除。这样成功与失败两条路径上冻结的去留都是确定的，
// 无需在此手工回滚。
func (s *Simulator) fillOrder(o Order, px decimal.Decimal,
	exec types.ExecType, ts int64) (FillResult, error) {

	return s.Fill(Fill{
		OrdID: o.OrdID, InstID: o.InstID, TdMode: o.TdMode, Side: o.Side,
		PosSide: o.PosSide, Sz: o.Sz, Px: px, ExecType: exec, Ts: ts,
	})
}

// Advance 推进一步行情：更新价格、撮合挂单，返回本步产生的成交。
//
// 挂单的成交判据是价格是否触及：买单在 Low <= 委托价时成交，卖单在
// High >= 委托价时成交，成交价即委托价，成交角色为 maker。
//
// 同一步内多笔可成交时按下单先后处理，与真实的时间优先一致；同一时刻下的
// 委托按委托 ID 排序，使结果可复现。
//
// 一步之内的执行顺序是确定的：资金费结算 -> 撮合。强平检查将在同一方法内
// 接在撮合之后，因为它要看的是本步全部变动落定后的风险状况。
func (s *Simulator) Advance(b Bar) (StepResult, error) {
	if b.InstID == "" {
		return StepResult{}, okxerr.New(okxerr.CodeParamEmpty, "instId 不能为空")
	}
	if !b.Last.IsPositive() {
		return StepResult{}, okxerr.New(okxerr.CodeParamError,
			"last: 最新价须为正数，实为 %s", b.Last)
	}
	if !b.High.IsPositive() {
		b.High = b.Last
	}
	if !b.Low.IsPositive() {
		b.Low = b.Last
	}
	if b.Low.GreaterThan(b.High) {
		return StepResult{}, okxerr.New(okxerr.CodeParamError,
			"low(%s) 不应高于 high(%s)", b.Low, b.High)
	}

	s.last[b.InstID] = b.Last
	if err := s.SetMark(b.InstID, b.markPx()); err != nil {
		return StepResult{}, err
	}

	res := StepResult{Ts: b.Ts}

	// 资金费先于撮合结算，作用于带入本步的仓位——结算时刻落在整点，
	// 即一根 K 线的起点；放在撮合之后会让本步新开的仓位被收一笔它并未持有过的费。
	if b.Funding != nil {
		fr, err := s.settleFunding(b.InstID, *b.Funding, b.markPx(), b.Ts)
		if err != nil {
			return StepResult{}, err
		}
		res.Fundings = fr
	}

	for _, o := range s.triggeredOrders(b) {
		fr, err := s.fillOrder(o, o.Px, types.Maker, b.Ts)
		if err != nil {
			// 余额不足以承接这笔成交时撤销该委托，与真实撮合中的资金校验一致
			delete(s.pending, o.OrdID)
			res.Canceled = append(res.Canceled, o.OrdID)
			continue
		}
		res.Fills = append(res.Fills, fr)
	}
	return res, nil
}

// triggeredOrders 返回本步行情会触发的挂单，按时间优先排序。
func (s *Simulator) triggeredOrders(b Bar) []Order {
	var out []Order
	for _, p := range s.pending {
		o := p.Order
		if o.InstID != b.InstID || !o.Px.IsPositive() {
			continue
		}
		if o.Side == types.Buy && b.Low.LessThanOrEqual(o.Px) {
			out = append(out, o)
			continue
		}
		if o.Side == types.Sell && b.High.GreaterThanOrEqual(o.Px) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ts != out[j].Ts {
			return out[i].Ts < out[j].Ts
		}
		return out[i].OrdID < out[j].OrdID
	})
	return out
}

// OpenOrders 返回某合约上尚未成交的委托，按下单先后排序；instID 为空则返回全部。
func (s *Simulator) OpenOrders(instID string) []Order {
	var out []Order
	for _, p := range s.pending {
		if instID != "" && p.Order.InstID != instID {
			continue
		}
		out = append(out, p.Order)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ts != out[j].Ts {
			return out[i].Ts < out[j].Ts
		}
		return out[i].OrdID < out[j].OrdID
	})
	return out
}

package okxsim

import (
	"fmt"
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

	// IdxPx 是指数价，只有 triggerPxType 为 index 的算法委托才用得上。
	// 留空时这类委托无从判断，会被跳过并在 StepResult 里说明原因——
	// 不拿标记价顶替，那是另一个价格。
	IdxPx decimal.Decimal

	Ts int64

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

// markPx 返回本步的标记价；没给就退回最新成交价。
//
// 这个退化是有代价的：强平判据本该看标记价，用最新成交价会让插针扫掉本不该爆的
// 仓位。所以本库默认拒绝——确实拿不到数据时才用 Config.AllowMarkPxFallback
// 显式选择退出，见那里的说明。
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

	// Reason 在 State 为 canceled 时说明原因。
	//
	// 只挂单委托会立即成交而被撤，与立即成交类委托无法成交而被撤，
	// 两者的状态都是 canceled，但含义截然不同——前者是价格挂得太激进，
	// 后者是价格挂得够不着。不区分开，使用者只能去猜。
	Reason CancelReason
	Detail string
}

// Canceled 报告该委托是否被撤销，并给出原因。
func (r PlaceResult) Canceled() (CancelReason, bool) {
	if r.State == types.OrdCanceled {
		return r.Reason, true
	}
	return "", false
}

// StepResult 是推进一步行情的结果。
type StepResult struct {
	Ts       int64
	Fundings []FundingResult
	Fills    []FillResult
	// Liquidations 是本步发生的强平。
	//
	// ⚠️ **非空意味着你的策略状态可能已经失效。** 强平会做两件事，而两件都发生在
	// 策略不知情的情况下：
	//
	//	撤掉该合约（全仓则是该结算币种）下的全部挂单——挂单簿被清空
	//	拿走仓位——阶梯减仓拿走一部分，全平拿走全部
	//
	// 只读 Fills 的策略会在**被清算之前的状态**上继续跑：以为自己还持有某个目标
	// 仓位、还挂着某几格单，于是在空簿上按旧层号重挂。此后每个数都建立在一个不存在
	// 的前提上，**而且全程不报错**。
	//
	// 这不是假想：一个下游的阶梯网格正是这么中招的——它的执行层读了 Canceled，
	// 但策略层只处理了「资金不足」那几种撤单原因，漏了强平这一类。见
	// docs/silent-risks.md 的「跨库静默」一节。
	//
	// 被撤销的委托 ID 在各 Liquidation 的 CanceledOrders 里，同时也会以
	// CancelLiquidation 为原因出现在 Canceled 中。
	Liquidations []Liquidation

	// AlgoTriggers 是本步被触发的算法委托。触发后生成的普通委托若当场成交，
	// 对应的成交同时出现在 Fills 里。
	AlgoTriggers []AlgoTrigger

	// Canceled 是本步被撤销的委托及其原因。
	//
	// 只给委托 ID 是不够的：委托可能因资金不足以承接成交而撤，也可能因强平
	// 前清场而撤，两者对策略的含义完全不同。
	Canceled []Cancellation
}

// LastPx 返回某合约当前的最新成交价；未推进过行情则返回零值。
func (s *Simulator) LastPx(instID string) decimal.Decimal { return s.last[instID] }

// SetLastPx 设置最新成交价，供下单时判断委托是否立即可成交。
//
// Advance 会自动更新它；只有在不使用内置撮合、却仍想下市价单时才需要手工设置。
func (s *Simulator) SetLastPx(instID string, px decimal.Decimal) error {
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

// IndexPx 返回某合约当前的指数价；未设置过时为零。
func (s *Simulator) IndexPx(instID string) decimal.Decimal { return s.index[instID] }

// SetIndexPx 设置某合约的指数价。
//
// 只有 triggerPxType 为 index 的算法委托用得上它。单独给一个设置方法，是因为
// 指数价与最新价、标记价一样是三条独立的行情——本库不拿其中一条去顶替另一条，
// 那会让按指数价触发的委托在一个错误的价位上被扫掉。
//
// Advance 里给了 Bar.IdxPx 时会自动写入，无需再调用本方法。
func (s *Simulator) SetIndexPx(instID string, px decimal.Decimal) error {
	if instID == "" {
		return okxerr.New(okxerr.CodeParamEmpty, "instId 不能为空")
	}
	if !px.IsPositive() {
		return okxerr.New(okxerr.CodeParamError, "idxPx: 指数价须为正数，实为 %s", px)
	}
	s.index[instID] = px
	return nil
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

	// 当前杠杆下有一个最大持仓量，判据含【同方向的开仓挂单】——挂单本身就要占额度，
	// 所以这里也得卡，而不是等到成交时才发现。见 checkPosLimitAtLever。
	mgnMode, _ := o.TdMode.MgnMode()
	if openSz := s.openSizeOf(o, side); openSz.IsPositive() {
		if err := s.checkPosLimitAtLever(inst, mgnMode, side, o.Side, openSz); err != nil {
			return PlaceResult{}, err
		}
	}

	last := s.last[o.InstID]
	fillPx, canFill := marketable(o, last)

	switch {
	case canFill && o.OrdType.IsPostOnly():
		// 只挂单委托若会立即成交，OKX 直接撤销而不成交
		return PlaceResult{
			OrdID: o.OrdID, State: types.OrdCanceled,
			Reason: CancelPostOnlyWouldTake,
			Detail: fmt.Sprintf("委托价 %s 相对最新价 %s 已可成交", o.Px, last),
		}, nil

	case canFill:
		fr, err := s.fillOrder(o, fillPx, types.Taker, o.Ts)
		if err != nil {
			return PlaceResult{}, err
		}
		return PlaceResult{OrdID: o.OrdID, State: types.OrdFilled,
			Fills: []FillResult{fr}}, nil

	case o.OrdType.IsImmediate():
		// 立即成交类委托无法成交时直接撤销，不挂入簿中
		detail := fmt.Sprintf("委托价 %s 相对最新价 %s 无法成交", o.Px, last)
		if !last.IsPositive() {
			detail = "尚无最新价，无从判断能否成交——请先经 Advance 推进行情或调用 SetLastPx"
		}
		return PlaceResult{
			OrdID: o.OrdID, State: types.OrdCanceled,
			Reason: CancelImmediateUnfilled, Detail: detail,
		}, nil
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
	bal, err := s.BalanceOf(cost.Ccy)
	if err != nil {
		return OrderCost{}, err
	}
	if !cost.Affordable(bal.AvailBal) {
		return OrderCost{}, newShortfallError(cost.Ccy, bal.AvailBal, cost.Frozen, "挂出该委托")
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
// 一步之内的执行顺序是确定的：资金费结算 -> 撮合 -> 强平检查。
// 强平排在最后，因为它要看的是本步全部变动落定后的风险状况：资金费扣过了、
// 该成交的成交了，此刻的保证金率才是真实的。
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
	if !s.cfg.AllowMarkPxFallback && !b.MarkPx.IsPositive() {
		return StepResult{}, okxerr.New(okxerr.CodeParamEmpty,
			"markPx: %s 在 ts=%d 没有标记价。"+
				"退回用最新成交价顶替会让插针扫掉本不该爆的仓位，本库宁可报错也不悄悄降级。"+
				"确实拿不到标记价数据时，把 Config.AllowMarkPxFallback 设为真表示"+
				"你接受这份偏差。若回测起点早于 2020-01-01（港时），标记价历史"+
				"很可能根本不存在——OKX 的标记价 K 线一律不早于那一天，而成交价更长",
			b.InstID, b.Ts)
	}

	res := StepResult{Ts: b.Ts}

	// 算法单的触发判定要赶在行情落库之前，见 detectAlgoTriggers。
	// 判定与执行分开：执行排在资金费之后，见下。
	algoHits := s.detectAlgoTriggers(b)

	s.last[b.InstID] = b.Last
	if err := s.SetMarkPx(b.InstID, b.markPx()); err != nil {
		return StepResult{}, err
	}
	if b.IdxPx.IsPositive() {
		s.index[b.InstID] = b.IdxPx
	}

	// 资金费先于撮合结算，作用于带入本步的仓位——结算时刻落在整点，
	// 即一根 K 线的起点；放在撮合之后会让本步新开的仓位被收一笔它并未持有过的费。
	if b.Funding != nil {
		fr, err := s.settleFunding(b.InstID, *b.Funding, b.markPx(), b.Ts)
		if err != nil {
			return StepResult{}, err
		}
		res.Fundings = fr
	}

	// 算法单的执行排在资金费之后、撮合之前：排在资金费之后，本步被平掉的仓位才
	// 不会漏收一笔它确实持有过的资金费；排在撮合之前，触发生成的委托才能参与本步
	// 的撮合，否则一笔市价止损会白白拖到下一根 K 线才成交。
	algoTriggers, algoFills := s.executeAlgoTriggers(algoHits, b.Ts)
	res.AlgoTriggers = algoTriggers
	res.Fills = append(res.Fills, algoFills...)

	for _, o := range s.triggeredOrders(b) {
		fr, err := s.fillOrder(o, o.Px, types.Maker, b.Ts)
		if err != nil {
			// 余额不足以承接这笔成交时撤销该委托，与真实撮合中的资金校验一致。
			// 把原因一并带出——不然使用者只会看到委托凭空消失。
			delete(s.pending, o.OrdID)
			c := Cancellation{
				OrdID: o.OrdID, InstID: o.InstID,
				Reason: CancelInsufficientFunds, Detail: err.Error(),
			}
			if sf, ok := ShortfallOf(err); ok {
				c.Detail = sf.String()
			}
			res.Canceled = append(res.Canceled, c)
			continue
		}
		res.Fills = append(res.Fills, fr)
	}

	// 强平检查排在最后：它要看的是本步全部变动落定后的风险状况——
	// 资金费扣过了、该成交的成交了，此刻的保证金率才是真实的。
	//
	// 走导出的 CheckLiquidation，与自行撮合的调用方共用同一条路径。分成两份写
	// 迟早会分岔，而分岔的表现是「内置撮合会爆仓、自己撮合不会」这种极难察觉的事。
	liqs, err := s.CheckLiquidation(b.InstID, b.Ts)
	if err != nil {
		return StepResult{}, err
	}
	res.Liquidations = liqs
	for _, l := range liqs {
		for _, id := range l.CanceledOrders {
			res.Canceled = append(res.Canceled, Cancellation{
				OrdID: id, InstID: l.InstID, Reason: CancelLiquidation,
				Detail: fmt.Sprintf("%s %s 触发强平", l.InstID, l.PosSide),
			})
		}
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

// openSizeOf 返回一笔委托中属于【开仓】的张数。
//
// 与现有持仓反向的部分是平仓，不占最大持仓量的额度；超出持仓量的那一段才是开仓。
func (s *Simulator) openSizeOf(o Order, side types.PosSide) decimal.Decimal {
	openSz, _ := s.splitOrderSize(OrderReq{
		InstID: o.InstID, TdMode: o.TdMode, Side: o.Side,
		PosSide: o.PosSide, Sz: o.Sz, Px: o.Px,
	}, side)
	return openSz
}

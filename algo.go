package okxsim

import (
	"sort"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// AlgoOrder 是一笔算法委托。
//
// 与普通委托的根本区别在于**它不占用任何资金**：不进订单簿、不冻结保证金、
// 不计入 imr/mmr。实测挂四张计划委托，availBal / ordFrozen / imr / mmr 全部
// 纹丝不动。它只是一条「价格到了就下单」的规则。
//
// 触发之后生成一笔普通委托，随后走与手工下单完全相同的撮合与核算路径——
// 这一点也经实测确认：触发后算法单的 ordId 字段带出一笔真实委托，其
// ordType=market、state=filled，成交与手工市价单毫无二致。
type AlgoOrder struct {
	AlgoID  string
	InstID  string
	TdMode  types.TdMode
	Side    types.Side
	PosSide types.PosSide
	OrdType types.AlgoOrdType

	Sz         decimal.Decimal
	ReduceOnly bool

	// TriggerPx / OrdPx 用于 trigger 类型。
	//
	// OrdPx 留空（零值）即触发后按市价成交，给 -1 也一样——判据是「非正数即市价」。
	// 让零值做最常见的那件事是 Go 的惯例，也省掉一个能被任何 importer 改写的全局。
	TriggerPx     decimal.Decimal
	TriggerPxType types.TriggerPxType
	OrdPx         decimal.Decimal

	// 止盈止损两条腿，用于 conditional 与 oco。
	// conditional 只认其中一条：实测同时给两组参数时 OKX 只保留止损那组。
	TpTriggerPx     decimal.Decimal
	TpTriggerPxType types.TriggerPxType
	TpOrdPx         decimal.Decimal
	SlTriggerPx     decimal.Decimal
	SlTriggerPxType types.TriggerPxType
	SlOrdPx         decimal.Decimal

	// CallbackRatio 用于 move_order_stop，是回调比例（0.05 即 5%）。
	CallbackRatio decimal.Decimal

	Ts int64
}

// algoLeg 是一条触发腿。一笔算法委托可能有一条（trigger / conditional）或
// 两条（oco）腿，移动止损则是一条会随行情移动的腿。
type algoLeg struct {
	kind     string // trigger / tp / sl / move
	px       decimal.Decimal
	pxType   types.TriggerPxType
	ordPx    decimal.Decimal
	above    bool // true 表示价格【涨到】该价位时触发，false 表示【跌到】
	trailing bool
}

// pendingAlgo 是模拟器内部保存的算法委托及其运行状态。
type pendingAlgo struct {
	Order AlgoOrder
	Legs  []algoLeg

	// Extreme 是移动止损自挂单以来见过的极值，按 TriggerPxType 取价。
	// 棘轮只朝一个方向走——实测 40 次采样中多头侧的触发价零回退、空头侧零回升。
	Extreme decimal.Decimal
}

// AlgoTrigger 是一次算法委托的触发。
type AlgoTrigger struct {
	AlgoID  string
	InstID  string
	OrdType types.AlgoOrdType
	Leg     string          // 触发的是哪条腿：trigger / tp / sl / move
	Px      decimal.Decimal // 触发时的比较价
	OrdID   string          // 触发后生成的普通委托 ID
	Ts      int64

	// Fill 是该委托当场成交的结果；挂住未成交时为 nil。
	Fill *FillResult
	// Reason 在触发后下单失败时说明原因，此时 AlgoOrder 转入 order_failed。
	Reason string
}

// AlgoPlaceResult 是挂出一笔算法委托的结果。
type AlgoPlaceResult struct {
	AlgoID string
	State  types.AlgoState
	// Frozen 恒为零，保留该字段是为了让「算法单不占资金」这件事在 API 上
	// 也看得见，而不是靠使用者去读文档。
	Frozen decimal.Decimal
}

// PlaceAlgoOrder 挂出一笔算法委托。
//
// 不做资金校验，也不冻结任何资金——这与真实 OKX 一致。资金是否够用要等触发
// 那一刻才算，届时若不足，该算法委托转入 order_failed 并在 StepResult 里说明原因。
func (s *Simulator) PlaceAlgoOrder(a AlgoOrder) (AlgoPlaceResult, error) {
	if a.AlgoID == "" {
		return AlgoPlaceResult{}, okxerr.New(okxerr.CodeParamEmpty, "algoId 不能为空")
	}
	if _, ok := s.algos[a.AlgoID]; ok {
		return AlgoPlaceResult{}, okxerr.New(okxerr.CodeParamError,
			"algoId %q 已存在", a.AlgoID)
	}
	inst, err := s.cfg.RefData.Instrument(a.InstID)
	if err != nil {
		return AlgoPlaceResult{}, err
	}
	if err := inst.ValidateSize(a.Sz); err != nil {
		return AlgoPlaceResult{}, err
	}
	if !a.Side.Valid() {
		return AlgoPlaceResult{}, okxerr.New(okxerr.CodeParamError, "side: 非法方向 %q", a.Side)
	}
	if !a.OrdType.Valid() {
		return AlgoPlaceResult{}, okxerr.New(okxerr.CodeParamError,
			"ordType: 非法的算法委托类型 %q", a.OrdType)
	}
	if _, ok := a.TdMode.MgnMode(); !ok {
		return AlgoPlaceResult{}, okxerr.New(okxerr.CodeParamError,
			"tdMode: 衍生品只支持 isolated 与 cross，实为 %q", a.TdMode)
	}
	side, err := s.normalizePosSide(a.PosSide)
	if err != nil {
		return AlgoPlaceResult{}, err
	}
	a.PosSide = side

	ref := s.markOf(a.InstID, s.last[a.InstID])
	legs, err := s.buildLegs(a, inst, ref)
	if err != nil {
		return AlgoPlaceResult{}, err
	}

	p := pendingAlgo{Order: a, Legs: legs}
	if a.OrdType == types.AlgoMoveStop {
		p.Extreme = s.triggerPrice(legs[0].pxType, a.InstID, ref)
		if !p.Extreme.IsPositive() {
			return AlgoPlaceResult{}, okxerr.New(okxerr.CodeParamError,
				"%s 没有可用的参考价，移动止损的极值无从起算"+
					"——请先经 Advance 推进行情，或调用 SetLastPx / SetMarkPx", a.InstID)
		}
		p.Legs[0].px = s.movePx(p.Extreme, a.CallbackRatio, legs[0].above)
	}
	s.algos[a.AlgoID] = p
	return AlgoPlaceResult{AlgoID: a.AlgoID, State: types.AlgoLive}, nil
}

// buildLegs 把一笔算法委托拆成若干条触发腿。
//
// 触发方向（涨到还是跌到）由**挂单时**触发价与参考价的相对位置决定，这与 OKX
// 的推断方式一致——它的委托里并没有一个方向字段。止盈止损也走同一套推断，
// 不去假定「止盈必在上方」：平空仓的止盈就在下方。
func (s *Simulator) buildLegs(a AlgoOrder, inst refdata.Instrument,
	ref decimal.Decimal) ([]algoLeg, error) {

	pxType := func(t types.TriggerPxType) (types.TriggerPxType, error) {
		if t == "" {
			return types.TriggerLast, nil
		}
		if !t.Valid() {
			return "", okxerr.New(okxerr.CodeParamError, "triggerPxType: 非法取值 %q", t)
		}
		return t, nil
	}

	switch a.OrdType {
	case types.AlgoTrigger:
		if !a.TriggerPx.IsPositive() {
			return nil, okxerr.New(okxerr.CodeParamError,
				"triggerPx: 计划委托的触发价须为正数，实为 %s", a.TriggerPx)
		}
		t, err := pxType(a.TriggerPxType)
		if err != nil {
			return nil, err
		}
		return []algoLeg{{
			kind: "trigger", px: a.TriggerPx, pxType: t, ordPx: a.OrdPx,
			above: a.TriggerPx.GreaterThan(ref),
		}}, nil

	case types.AlgoConditional, types.AlgoOCO:
		var legs []algoLeg
		if a.TpTriggerPx.IsPositive() {
			t, err := pxType(a.TpTriggerPxType)
			if err != nil {
				return nil, err
			}
			legs = append(legs, algoLeg{
				kind: "tp", px: a.TpTriggerPx, pxType: t, ordPx: a.TpOrdPx,
				above: a.TpTriggerPx.GreaterThan(ref),
			})
		}
		if a.SlTriggerPx.IsPositive() {
			t, err := pxType(a.SlTriggerPxType)
			if err != nil {
				return nil, err
			}
			legs = append(legs, algoLeg{
				kind: "sl", px: a.SlTriggerPx, pxType: t, ordPx: a.SlOrdPx,
				above: a.SlTriggerPx.GreaterThan(ref),
			})
		}
		if len(legs) == 0 {
			return nil, okxerr.New(okxerr.CodeParamError,
				"%s 须至少给出止盈或止损其中一条腿的触发价", a.OrdType)
		}
		// conditional 只认一条腿。实测同时给两组参数时 OKX 保留止损那组，
		// 另一组悄悄丢弃且不报错——此处照做，但把它显式化在代码里。
		if a.OrdType == types.AlgoConditional && len(legs) > 1 {
			legs = legs[len(legs)-1:]
		}
		return legs, nil

	case types.AlgoMoveStop:
		if !a.CallbackRatio.IsPositive() || a.CallbackRatio.GreaterThanOrEqual(decimal.NewFromInt(1)) {
			return nil, okxerr.New(okxerr.CodeParamError,
				"callbackRatio: 回调比例须在 (0, 1) 之间，实为 %s", a.CallbackRatio)
		}
		t, err := pxType(a.TriggerPxType)
		if err != nil {
			return nil, err
		}
		// 移动止损总是逆着持仓方向平仓：平多的卖单跟最高价、平空的买单跟最低价。
		// 因此触发方向与 side 绑定，而不是与某个给定的价位比出来的。
		return []algoLeg{{
			kind: "move", pxType: t, ordPx: a.OrdPx,
			above: a.Side == types.Buy, trailing: true,
		}}, nil
	}
	return nil, okxerr.New(okxerr.CodeParamError, "ordType: 非法的算法委托类型 %q", a.OrdType)
}

// movePx 由极值与回调比例算出移动止损的当前触发价。
//
//	平多的卖单  最高价 × (1 − callbackRatio)
//	平空的买单  最低价 × (1 + callbackRatio)
//
// 实测确认：挂单瞬间的触发价就是「当时价格 × (1 ∓ ratio)」，极值自挂单起算而
// 不需要先出现一段有利行情；此后只朝一个方向棘轮，40 次采样零回退。
func (s *Simulator) movePx(extreme, ratio decimal.Decimal, above bool) decimal.Decimal {
	one := decimal.NewFromInt(1)
	if above {
		return extreme.Mul(one.Add(ratio))
	}
	return extreme.Mul(one.Sub(ratio))
}

// CancelAlgoOrder 撤销一笔算法委托。
func (s *Simulator) CancelAlgoOrder(algoID string) error {
	if _, ok := s.algos[algoID]; !ok {
		return okxerr.New(okxerr.CodeParamError, "找不到算法委托 %q", algoID)
	}
	delete(s.algos, algoID)
	return nil
}

// AlgoLeg 是一条触发腿。
//
// 一笔算法委托可能有一条（trigger / conditional）或两条（oco）腿，移动止损则是
// 一条会随行情移动的腿。
//
// 导出它是为了状态存档：**触发方向是挂单那一刻算出来的**，由触发价与当时的参考价
// 比出来，而 AlgoOrder 里并没有那个参考价。存档只存下单参数、恢复时重新推断方向，
// 行情已经走过去的那些委托方向就会反过来——止损变成止盈。
type AlgoLeg struct {
	Kind     string              `json:"kind"`   // trigger / tp / sl / move
	Px       decimal.Decimal     `json:"px"`     // 当前触发价
	PxType   types.TriggerPxType `json:"pxType"` // 拿哪个价来比
	OrdPx    decimal.Decimal     `json:"ordPx"`  // 触发后的委托价，非正数即市价
	Above    bool                `json:"above"`  // true 表示价格【涨到】该价位时触发
	Trailing bool                `json:"trailing"`
}

// PendingAlgo 是一笔待触发的算法委托及其当前状态。
//
// 与 PendingOrder 同构：Order 是当初挂出去的参数，其余字段是它此刻的运行状态。
// 移动止损的触发价会随行情走，只看 Order 是看不出来的。
type PendingAlgo struct {
	AlgoID string    `json:"algoId"`
	Order  AlgoOrder `json:"order"`

	// TriggerPx 是当前的触发价，等于 Legs[0].Px，作为最常用的那一项单独给出。
	TriggerPx decimal.Decimal `json:"triggerPx"`

	// Extreme 是移动止损自挂单以来见过的极值，其余类型为零。
	Extreme decimal.Decimal `json:"extreme"`

	// Legs 是全部触发腿。oco 有两条，其余一条。
	Legs []AlgoLeg `json:"legs"`
}

// PendingAlgos 返回待触发的算法委托，instID 为空则返回全部，按 algoId 排序。
func (s *Simulator) PendingAlgos(instID string) []PendingAlgo {
	out := make([]PendingAlgo, 0, len(s.algos))
	for id, p := range s.algos {
		if instID != "" && p.Order.InstID != instID {
			continue
		}
		out = append(out, toPendingAlgo(id, p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AlgoID < out[j].AlgoID })
	return out
}

// PendingAlgoOf 按 algoId 取一笔算法委托；不存在时第二个返回值为 false。
func (s *Simulator) PendingAlgoOf(algoID string) (PendingAlgo, bool) {
	p, ok := s.algos[algoID]
	if !ok {
		return PendingAlgo{}, false
	}
	return toPendingAlgo(algoID, p), true
}

func toPendingAlgo(id string, p pendingAlgo) PendingAlgo {
	out := PendingAlgo{AlgoID: id, Order: p.Order, Extreme: p.Extreme}
	for _, l := range p.Legs {
		out.Legs = append(out.Legs, AlgoLeg{
			Kind: l.kind, Px: l.px, PxType: l.pxType,
			OrdPx: l.ordPx, Above: l.above, Trailing: l.trailing,
		})
	}
	if len(out.Legs) > 0 {
		out.TriggerPx = out.Legs[0].Px
	}
	return out
}

// triggerPrice 按触发价类型取当前用于比较的价格。
func (s *Simulator) triggerPrice(t types.TriggerPxType, instID string,
	fallback decimal.Decimal) decimal.Decimal {

	switch t {
	case types.TriggerMark:
		return s.markOf(instID, fallback)
	case types.TriggerIndex:
		return s.index[instID]
	default:
		if px := s.last[instID]; px.IsPositive() {
			return px
		}
		return fallback
	}
}

// algoHit 是一次已判定成立、尚未执行的触发。
//
// 判定与执行必须分开：判定要用【本步行情落库之前】的价格才能看出标记价与指数价
// 的穿越，而执行要排在资金费结算【之后】，否则本步被平掉的仓位会漏收一笔它确实
// 持有过的资金费。两件事发生在 Advance 的不同位置，中间隔着行情落库与资金费。
type algoHit struct {
	algoID string
	order  AlgoOrder
	leg    algoLeg
	px     decimal.Decimal
	reason string
}

// detectAlgoTriggers 判定本步有哪些算法委托被触及，并推进移动止损的棘轮。
//
// 必须在本步行情落库【之前】调用：判断价格有没有触及触发价，要用「上一步的值到
// 本步的值」这一段，先落库会把上一步的值冲掉，标记价与指数价的穿越就丢了。
//
// 一根 K 线只给得出价格的区间而非路径，因此判据按价格类型分两档：
//
//	last          用 [Low, High] 整个区间判，分辨率最细
//	mark / index  只有单点值，用「上一步的值 → 本步的值」这一段判穿越
//
// 后者的分辨率天然更粗——本库不拿最新价去顶替标记价或指数价，那是另一个价格，
// 顶替会让触发点系统性偏移。指数价未提供时，index 类型的委托会被跳过并在
// AlgoTrigger.Reason 里说明。
func (s *Simulator) detectAlgoTriggers(b Bar) []algoHit {
	ids := make([]string, 0, len(s.algos))
	for id, p := range s.algos {
		if p.Order.InstID == b.InstID {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// 同一步内多笔可触发时按 algoId 处理，使结果可复现
	sort.Strings(ids)

	var out []algoHit
	for _, id := range ids {
		p := s.algos[id]

		// 移动止损先按本步的有利极值棘轮，再判触发。
		//
		// 一根 K 线内的路径不可知，这是一个**明确的取舍**：先棘轮意味着假定有利
		// 的那一段先发生，于是波动大的一根 K 线更容易触发止损。反过来假定不利
		// 的一段先发生，则同一根 K 线可能完全不触发。两者都可能是真实路径。
		// 选前者是因为它对策略更保守——止损触发得更频繁，回测里不会凭空多出
		// 一批本该被止损扫掉的长趋势。
		if p.Legs[0].trailing {
			hi, lo := s.triggerRange(p.Legs[0].pxType, b)
			if !hi.IsPositive() {
				out = append(out, algoHit{algoID: id, order: p.Order, leg: p.Legs[0],
					reason: "缺少 " + string(p.Legs[0].pxType) + " 价格，无法推进移动止损"})
				continue
			}
			ext := p.Extreme
			if p.Legs[0].above {
				if lo.LessThan(ext) {
					ext = lo
				}
			} else if hi.GreaterThan(ext) {
				ext = hi
			}
			p.Extreme = ext
			p.Legs[0].px = s.movePx(ext, p.Order.CallbackRatio, p.Legs[0].above)
			s.algos[id] = p
		}

		leg, hit, px, reason := s.firstTriggeredLeg(p, b)
		if reason != "" {
			out = append(out, algoHit{algoID: id, order: p.Order, reason: reason})
			continue
		}
		if !hit {
			continue
		}
		// 触发即作废：oco 的两条腿只能生效一条，与真实 OCO 一致。
		delete(s.algos, id)
		out = append(out, algoHit{algoID: id, order: p.Order, leg: leg, px: px})
	}
	return out
}

// executeAlgoTriggers 把已判定的触发转成普通委托并执行。
func (s *Simulator) executeAlgoTriggers(hits []algoHit, ts int64) ([]AlgoTrigger, []FillResult) {
	var out []AlgoTrigger
	var fills []FillResult
	for _, h := range hits {
		t := AlgoTrigger{
			AlgoID: h.algoID, InstID: h.order.InstID, OrdType: h.order.OrdType,
			Leg: h.leg.kind, Px: h.px, Ts: ts, Reason: h.reason,
		}
		if h.reason != "" {
			out = append(out, t)
			continue
		}
		t.OrdID = algoOrdID(h.algoID, h.leg.kind)
		fr, err := s.executeAlgo(h.order, h.leg, h.px, ts)
		if err != nil {
			t.Reason = err.Error()
			if sf, ok := ShortfallOf(err); ok {
				t.Reason = sf.String()
			}
			out = append(out, t)
			continue
		}
		if fr != nil {
			t.Fill = fr
			fills = append(fills, *fr)
		}
		out = append(out, t)
	}
	return out, fills
}

// firstTriggeredLeg 找出本步第一条被触及的腿。
//
// 多条腿同时触及时取先给出的那条——oco 的两条腿分处价格两侧，一根 K 线同时
// 扫到两侧只能说明这根 K 线太粗，此时的先后无从判定，取序即可复现。
func (s *Simulator) firstTriggeredLeg(p pendingAlgo, b Bar) (algoLeg, bool, decimal.Decimal, string) {
	for _, leg := range p.Legs {
		hi, lo := s.triggerRange(leg.pxType, b)
		if !hi.IsPositive() {
			return algoLeg{}, false, decimal.Zero,
				"缺少 " + string(leg.pxType) + " 价格，该委托本步无从判断"
		}
		if leg.above && hi.GreaterThanOrEqual(leg.px) {
			return leg, true, leg.px, ""
		}
		if !leg.above && lo.LessThanOrEqual(leg.px) {
			return leg, true, leg.px, ""
		}
	}
	return algoLeg{}, false, decimal.Zero, ""
}

// triggerRange 返回本步内某种价格类型走过的区间。
func (s *Simulator) triggerRange(t types.TriggerPxType, b Bar) (hi, lo decimal.Decimal) {
	switch t {
	case types.TriggerMark:
		cur := b.markPx()
		prev := s.marks[b.InstID]
		if !prev.IsPositive() {
			prev = cur
		}
		return decimal.Max(prev, cur), decimal.Min(prev, cur)
	case types.TriggerIndex:
		cur := b.IdxPx
		if !cur.IsPositive() {
			return decimal.Zero, decimal.Zero
		}
		prev := s.index[b.InstID]
		if !prev.IsPositive() {
			prev = cur
		}
		return decimal.Max(prev, cur), decimal.Min(prev, cur)
	default:
		return b.High, b.Low
	}
}

// executeAlgo 把一条已触发的腿转成普通委托并执行。
//
//	ordPx 为 -1 或留空  按市价成交，成交价取触发价——触发那一刻的市价就是触发价，
//	                    这是本模型能给出的最准的价，不去凭空加一段滑点
//	ordPx 为具体价      挂成限价委托，随后与手工挂单走同一条撮合路径
func (s *Simulator) executeAlgo(a AlgoOrder, leg algoLeg, px decimal.Decimal,
	ts int64) (*FillResult, error) {

	ordID := algoOrdID(a.AlgoID, leg.kind)
	if leg.ordPx.IsPositive() {
		_, err := s.PlaceOrder(Order{
			OrdID: ordID, InstID: a.InstID, TdMode: a.TdMode,
			Side: a.Side, PosSide: a.PosSide, OrdType: types.OrdLimit,
			Px: leg.ordPx, Sz: a.Sz, ReduceOnly: a.ReduceOnly, Ts: ts,
		})
		return nil, err
	}
	fr, err := s.Fill(Fill{
		InstID: a.InstID, TdMode: a.TdMode, Side: a.Side, PosSide: a.PosSide,
		Sz: a.Sz, Px: px, ExecType: types.Taker, Ts: ts,
	})
	if err != nil {
		return nil, err
	}
	return &fr, nil
}

// algoOrdID 由算法委托 ID 与触发的腿拼出生成委托的 ID。
//
// 真实 OKX 给的是一个新的雪花 ID，本库改用可推导的拼接——回测里可复现比
// 拟真更要紧，使用者能从委托直接看出它由哪笔算法委托的哪条腿生成。
func algoOrdID(algoID, leg string) string { return algoID + ":" + leg }

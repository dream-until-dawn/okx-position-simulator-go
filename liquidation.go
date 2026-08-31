package okxsim

import (
	"sort"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// 本文件实现逐仓强平。
//
// 触发判据与强平价均已在真实仓位上标定（五个独立样本，横跨 tickSz 三个量级、
// 杠杆 5x 与 50x、多空两侧，公式与 OKX 的差值在 1e-16 至 1e-19）。
// 强平后的资金去向以模拟盘上真实触发的一次强平为准。

// LiquidationKind 是一次强平动作的类型。
type LiquidationKind string

const (
	// LiqPartial 阶梯部分减仓：减到降入更低档位后重算，仓位可能因此被救回。
	LiqPartial LiquidationKind = "partial"
	// LiqFull 全部强平。
	LiqFull LiquidationKind = "full"
)

// Liquidation 是一次强平动作。
type Liquidation struct {
	InstID  string
	PosSide types.PosSide
	Kind    LiquidationKind

	Sz       decimal.Decimal // 被平掉的张数
	Px       decimal.Decimal // 强平成交价
	Pnl      decimal.Decimal // 该次平仓实现的盈亏
	Fee      decimal.Decimal // 强平手续费，负数表示收取
	Loss     decimal.Decimal // 该仓位在本次强平中损失的保证金
	Bankrupt decimal.Decimal // 穿仓金额，正数表示亏损超出保证金的部分

	MgnRatioBefore decimal.Decimal
	MgnRatioAfter  decimal.Decimal
	TierBefore     int
	TierAfter      int

	CanceledOrders []string // 强平前撤销的挂单
	Ts             int64
}

// IsBankrupt 报告本次强平是否发生了穿仓。
func (l Liquidation) IsBankrupt() bool { return l.Bankrupt.IsPositive() }

// checkLiquidation 检查某合约上的仓位是否触及强平线，并执行强平。
//
// 触发判据是保证金率 ≤ 100%，即权益 ≤ 维持保证金 + 平仓手续费。
// 判断用标记价而非最新成交价——这一点与 OKX 一致，也是强平判定的通行做法：
// 用最新价会让插针把本不该爆的仓位扫掉。
func (s *Simulator) checkLiquidation(instID string, ts int64) ([]Liquidation, error) {
	var out []Liquidation

	for _, side := range s.cfg.PosMode.PosSides() {
		key := positionKey{instID, side}
		if pos, ok := s.pos[key]; !ok || pos.IsEmpty() {
			continue
		}
		m, err := s.MetricsOf(instID, side)
		if err != nil {
			return nil, err
		}
		if !m.IsLiquidatable() {
			continue
		}
		liq, err := s.liquidate(key, m, ts)
		if err != nil {
			return nil, err
		}
		out = append(out, liq...)
	}
	return out, nil
}

// cancelOrdersOf 撤销某合约上的全部挂单，返回被撤销的委托 ID。
//
// 强平的第一步是撤单：挂单占用的保证金必须先释放回来，否则会把本可用于
// 维持仓位的资金白白锁着。
func (s *Simulator) cancelOrdersOf(instID string) []string {
	var ids []string
	for id, p := range s.pending {
		if p.Order.InstID == instID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		delete(s.pending, id)
	}
	return ids
}

// liquidate 对一个已触及强平线的仓位执行强平。
//
// 真实链路是「撤单 → 阶梯部分减仓 → 全部强平 → 穿仓」。阶梯减仓的意义在于：
// 减仓后持仓量可能落入更低的档位，维持保证金率随之下降，仓位有机会被救回。
//
// 减仓的目标档位取当前档位的下一档：减到该档上限即可使维持保证金率降下来。
// 若减到首档仍不足以让保证金率回到 1 以上，则全部强平。
func (s *Simulator) liquidate(key positionKey, m Metrics, ts int64) ([]Liquidation, error) {
	inst, err := s.cfg.RefData.Instrument(key.instID)
	if err != nil {
		return nil, err
	}
	pos := s.pos[key]
	tbl, err := refdata.TierTableFor(s.cfg.RefData, inst, pos.MgnMode)
	if err != nil {
		return nil, err
	}
	canceled := s.cancelOrdersOf(key.instID)

	var out []Liquidation
	for {
		pos = s.pos[key]
		if pos.IsEmpty() {
			break
		}
		cur, err := s.MetricsOf(key.instID, key.posSide)
		if err != nil {
			return nil, err
		}
		if !cur.IsLiquidatable() {
			break // 已被救回
		}

		target := nextLowerTier(tbl, cur.Tier)
		if target == nil {
			liq, err := s.liquidateFully(key, cur, ts)
			if err != nil {
				return nil, err
			}
			liq.CanceledOrders = canceled
			out = append(out, liq)
			break
		}

		liq, err := s.reduceToTier(key, cur, *target, ts)
		if err != nil {
			return nil, err
		}
		liq.CanceledOrders = canceled
		canceled = nil // 撤单只记在第一条上
		out = append(out, liq)
	}
	return out, nil
}

// nextLowerTier 返回比 tier 更低的一档；已是首档时返回 nil。
func nextLowerTier(tbl *refdata.TierTable, tier int) *refdata.PositionTier {
	if tier <= 1 {
		return nil
	}
	for i := range tbl.Tiers {
		if tbl.Tiers[i].Tier == tier-1 {
			return &tbl.Tiers[i]
		}
	}
	return nil
}

// liquidateFully 全部强平一个仓位。
//
// 成交价取破产价，即权益恰好归零的价格。强平在保证金率触及 1 时触发，此刻权益
// 尚有「维持保证金 + 平仓手续费」的缓冲；强平引擎接手后按破产价了结，缓冲部分
// 归风险准备金而不退还持仓方——OKX 的仓位结构里有 liqPenalty（累计爆仓罚金）
// 一项，正是这笔钱的去处。因此逐仓强平的结果是保证金全额损失。
//
// 行情跳空穿过破产价时无法按破产价成交，只能按当前标记价了结，亏损因而超出
// 保证金，超出的部分即穿仓金额。这是回测中真实存在的情形：一根 K 线内价格从
// 强平价上方直接跳到破产价下方，中间没有可成交的价位。
//
// 现金余额在整个过程中不变——保证金在开仓时就已从现金划走，强平只是让它归零。
func (s *Simulator) liquidateFully(key positionKey, m Metrics, ts int64) (Liquidation, error) {
	pos := s.pos[key]
	inst, err := s.cfg.RefData.Instrument(key.instID)
	if err != nil {
		return Liquidation{}, err
	}
	rate, err := s.fees.Rate(inst)
	if err != nil {
		return Liquidation{}, err
	}

	px := liquidationFillPx(inst, pos, m, rate.Taker)
	sz := pos.AbsPos()
	pnl := realizedPnl(inst, pos.SignedPos(), sz, pos.AvgPx, px)
	fee := notional(inst, sz, px).Mul(rate.Taker)

	liq := Liquidation{
		InstID: key.instID, PosSide: key.posSide, Kind: LiqFull,
		Sz: sz, Px: px, Pnl: pnl, Fee: fee,
		MgnRatioBefore: m.MgnRatio, TierBefore: m.Tier, TierAfter: m.Tier,
		Ts: ts,
	}

	// 保证金全额损失；亏损超出保证金的部分是穿仓
	liq.Loss = pos.Margin
	if deficit := pos.Margin.Add(pnl).Add(fee); deficit.IsNegative() {
		liq.Bankrupt = deficit.Neg()
	}

	pos.RealizedPnl = pos.RealizedPnl.Add(pnl)
	pos.Fee = pos.Fee.Add(fee)
	// 保证金里未被盈亏与手续费消耗掉的残余归风险准备金，记为爆仓罚金——
	// 这正是 OKX 仓位结构中 liqPenalty 一项的来源。按费后破产价了结时它恰为零。
	if resid := pos.Margin.Add(pnl).Add(fee); resid.IsPositive() {
		pos.LiqPenalty = pos.LiqPenalty.Sub(resid)
	}
	pos.Pos = decimal.Zero
	pos.AvgPx = decimal.Zero
	pos.Margin = decimal.Zero
	pos.UTime = ts
	delete(s.pos, key)

	return liq, nil
}

// liquidationFillPx 返回强平的【成交价】。
//
// 与 metrics.go 里的 liquidationPx 是两回事：那个算的是「预估强平价」，
// 即触发强平的价格门槛；这个算的是强平真正在哪个价位了结。
//
// 取的是【费后破产价】——保证金、已实现盈亏与强平手续费三者相加恰好归零的价格：
//
//	多头  (avgPx×Q − margin) / (Q × (1 − taker))
//	空头  (avgPx×Q + margin) / (Q × (1 + taker))
//
// 不能直接取破产价。破产价的定义是「权益恰好归零」，在那个价位上盈亏已经吃掉
// 全部保证金，再扣一笔手续费必然为负，会凭空造出一笔并不存在的穿仓。
// 现实中手续费由「强平价到破产价」之间的那段缓冲支付，费后破产价正落在两者之间。
//
// 若标记价已越过费后破产价（行情跳空），则按标记价成交——此时找不到更好的
// 对手价，亏损必然超出保证金，超出部分即真正的穿仓。
func liquidationFillPx(inst refdata.Instrument, pos Position, m Metrics,
	takerRate decimal.Decimal) decimal.Decimal {

	q := m.Qty
	if !q.IsPositive() {
		return m.MarkPx
	}
	taker := takerRate.Abs()
	one := decimal.NewFromInt(1)

	var fair decimal.Decimal
	if pos.IsLong() {
		den := q.Mul(one.Sub(taker))
		if !den.IsPositive() {
			return m.MarkPx
		}
		fair = div(pos.AvgPx.Mul(q).Sub(pos.Margin), den)
		if m.MarkPx.LessThan(fair) {
			return m.MarkPx // 跳空穿过，只能按标记价了结
		}
		return fair
	}

	den := q.Mul(one.Add(taker))
	fair = div(pos.AvgPx.Mul(q).Add(pos.Margin), den)
	if m.MarkPx.GreaterThan(fair) {
		return m.MarkPx
	}
	return fair
}

// reduceToTier 阶梯减仓：把持仓减到目标档位的上限。
//
// 减仓后持仓量落入更低的档位，维持保证金率随之下降，仓位有机会因此被救回——
// 这是「阶梯减仓」存在的意义，也是它与直接全平的区别所在。减掉的部分按破产价
// 了结，其占用的保证金同比例损失。
//
// 注意：本路径尚未在真实强平中观察到。触发它需要一个跨越多个档位的大仓位，
// 而实测用的是小资金仓位，落在首档、不会降档。公式依据 OKX 的阶梯档位规则，
// 但未经实测验证，使用时请知悉。
func (s *Simulator) reduceToTier(key positionKey, m Metrics,
	target refdata.PositionTier, ts int64) (Liquidation, error) {

	pos := s.pos[key]
	inst, err := s.cfg.RefData.Instrument(key.instID)
	if err != nil {
		return Liquidation{}, err
	}
	rate, err := s.fees.Rate(inst)
	if err != nil {
		return Liquidation{}, err
	}

	// 减到目标档位的上限即可降档；按 lotSz 取整，宁可多减一档也不要减不到位
	keep := refdata.FloorToStep(target.MaxSz, inst.LotSz)
	cut := pos.AbsPos().Sub(keep)
	if !cut.IsPositive() {
		// 目标档位容不下更少的仓位，只能全平
		return s.liquidateFully(key, m, ts)
	}

	px := liquidationFillPx(inst, pos, m, rate.Taker)
	pnl := realizedPnl(inst, pos.SignedPos(), cut, pos.AvgPx, px)
	fee := notional(inst, cut, px).Mul(rate.Taker)
	// 被减掉的那部分所占用的保证金同比例损失
	lost := pos.Margin.Mul(div(cut, pos.AbsPos()))

	pos.Pos = signedToOKX(
		pos.SignedPos().Sub(signOf(pos.SignedPos()).Mul(cut)), pos.PosSide, s.cfg.PosMode)
	pos.Margin = pos.Margin.Sub(lost)
	pos.RealizedPnl = pos.RealizedPnl.Add(pnl)
	pos.Fee = pos.Fee.Add(fee)
	pos.UTime = ts
	s.pos[key] = pos

	after, err := s.MetricsOf(key.instID, key.posSide)
	if err != nil {
		return Liquidation{}, err
	}
	return Liquidation{
		InstID: key.instID, PosSide: key.posSide, Kind: LiqPartial,
		Sz: cut, Px: px, Pnl: pnl, Fee: fee, Loss: lost,
		MgnRatioBefore: m.MgnRatio, MgnRatioAfter: after.MgnRatio,
		TierBefore: m.Tier, TierAfter: after.Tier, Ts: ts,
	}, nil
}

// signOf 返回 d 的符号：正数为 1，负数为 -1，零为 0。
func signOf(d decimal.Decimal) decimal.Decimal {
	if d.IsPositive() {
		return decimal.NewFromInt(1)
	}
	if d.IsNegative() {
		return decimal.NewFromInt(-1)
	}
	return decimal.Zero
}

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

	Sz      decimal.Decimal // 被平掉的张数
	Px      decimal.Decimal // 强平成交价
	Pnl     decimal.Decimal // 该次平仓实现的盈亏
	Fee     decimal.Decimal // 强平手续费，负数表示收取
	Penalty decimal.Decimal // 爆仓罚金，负数表示收取；等于名义价值乘以维持保证金率
	Loss    decimal.Decimal // 持仓方实际损失的保证金，封顶为该仓位的保证金

	// Excess 是亏损、手续费与罚金之和超出保证金的部分，由风险准备金承担，
	// 持仓方不承担。实测 OKX 会以一笔单独的账单把这部分退回仓位，
	// 使损失恰好等于保证金。
	//
	// 数值大意味着行情在触发与成交之间跳空穿过，是极端行情的信号，
	// 回测中值得留意。
	Excess decimal.Decimal

	MgnRatioBefore decimal.Decimal
	MgnRatioAfter  decimal.Decimal
	TierBefore     int
	TierAfter      int

	CanceledOrders []string // 强平前撤销的挂单
	Ts             int64
}

// IsBankrupt 报告本次强平的亏损是否超出了保证金。
//
// 超出的部分由风险准备金承担，持仓方的损失仍封顶为保证金；
// 该标志的意义在于提示行情在触发与成交之间跳空穿过。
func (l Liquidation) IsBankrupt() bool { return l.Excess.IsPositive() }

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
// 结算方式以模拟盘上一次真实强平为准，各项差值均为零：
//
//	成交价  触发时的市价，可能已越过强平价（实测触发价 78526.61，成交价 78566.58）
//	盈亏    按该成交价计算
//	手续费  名义价值 × taker 费率
//	罚金    名义价值 × 维持保证金率 —— 实测 0.47139948 / 117.84987 = 0.004，正是 mmr
//	损失    封顶为该仓位的保证金；超出部分由风险准备金承担并退回，持仓方不承担
//	现金    自始至终不变
//
// 曾按「成交于费后破产价、罚金为零」建模，净效果相同但四个字段全对不上：
// OKX 是照市价成交后另收一笔等于维持保证金的罚金，再把超额部分退回。
// 净效果相同不等于字段相同，而字段级同构正是本项目的验收标准之一。
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

	px := m.MarkPx
	sz := pos.AbsPos()
	nom := notional(inst, sz, px)
	pnl := realizedPnl(inst, pos.SignedPos(), sz, pos.AvgPx, px)
	fee := nom.Mul(rate.Taker.Abs()).Neg()
	penalty := nom.Mul(m.MMRRate).Neg()

	liq := Liquidation{
		InstID: key.instID, PosSide: key.posSide, Kind: LiqFull,
		Sz: sz, Px: px, Pnl: pnl, Fee: fee, Penalty: penalty,
		MgnRatioBefore: m.MgnRatio, TierBefore: m.Tier, TierAfter: m.Tier,
		Ts: ts,
	}

	// 损失封顶为保证金；超出的部分由风险准备金承担并退回，持仓方不承担
	liq.Loss = pos.Margin
	if excess := pos.Margin.Add(pnl).Add(fee).Add(penalty); excess.IsNegative() {
		liq.Excess = excess.Neg()
	}

	pos.RealizedPnl = pos.RealizedPnl.Add(pnl)
	pos.Fee = pos.Fee.Add(fee)
	pos.LiqPenalty = pos.LiqPenalty.Add(penalty)
	pos.Pos = decimal.Zero
	pos.AvgPx = decimal.Zero
	pos.Margin = decimal.Zero
	pos.UTime = ts
	delete(s.pos, key)

	return liq, nil
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

	px := m.MarkPx
	nom := notional(inst, cut, px)
	pnl := realizedPnl(inst, pos.SignedPos(), cut, pos.AvgPx, px)
	fee := nom.Mul(rate.Taker.Abs()).Neg()
	penalty := nom.Mul(m.MMRRate).Neg()
	// 被减掉的那部分所占用的保证金同比例损失
	lost := pos.Margin.Mul(div(cut, pos.AbsPos()))

	pos.Pos = signedToOKX(
		pos.SignedPos().Sub(signOf(pos.SignedPos()).Mul(cut)), pos.PosSide, s.cfg.PosMode)
	pos.Margin = pos.Margin.Sub(lost)
	pos.RealizedPnl = pos.RealizedPnl.Add(pnl)
	pos.Fee = pos.Fee.Add(fee)
	pos.LiqPenalty = pos.LiqPenalty.Add(penalty)
	pos.UTime = ts
	s.pos[key] = pos

	after, err := s.MetricsOf(key.instID, key.posSide)
	if err != nil {
		return Liquidation{}, err
	}
	return Liquidation{
		InstID: key.instID, PosSide: key.posSide, Kind: LiqPartial,
		Sz: cut, Px: px, Pnl: pnl, Fee: fee, Penalty: penalty, Loss: lost,
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

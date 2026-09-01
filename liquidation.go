package okxsim

import (
	"sort"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// 本文件实现强平：逐仓是仓位级的，全仓是结算币种级的。
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
	MgnMode types.MgnMode // 触发方式：逐仓是仓位级的，全仓是结算币种级的
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
// CheckLiquidation 在当前状态下检查某合约是否触发强平，并在触发时了结仓位。
//
// 内置撮合的使用者不必调用它——Advance 每步末尾已经调了同一个函数。本方法供
// 自行撮合、手工灌 Fill 的调用方使用：那条路径不经过 Advance，若不显式检查一次，
// 仓位就永远不会爆仓，回测里表现为本该归零的策略一路活到最后。
//
// **两级都查**：合约级的逐仓仓位，以及该合约结算币种下的全部全仓仓位。全仓的
// 强平是币种级的——同币种下的全仓仓位共担一份权益，任一合约的行情变动都可能把
// 整个币种推过强平线，所以只查合约那一级是不够的。
//
// 调用时机应当与 Advance 一致：在本步全部价格变动、资金费与成交都落定【之后】。
// 早于成交去查，看到的是一个还没发生完的状态。
//
// 被强平连带撤销的挂单在各 Liquidation 的 CanceledOrders 里。
func (s *Simulator) CheckLiquidation(instID string, ts int64) ([]Liquidation, error) {
	liqs, err := s.checkLiquidation(instID, ts)
	if err != nil {
		return nil, err
	}
	inst, err := s.cfg.RefData.Instrument(instID)
	if err != nil {
		return nil, err
	}
	cross, err := s.checkCrossLiquidation(inst.SettleCcy, ts)
	if err != nil {
		return nil, err
	}
	return append(liqs, cross...), nil
}

func (s *Simulator) checkLiquidation(instID string, ts int64) ([]Liquidation, error) {
	var out []Liquidation

	for _, side := range s.cfg.PosMode.PosSides() {
		key := positionKey{instID, side}
		pos, ok := s.pos[key]
		if !ok || pos.IsEmpty() {
			continue
		}
		// 全仓的强平是结算币种级的，走 checkCrossLiquidation。
		// 放在这里会拿 pos.Margin 去算损失，而全仓的它恒为零。
		if pos.MgnMode == types.MgnCross {
			continue
		}
		// 判据每根 K 线都要跑，所以只算它真正需要的那一个量。
		// 走 MetricsOf 会连带算出强平价、盈亏平衡价、破产价、按最新价的浮盈——
		// 实测这些占 Advance 全部分配的 42%，而判据一个都用不上。
		liquidatable, err := s.isolatedIsLiquidatable(pos, instID)
		if err != nil {
			return nil, err
		}
		if !liquidatable {
			continue
		}
		// 真要强平了才算整套：强平在一轮回测里至多几次，这里省不下什么，
		// 而给 liquidate 一个只填了几个字段的 Metrics 迟早会出事——
		// 哪天它多用一个字段，那个字段会是静默的零。
		m, err := s.MetricsOf(instID, side)
		if err != nil {
			return nil, err
		}
		liq, err := s.liquidate(key, m, ts)
		if err != nil {
			return nil, err
		}
		out = append(out, liq...)
	}
	return out, nil
}

// isolatedIsLiquidatable 只算逐仓强平的判据：保证金率是否已跌破 1。
//
// 与 computeMetrics 里那段**必须逐位一致**，所以照样是「先 div 再比」，而不是
// 改写成等价的 equity <= den。后者数学上更干净、还能省掉一次 20 位除法，但两者
// 在 1e-20 量级上不等价：div 会把 1+1e-25 舍成恰好 1。若判据用精确比较、而
// Metrics.MgnRatio 仍走 div，使用者就会看到「保证金率显示 1 却没强平」这种
// 对不上的事。省那一次除法不值得。
//
// 省下的是另外三样：强平价、盈亏平衡价、破产价。它们每根 K 线都被算出来又扔掉。
func (s *Simulator) isolatedIsLiquidatable(pos Position, instID string) (bool, error) {
	inst, err := s.cfg.RefData.Instrument(instID)
	if err != nil {
		return false, err
	}
	tier, err := s.tierOf(pos, inst)
	if err != nil {
		return false, err
	}
	rate, err := s.fees.Rate(inst)
	if err != nil {
		return false, err
	}
	markPx := s.markOf(instID, pos.AvgPx)
	// 与 computeMetrics 的 HasPosition 同口径：没有标记价就无从判断
	if markPx.IsZero() {
		return false, nil
	}

	nom := notional(inst, pos.AbsPos(), markPx)
	// 维持保证金与平仓手续费同乘一个名义价值，合并成一次乘法。
	// decimal 的加减乘都是精确的（只有除法舍入），故与分开算逐位相同。
	den := nom.Mul(tier.MMR.Add(rate.Taker.Abs()))
	if den.IsZero() {
		// 与 computeMetrics 一致：分母为零时保证金率留零，而有仓位即可强平
		return true, nil
	}
	equity := pos.Margin.Add(unrealizedPnl(inst, pos.SignedPos(), pos.AvgPx, markPx))
	return div(equity, den).LessThanOrEqual(decimal.NewFromInt(1)), nil
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
		InstID: key.instID, PosSide: key.posSide, MgnMode: types.MgnIsolated,
		Kind: LiqFull, Sz: sz, Px: px, Pnl: pnl, Fee: fee, Penalty: penalty,
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
// **本路径已由一次真实的阶梯减仓确证**（FIL-USDT-SWAP 空头 50 万张、40 倍杠杆）：
// 减仓量、盈亏、手续费、损失封顶与超额退回全部逐位对上，唯一的差异是罚金所用的
// 维持保证金率——实测是【减仓后】的档位，此前按减仓前算，多收了 85.11。
//
// 真实序列是 部分强平(101) -> 全部强平(105) -> 穿仓补偿(108)：第一次减到一档上限
// 后价格继续走，第二次才全平。两次的损失合计恰好等于仓位保证金，超出的 1.66
// 由风险准备金退回，现金余额 balChg 全程为 0。
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

	// 罚金按【减仓后】所在档位的维持保证金率算，不是减仓前的。
	//
	// 这一条由一次真实的阶梯减仓确证：FIL-USDT-SWAP 空头 50 万张（二档 mmr 0.015）
	// 减掉 25 万张，名义 17022.5，实际罚金 170.225 —— 反推率恰为 0.01，即减仓后
	// 25 万张所在的一档。按减仓前的 0.015 会算成 255.34，多出 85.11。
	//
	// 同一样本无法区分另外两种解释：「被减掉的那部分自己查档」（25 万张也落在一档）
	// 与「目标档位的率」（正是 target）。三者在这里给出同一个数，要区分得有一次
	// 从三档减到二档、而被减部分落在一档的样本。此处取 target.MMR——它与阶梯减仓
	// 的语义最直白：减到哪一档，就按哪一档收。见 docs/okx-rules.md §7。
	penalty := nom.Mul(target.MMR).Neg()

	// 保证金按【实际发生的盈亏、手续费与罚金】扣减，**不是按张数比例释放**。
	//
	// 普通的部分平仓才是比例释放（实测 624.705284 平掉四分之一释放 156.176321），
	// 而强平减仓不把任何东西释放回现金——它只是把损失从仓位保证金里扣掉，
	// balChg 全程为 0。
	//
	// 真实事件确证：保证金 843.234041 的仓位第一次减仓后 posBal 变为 506.678611，
	// 差值恰为 pnl+fee+罚金 = -336.555430；按「减半」得出的 421.617 相差 85.06。
	charge := pnl.Add(fee).Add(penalty)
	lost := charge.Neg()
	var excess decimal.Decimal
	if remain := pos.Margin.Add(charge); remain.IsNegative() {
		// 扣穿了：持仓方的损失封顶为保证金，超出部分由风险准备金承担并退回
		excess = remain.Neg()
		lost = pos.Margin
	}

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
		InstID: key.instID, PosSide: key.posSide, MgnMode: types.MgnIsolated,
		Kind: LiqPartial, Sz: cut, Px: px, Pnl: pnl, Fee: fee, Penalty: penalty, Loss: lost, Excess: excess,
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

// checkCrossLiquidation 检查某结算币种的全仓整体是否触及强平线。
//
// 全仓的风险是币种级的：同一结算币下的所有全仓仓位共担一份权益，因此触发判据也
// 只有一个——该币种的全仓保证金率 ≤ 1。触发后该币种下的全仓仓位一并了结。
//
// ⚠️ **本路径尚未实测。** 触发判据本身是可靠的：保证金率的公式经 13 个真实快照
// 逐项核对。但触发之后 OKX 究竟怎么了结，本项目没有观察到——真实全仓爆仓需要
// 一个不带对冲的裸仓位吃掉整个币种的现金，而模拟盘上的资金规模不足以在可控时间内
// 复现。故结算方式是照【已实测的逐仓强平】平移过来的：按标记价成交、收一笔吃单
// 手续费、再收一笔等于维持保证金的罚金，损失封顶、超额由风险准备金承担。
//
// 与逐仓的两处必然差异：
//
//	损失的来源  逐仓损失的是划入仓位的保证金，现金不动；全仓的保证金从未离开现金，
//	            故盈亏、手续费与罚金直接落在现金余额上
//	封顶的口径  逐仓封顶为该仓位的保证金，全仓封顶为该币种的全仓权益
//
// **阶梯减仓在全仓下未建模**：本路径一律全平该币种的全部全仓仓位。真实 OKX 在
// 全仓下也会先做阶梯减仓，那样的结果比全平温和；本实现偏保守，回测里表现为
// 高估爆仓的杀伤力，而不是低估。见 docs/roadmap.md 的覆盖缺口。
func (s *Simulator) checkCrossLiquidation(ccy string, ts int64) ([]Liquidation, error) {
	// 判据每根 K 线都要跑，走精简版：跳过初始保证金与全仓强平价，它们判据用不上。
	cm, err := s.crossMetrics(ccy, false)
	if err != nil {
		return nil, err
	}
	if !cm.IsLiquidatable() {
		return nil, nil
	}
	// 真要强平了才补齐——下面的 Liquidation 里要报出触发时的保证金率，
	// 而精简版里的其余字段是零，不该流到结果里去
	if cm, err = s.CrossMetricsOf(ccy); err != nil {
		return nil, err
	}

	keys := make([]positionKey, 0, len(s.pos))
	for k, p := range s.pos {
		if p.MgnMode != types.MgnCross {
			continue
		}
		inst, err := s.cfg.RefData.Instrument(p.InstID)
		if err != nil {
			return nil, err
		}
		if inst.SettleCcy == ccy {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	// 按 instId 与方向排序，使同一状态下的强平顺序可复现
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].instID != keys[j].instID {
			return keys[i].instID < keys[j].instID
		}
		return keys[i].posSide < keys[j].posSide
	})

	canceled := s.cancelCrossOrders(ccy)

	out := make([]Liquidation, 0, len(keys))
	var charge decimal.Decimal
	for _, key := range keys {
		pos := s.pos[key]
		inst, err := s.cfg.RefData.Instrument(key.instID)
		if err != nil {
			return nil, err
		}
		rate, err := s.fees.Rate(inst)
		if err != nil {
			return nil, err
		}
		tier, err := s.tierOf(pos, inst)
		if err != nil {
			return nil, err
		}

		px := s.markOf(key.instID, pos.AvgPx)
		sz := pos.AbsPos()
		nom := notional(inst, sz, px)
		pnl := realizedPnl(inst, pos.SignedPos(), sz, pos.AvgPx, px)
		fee := nom.Mul(rate.Taker.Abs()).Neg()
		penalty := nom.Mul(tier.MMR).Neg()
		charge = charge.Add(pnl).Add(fee).Add(penalty)

		out = append(out, Liquidation{
			InstID: key.instID, PosSide: key.posSide, MgnMode: types.MgnCross,
			Kind: LiqFull, Sz: sz, Px: px, Pnl: pnl, Fee: fee, Penalty: penalty,
			MgnRatioBefore: cm.MgnRatio, TierBefore: tier.Tier, TierAfter: tier.Tier,
			Ts: ts,
		})

		pos.RealizedPnl = pos.RealizedPnl.Add(pnl)
		pos.Fee = pos.Fee.Add(fee)
		pos.LiqPenalty = pos.LiqPenalty.Add(penalty)
		pos.UTime = ts
		delete(s.pos, key)
	}
	out[len(out)-1].CanceledOrders = canceled

	// 现金承接全部盈亏、手续费与罚金；跌破零的部分由风险准备金承担，
	// 持仓方的损失封顶为该币种的全仓权益。
	before := s.cash[ccy]
	after := before.Add(charge)
	var excess decimal.Decimal
	if after.IsNegative() {
		excess = after.Neg()
		after = decimal.Zero
	}
	s.cash[ccy] = after

	// 损失与超额按各仓位的消耗占比分摊，使各笔之和恰好等于整体
	loss := before.Sub(after)
	if total := charge.Neg(); total.IsPositive() {
		for i := range out {
			share := div(out[i].Pnl.Add(out[i].Fee).Add(out[i].Penalty).Neg(), total)
			out[i].Loss = loss.Mul(share)
			out[i].Excess = excess.Mul(share)
		}
	}
	return out, nil
}

// cancelCrossOrders 撤销某结算币种下的全部全仓挂单，返回被撤销的委托 ID。
func (s *Simulator) cancelCrossOrders(ccy string) []string {
	var ids []string
	for id, o := range s.pending {
		if o.Cost.Ccy == ccy && o.Order.TdMode == types.TdCross {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		delete(s.pending, id)
	}
	return ids
}

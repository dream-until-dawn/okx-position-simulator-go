package okxsim

import (
	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// familyKey 是全仓合并查档的聚合键。
//
// 必须带上 InstType。同一 instFamily 下的永续与交割**不共用一张档位表**，
// 这一点由实测确认：GRASS-USDT 的永续 ctVal=10、一档上限 10000 张（即 100000
// 个标的币）、mmr 0.02、杠杆上限 20；交割 ctVal=1、一档上限 12500 张（12500 个
// 标的币）、mmr 0.01、杠杆上限 50。名义口径、维持保证金率、杠杆上限三项全不相同，
// 不可能是同一条阶梯的两种表述。
type familyKey struct {
	instType types.InstType
	family   string
}

// crossTierSizes 汇总每个 (instType, instFamily) 下全仓持仓的张数**绝对值之和**。
//
// 用绝对值相加而非净额，由实测确认：同一合约上开多 7000 张、开空 7000 张，净额为
// 零，而两个仓位的维持保证金率都是 0.015 —— 那是 12500 张一档上限之外的第二档，
// 只有按绝对值合计 14000 张才查得到。
//
// 挂单不计入查档张数。OKX 确实把挂单算进了币种级的 imr 与 mmr，但当时持仓本身
// 已在第二档，挂单是否影响档位无从分辨，故只按实测所及的部分建模。
// 见 docs/okx-rules.md §13。
func (s *Simulator) crossTierSizes() (map[familyKey]decimal.Decimal, error) {
	out := make(map[familyKey]decimal.Decimal)
	for _, p := range s.pos {
		if p.MgnMode != types.MgnCross {
			continue
		}
		inst, err := s.cfg.RefData.Instrument(p.InstID)
		if err != nil {
			return nil, err
		}
		k := familyKey{inst.InstType, inst.InstFamily}
		out[k] = out[k].Add(p.AbsPos())
	}
	// 开仓挂单按【绝对值】一并计入，与持仓同一口径——实测确证。
	//
	// 构造了一个跨档 straddle 才分辨出来：持仓 1900 张落在一档 [0,2000]，
	// 挂单 1000 张，合并 2900 张跨进二档。币种级 mmr 实测 0.0183630772971576，
	// 与「合并 2900 张查到二档、整体按标记价算」差 1.16e-7（两次读数间的价格漂移），
	// 而「持仓一档 + 挂单各自查档」差 2.4e-3 到 4.3e-3，差了四个量级。
	//
	// 方向不相抵：反方向的开仓挂单同样计入（残差 1.63e-7）。平仓挂单不计入
	// （残差 3.66e-8）——它让持仓变小，不占额度。
	//
	// ⚠️ 仓位自身返回的 `mmr` 字段只报它【自己那一档】的数（实测持仓在一档时
	// 恒为 0.004 率），那是显示口径，不是风控口径。拿它去反推档位会得出相反的结论。
	for _, o := range s.pending {
		if o.Order.TdMode != types.TdCross || !o.Cost.OpenSz.IsPositive() {
			continue
		}
		inst, err := s.cfg.RefData.Instrument(o.Order.InstID)
		if err != nil {
			return nil, err
		}
		k := familyKey{inst.InstType, inst.InstFamily}
		out[k] = out[k].Add(o.Cost.OpenSz.Abs())
	}
	return out, nil
}

// tierOf 返回某个仓位查档所得的档位。
//
// 两种模式的查档口径不同，且都经实测确认：
//
//	逐仓  每个仓位单独查。同一 instId 的多空两个仓位也各查各的——实测 FIL-USDT
//	      多空各 50 万张，合计 100 万本应落在第三档（mmr 0.03），实际两个仓位
//	      都是 0.015，正是各自按 50 万张单独查档的结果。
//	全仓  同一 (instType, instFamily) 下所有仓位合并后查一次，结果对该家族的
//	      每个仓位一体适用。实测两个交割期各持 7000 张，先开的那条腿一张未动，
//	      仅因另一期开了仓，其维持保证金率就从 0.01 跳到 0.015。
func (s *Simulator) tierOf(pos Position, inst refdata.Instrument) (refdata.PositionTier, error) {
	var sizes map[familyKey]decimal.Decimal
	if pos.MgnMode == types.MgnCross {
		var err error
		if sizes, err = s.crossTierSizes(); err != nil {
			return refdata.PositionTier{}, err
		}
	}
	return s.tierWith(pos, inst, sizes)
}

// tierWith 与 tierOf 相同，但复用调用方已经算好的合并张数。
//
// 分出这个版本是为了性能：合并张数要遍历全部仓位，而在一个已经在遍历仓位的循环里
// 逐个调用 tierOf，复杂度就是仓位数的平方。实测四个全仓仓位时，crossTierSizes 占
// 一次 Fill 的 13%，而它算出来的东西每次都一样。
func (s *Simulator) tierWith(pos Position, inst refdata.Instrument,
	sizes map[familyKey]decimal.Decimal) (refdata.PositionTier, error) {

	tbl, err := refdata.TierTableFor(s.cfg.RefData, inst, pos.MgnMode)
	if err != nil {
		return refdata.PositionTier{}, err
	}
	sz := pos.AbsPos()
	if pos.MgnMode == types.MgnCross && sizes != nil {
		if merged, ok := sizes[familyKey{inst.InstType, inst.InstFamily}]; ok {
			sz = merged
		}
	}
	return tbl.Lookup(sz)
}

// CrossMetrics 是一个结算币种下全仓整体的风险指标。
//
// 全仓的风险是**币种级**的，不是仓位级的：仓位不划走保证金，亏损直接吃现金余额，
// 因此同一结算币下的所有全仓仓位共担一份权益。OKX 在仓位级返回的 mgnRatio 与 liqPx
// 就是这个币种级的值原样重复到每个仓位上，而 margin 一律为空串——那笔钱根本没划过。
//
// acctLv=2 现货合约模式下账户级的 adjEq / availEq / imr / mmr / mgnRatio 实测
// 恒为空串——风险按结算币种分别核算，不做全账户折算。跨币种折算是 acctLv=3 的
// 事，明确排除在 v1.0 之外。
type CrossMetrics struct {
	Ccy     string          // 结算币种
	CashBal decimal.Decimal // 现金余额
	Upl     decimal.Decimal // 全仓仓位的未实现盈亏合计
	// Equity 是参与保证金率的权益：现金 + 全仓浮盈 − 挂单冻结的开仓手续费。
	//
	// ⚠️ 最后一项容易漏。实测：eq −(availBal + imr) 恰好等于挂单按【委托价】算的
	// 名义 × 吃单费率，三个差异极大的委托价（现价的 1.05/1.50/2.00 倍）下逐位吻合，
	// 残差 1e-17。漏掉它会让含挂单时的保证金率偏大——也就是偏乐观。
	Equity decimal.Decimal

	// OrderFrozenFee 是挂单冻结的【开仓】手续费合计，已从 Equity 中扣除。
	//
	// 名字要带 Frozen：本结构里已有一个 CloseFee，而那一项**也包含挂单的那份**
	// （平仓手续费）。只叫 OrderFee 的话，两个字段并列时分不清哪个是开仓、
	// 哪个是平仓。Frozen 同时对应 OKX 的 ordFrozen 口径。
	OrderFrozenFee decimal.Decimal

	IMR decimal.Decimal // 初始保证金合计，含挂单占用
	MMR decimal.Decimal // 维持保证金合计，含挂单占用

	// CloseFee 是平仓手续费合计，**开仓挂单也算一份**。
	//
	// 「尚未成交的委托没有可平的仓位，也就没有平仓手续费」听着合理，实测是错的：
	// OKX 在风险计算上把开仓挂单整个当仓位看待，维持保证金与平仓手续费都收。
	// 把挂单排除在外会让分母小一成以上——实测持仓 100 张、挂单 1900 张时，
	// 排除挂单算出的保证金率比真实值高 12%。
	CloseFee decimal.Decimal
	MgnRatio decimal.Decimal // 保证金率，≤ 1 触发全仓强平
	LiqPx    decimal.Decimal // 预估强平价；无定义时为零，判据见 crossLiquidationPx

	// HasPosition 报告该币种下是否真的有全仓持仓。
	//
	// 与 Metrics.HasPosition 同理：权益耗尽时保证金率也是零，而那正是最该强平的
	// 时刻，不能用保证金率是否为零来代替这个判断。
	HasPosition bool
}

// IsLiquidatable 报告该币种的全仓整体是否已触及强平线。
func (m CrossMetrics) IsLiquidatable() bool {
	// 精确比较，不走 MgnRatio。规则是「保证金率 ≤ 1」，而分母恒为正，
	// 于是它与「权益 ≤ 分母」等价且少一次 20 位舍入。与 isolatedIsLiquidatable
	// 同口径，见那里的说明。
	return m.HasPosition && m.Equity.Cmp(m.MMR.Add(m.CloseFee)) <= 0
}

// CrossMetricsOf 计算某结算币种下全仓整体的风险指标。
//
// 各项关系经模拟盘标定，10 个快照横跨两个合约、含多仓位与挂单，最大差 1.6e-13：
//
//	imr       = Σ仓位(名义/杠杆, 标记价) + Σ挂单(名义/杠杆, 委托价)
//	mmr       = Σ仓位(名义 × 档位率) + Σ挂单(名义 × 档位率)，两者都按标记价
//	mgnRatio  = (现金余额 + 全仓浮盈) / (mmr + Σ仓位平仓手续费)
//
// 注意冻结与风险两侧取价不同：挂单的保证金按**委托价**（那是真要掏的钱），
// 而它的维持保证金按**标记价**（那是它此刻值多少）。实测中挂单的 imr 贡献
// 62.625 = 5000×0.2505/20 用委托价，mmr 贡献 26.85 = 5000×0.358×0.015 用标记价。
func (s *Simulator) CrossMetricsOf(ccy string) (CrossMetrics, error) {
	return s.crossMetrics(ccy, true, true)
}

// crossMetrics 是 CrossMetricsOf 的本体，full 为 false 时跳过强平检查用不上的量。
//
// 用一个开关而不是抄一份精简版，是因为**判据与对外报出的保证金率必须永远一致**。
// 抄两份的话，哪天有人改了其中一份的口径，模拟器就会出现「保证金率显示没事、
// 却被强平了」这种对不上的事，而且不会有任何报错。
//
// 两个开关只允许省掉**判据读不到的输出**：初始保证金与全仓强平价。任何流向
// MgnRatio 的计算都不得放进开关后面。分成两个而不是一个 full，是因为三个调用方
// 要的东西各不相同——强平判据两样都不要，BalanceOf 要 imr 但不要强平价。
func (s *Simulator) crossMetrics(ccy string, withIMR, withLiqPx bool) (CrossMetrics, error) {
	m := CrossMetrics{Ccy: ccy, CashBal: s.cash[ccy]}

	// 没有全仓敞口时，跳过尾部那几次 decimal 加减直接返回。
	//
	// 这不是为了少走两个空循环——是为了躲开那几次跨指数的加减。零值的指数是 0，
	// 而现金余额经 div 之后指数常是 -20，两者相加要先算 10^20 对齐；这笔钱花在
	// 「确认没有全仓仓位」上，而逐仓策略每个子步都要付一次。
	//
	// 上游网格引擎实测：18 个月 1m、286 万个子步、零全仓仓位的回测里，
	// crossMetrics 占了全程 10.34% CPU。
	//
	// 判断不另起一趟遍历——那样有敞口时要多扫一遍 s.pos，实测反而慢 5%。
	// 改成边扫边记，并把档位表推迟到第一次真的用上时才建。
	var sizes map[familyKey]decimal.Decimal
	any := false

	for _, p := range s.pos {
		if p.MgnMode != types.MgnCross {
			continue
		}
		inst, err := s.cfg.RefData.Instrument(p.InstID)
		if err != nil {
			return CrossMetrics{}, err
		}
		if inst.SettleCcy != ccy {
			continue
		}
		if sizes == nil {
			var err error
			if sizes, err = s.crossTierSizes(); err != nil {
				return CrossMetrics{}, err
			}
		}
		tier, err := s.tierWith(p, inst, sizes)
		if err != nil {
			return CrossMetrics{}, err
		}
		any = true
		rate, err := s.fees.Rate(inst)
		if err != nil {
			return CrossMetrics{}, err
		}
		markPx := s.markOf(p.InstID, p.AvgPx)
		nom := notional(inst, p.AbsPos(), markPx)

		m.HasPosition = true
		m.Upl = m.Upl.Add(unrealizedPnl(inst, p.SignedPos(), p.AvgPx, markPx))
		if withIMR {
			m.IMR = m.IMR.Add(div(nom, p.Lever))
		}
		m.MMR = m.MMR.Add(nom.Mul(tier.MMR))
		m.CloseFee = m.CloseFee.Add(nom.Mul(rate.Taker.Abs()))
	}

	// 开仓挂单在风险计算上被整个当仓位看待：初始保证金、维持保证金、平仓手续费
	// 三项都收。这一条与直觉相反——「尚未成交的委托没有可平的仓位，也就没有平仓
	// 手续费」听着合理，实测是错的。
	//
	// 另外它冻结的【开仓】手续费要从权益里扣掉，见 CrossMetrics.Equity。
	for _, o := range s.pending {
		if o.Cost.Ccy != ccy || o.Order.TdMode != types.TdCross {
			continue
		}
		if !o.Cost.OpenSz.IsPositive() {
			continue
		}
		inst, err := s.cfg.RefData.Instrument(o.Order.InstID)
		if err != nil {
			return CrossMetrics{}, err
		}
		if withIMR {
			m.IMR = m.IMR.Add(o.Cost.Margin)
		}
		// 挂单冻结的开仓手续费要从权益里扣掉，实测确证，见 CrossMetrics.Equity
		m.OrderFrozenFee = m.OrderFrozenFee.Add(o.Cost.Fee)
		any = true

		tier, err := s.pendingTier(o, inst)
		if err != nil {
			return CrossMetrics{}, err
		}
		markPx := s.markOf(o.Order.InstID, o.Order.Px)
		nom := notional(inst, o.Cost.OpenSz, markPx)
		m.MMR = m.MMR.Add(nom.Mul(tier.MMR))
		// 开仓挂单同样计一份平仓手续费——实测如此，见 CrossMetrics.CloseFee
		rate, err := s.fees.Rate(inst)
		if err != nil {
			return CrossMetrics{}, err
		}
		m.CloseFee = m.CloseFee.Add(nom.Mul(rate.Taker.Abs()))
	}

	if !any {
		// 等价性：无敞口时 Upl / OrderFrozenFee / MMR / CloseFee 全是零，
		// Equity = CashBal.Add(0).Sub(0) 与 CashBal 同值同指数；den 为零故
		// MgnRatio 不赋值，与此处留零一致；crossLiquidationPx 无仓位时也返回零。
		m.Equity = m.CashBal
		return m, nil
	}
	m.Equity = m.CashBal.Add(m.Upl).Sub(m.OrderFrozenFee)
	if den := m.MMR.Add(m.CloseFee); !den.IsZero() {
		m.MgnRatio = div(m.Equity, den)
	}

	if !withLiqPx {
		return m, nil
	}

	liq, err := s.crossLiquidationPx(ccy)
	if err != nil {
		return CrossMetrics{}, err
	}
	m.LiqPx = liq
	return m, nil
}

// crossLiquidationPx 计算某结算币种的全仓预估强平价。
//
// 由「币种全仓权益恰好等于维持保证金加平仓手续费」解出。全仓权益随价格线性变化，
// 维持保证金与平仓费也是，故在档位不变的前提下有闭式解：
//
//	P = (Σ sᵢ·Qᵢ·avgPxᵢ − 现金余额) / (Σ sᵢ·Qᵢ − Σ Qᵢ·(mmr率 + taker))
//
// 其中 sᵢ 多头取 +1、空头取 −1。这正是逐仓强平价公式的推广：单个仓位、把现金余额
// 换成该仓位的保证金，即退化为 liquidationPx 的形态。
//
// 与逐仓的一处实测差异：**全仓的强平价没有那一个 tickSz 的安全缓冲**。三个样本
// 与本式的差都在 1e-15 量级，加上缓冲反而会差整整一个 tick。
//
// 三种情形下返回零，均由实测确定（13 个样本无一例外）：
//
//	同币种下有多个合约  两个交割期的标记价并不同步，单一强平价无从定义。实测两个
//	                    交割期各持 7000 张时 OKX 返回空串，而本式仍能解出 0.1449
//	                    这个看似合理的数——正因为看似合理，才更不能给
//	解为负              现金相对仓位太厚，价格到不了那里，OKX 同样返回空串
//
// 反向合约走倒数对偶的形态，见 crossInverseLiquidationPx。
//
// 挂单不参与计算：末两个快照上挂着一笔 5000 张的委托，OKX 给出的强平价与没挂单时
// 逐位相同。
func (s *Simulator) crossLiquidationPx(ccy string) (decimal.Decimal, error) {
	var num, den decimal.Decimal
	only := ""

	inverse := false
	for _, p := range s.pos {
		if p.MgnMode != types.MgnCross {
			continue
		}
		inst, err := s.cfg.RefData.Instrument(p.InstID)
		if err != nil {
			return decimal.Zero, err
		}
		if inst.SettleCcy != ccy {
			continue
		}
		if only == "" {
			only, inverse = p.InstID, inst.IsInverse()
		} else if only != p.InstID {
			return decimal.Zero, nil
		}

		tier, err := s.tierOf(p, inst)
		if err != nil {
			return decimal.Zero, err
		}
		rate, err := s.fees.Rate(inst)
		if err != nil {
			return decimal.Zero, err
		}
		qty := inst.ContractQty(p.AbsPos())
		signed := qty
		if p.SignedPos().IsNegative() || p.PosSide == types.PosShort {
			signed = qty.Neg()
		}
		r := tier.MMR.Add(rate.Taker.Abs())
		if inverse {
			// 反向：分子累加 Σ sQ + Σ Q·r，分母累加 Σ sQ/avgPx
			num = num.Add(signed).Add(qty.Mul(r))
			if !p.AvgPx.IsPositive() {
				return decimal.Zero, nil
			}
			den = den.Add(div(signed, p.AvgPx))
			continue
		}
		num = num.Add(signed.Mul(p.AvgPx))
		den = den.Sub(qty.Mul(r)).Add(signed)
	}
	if only == "" {
		return decimal.Zero, nil
	}

	// 反向合约是倒数对偶的形态，由同一条「权益 = 维持保证金 + 平仓费」解出：
	//
	//	正向  P = (Σ sQ·avgPx − 现金) / (Σ sQ − Σ Q·r)
	//	反向  P = (Σ sQ + Σ Q·r) / (现金 + Σ sQ/avgPx)
	//
	// 实测 BTC-USD-SWAP 全仓多头，本式与 OKX 差 6.3e-13，且同样没有 tickSz 缓冲。
	var px decimal.Decimal
	if inverse {
		d := s.cash[ccy].Add(den)
		if !d.IsPositive() {
			return decimal.Zero, nil
		}
		px = div(num, d)
	} else {
		if den.IsZero() {
			return decimal.Zero, nil
		}
		px = div(num.Sub(s.cash[ccy]), den)
	}
	if !px.IsPositive() {
		return decimal.Zero, nil
	}
	return px, nil
}

// pendingTier 返回一笔全仓挂单计算维持保证金所用的档位。
//
// 用【现有持仓】合并后的档位，不把该委托自身的张数算进去。实测样本中持仓已在
// 第二档、挂单也按第二档的 0.015 计，无法分辨 OKX 是否把挂单计入了查档张数，
// 故只建模到证据所及之处。见 docs/okx-rules.md §13。
func (s *Simulator) pendingTier(o PendingOrder, inst refdata.Instrument) (refdata.PositionTier, error) {
	tbl, err := refdata.TierTableFor(s.cfg.RefData, inst, types.MgnCross)
	if err != nil {
		return refdata.PositionTier{}, err
	}
	sizes, err := s.crossTierSizes()
	if err != nil {
		return refdata.PositionTier{}, err
	}
	return tbl.Lookup(sizes[familyKey{inst.InstType, inst.InstFamily}])
}

// markOf 返回某合约的当前标记价；未推送过行情时以 fallback 代替。
func (s *Simulator) markOf(instID string, fallback decimal.Decimal) decimal.Decimal {
	if px := s.marks[instID]; !px.IsZero() {
		return px
	}
	return fallback
}

// crossOrderFreeze 把某币种的挂单冻结按保证金模式拆开。
//
// 拆开是必要的：全仓挂单的保证金已经计入币种级 imr，而逐仓挂单的没有；
// 两者若混在一起，可用余额会把全仓挂单的那份重复扣掉。
func (s *Simulator) crossOrderFreeze(ccy string) (isoMargin, crossMargin, fee decimal.Decimal) {
	for _, o := range s.pending {
		if o.Cost.Ccy != ccy {
			continue
		}
		if o.Order.TdMode == types.TdCross {
			crossMargin = crossMargin.Add(o.Cost.Margin)
		} else {
			isoMargin = isoMargin.Add(o.Cost.Margin)
		}
		fee = fee.Add(o.Cost.Fee)
	}
	return isoMargin, crossMargin, fee
}

// availBalance 只算可用余额，不算保证金率、平仓手续费与强平价。
//
// Fill 的资金校验只需要这一个数，而完整的 Balance 会顺带算出一堆它用不上的东西：
// 维持保证金要逐仓位查档、强平价要解方程。实测四个全仓仓位时，Balance 占一次
// Fill 耗时的 52%，其中大部分花在这些用不上的项上。
//
// 关键的一点是**可用余额根本不需要查档**：初始保证金是名义除以杠杆，与档位无关；
// 只有维持保证金才要档位。省掉查档也就省掉了合并张数的那一遍遍历。
func (s *Simulator) availBalance(ccy string) (decimal.Decimal, error) {
	var crossUpl, imr decimal.Decimal
	for _, p := range s.pos {
		if p.MgnMode != types.MgnCross {
			continue
		}
		inst, err := s.cfg.RefData.Instrument(p.InstID)
		if err != nil {
			return decimal.Zero, err
		}
		if inst.SettleCcy != ccy {
			continue
		}
		markPx := s.markOf(p.InstID, p.AvgPx)
		crossUpl = crossUpl.Add(unrealizedPnl(inst, p.SignedPos(), p.AvgPx, markPx))
		imr = imr.Add(div(notional(inst, p.AbsPos(), markPx), p.Lever))
	}
	isoOrdMargin, crossOrdMargin, ordFee := s.crossOrderFreeze(ccy)
	imr = imr.Add(crossOrdMargin)
	return s.cash[ccy].Add(crossUpl).Sub(imr).Sub(isoOrdMargin).Sub(ordFee), nil
}

// maxPosAtLever 返回某个方向上「持仓 + 同方向挂单 + 本单」的合计上限。
//
// 档位表的杠杆上限逐档递减，因此选定杠杆也就选定了持仓量的天花板：能用该杠杆的
// 最高那一档，其 maxSz 即为上限。见 refdata.TierTable.MaxSizeAt。
func (s *Simulator) maxPosAtLever(inst refdata.Instrument, mgnMode types.MgnMode,
	posSide types.PosSide) (decimal.Decimal, error) {

	tbl, err := refdata.TierTableFor(s.cfg.RefData, inst, mgnMode)
	if err != nil {
		return decimal.Zero, err
	}
	return tbl.MaxSizeAt(s.Leverage(inst.InstID, mgnMode, posSide)), nil
}

// sameSideExposure 汇总某方向上已经占掉的额度：现有持仓，加上同方向的开仓挂单。
//
// 挂单要算进去是实测确定的——OKX 的 51004 报文里把它写得很明白：
// 「the sum of current order size, position quantity in the same direction,
// and pending orders in the same direction」。实测持仓 600 张、同方向挂单 300 张时
// 再挂 200 张（合计 1100 > 1000）被拒，改挂 100 张（恰好 1000）通过。
//
// 只算开仓方向的挂单：平仓方向的委托会让持仓变小，不占额度。
func (s *Simulator) sameSideExposure(instID string, posSide types.PosSide,
	side types.Side) decimal.Decimal {

	var out decimal.Decimal
	if p, ok := s.pos[positionKey{instID, posSide}]; ok {
		out = p.AbsPos()
	}
	for _, o := range s.pending {
		if o.Order.InstID != instID || o.Order.PosSide != posSide {
			continue
		}
		if o.Order.Side != side {
			continue
		}
		out = out.Add(o.Cost.OpenSz)
	}
	return out
}

// checkPosLimitAtLever 校验一笔新的开仓量会不会超出当前杠杆下的最大持仓量。
//
// 不校验会让模拟器走到一个 OKX 根本不允许存在的状态：比如一档顶格杠杆开满之后
// 再加一张，仓位落进二档、按二档的维持保证金率计算，而二档的杠杆上限根本容不下
// 这个杠杆。那样算出来的风险指标看着正常，实盘却下不出这一单。
func (s *Simulator) checkPosLimitAtLever(inst refdata.Instrument, mgnMode types.MgnMode,
	posSide types.PosSide, side types.Side, openSz decimal.Decimal) error {

	if !openSz.IsPositive() {
		return nil
	}
	limit, err := s.maxPosAtLever(inst, mgnMode, posSide)
	if err != nil {
		return err
	}
	if !limit.IsPositive() {
		return nil
	}
	have := s.sameSideExposure(inst.InstID, posSide, side)
	if total := have.Add(openSz); total.GreaterThan(limit) {
		return okxerr.New(okxerr.CodeExceedsMaxPosAtLever,
			"%s %s：持仓 %s + 同方向挂单与本单 %s = %s 张，超过 %s 倍杠杆下的最大持仓量 %s 张"+
				"——请降低杠杆或减小数量",
			inst.InstID, posSide, have, openSz, total,
			s.Leverage(inst.InstID, mgnMode, posSide), limit)
	}
	return nil
}

// openSideOf 返回某个持仓方向上「开仓」对应的买卖方向。
//
// 买卖模式（net）下没有固定答案——同一个净仓位既可能是多头也可能是空头，
// 此处按当前持仓的符号判断，没有持仓时取买入。
func openSideOf(posSide types.PosSide) types.Side {
	if posSide == types.PosShort {
		return types.Sell
	}
	return types.Buy
}

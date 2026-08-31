package okxsim

import (
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
// 见 docs/okx-rules.md §11。
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
	tbl, err := refdata.TierTableFor(s.cfg.RefData, inst, pos.MgnMode)
	if err != nil {
		return refdata.PositionTier{}, err
	}
	sz := pos.AbsPos()
	if pos.MgnMode == types.MgnCross {
		sizes, err := s.crossTierSizes()
		if err != nil {
			return refdata.PositionTier{}, err
		}
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
	Ccy      string          // 结算币种
	CashBal  decimal.Decimal // 现金余额
	Upl      decimal.Decimal // 全仓仓位的未实现盈亏合计
	Equity   decimal.Decimal // 全仓权益 = CashBal + Upl
	IMR      decimal.Decimal // 初始保证金合计，含挂单占用
	MMR      decimal.Decimal // 维持保证金合计，含挂单占用
	CloseFee decimal.Decimal // 全仓仓位的平仓手续费合计，参与保证金率的分母
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
	return m.HasPosition && m.MgnRatio.LessThanOrEqual(decimal.NewFromInt(1))
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
	m := CrossMetrics{Ccy: ccy, CashBal: s.cash[ccy]}

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
		tier, err := s.tierOf(p, inst)
		if err != nil {
			return CrossMetrics{}, err
		}
		rate, err := s.fees.Rate(inst)
		if err != nil {
			return CrossMetrics{}, err
		}
		markPx := s.markOf(p.InstID, p.AvgPx)
		nom := notional(inst, p.AbsPos(), markPx)

		m.HasPosition = true
		m.Upl = m.Upl.Add(unrealizedPnl(inst, p.SignedPos(), p.AvgPx, markPx))
		m.IMR = m.IMR.Add(div(nom, p.Lever))
		m.MMR = m.MMR.Add(nom.Mul(tier.MMR))
		m.CloseFee = m.CloseFee.Add(nom.Mul(rate.Taker.Abs()))
	}

	// 挂单同样占用初始保证金与维持保证金。它们不进 CloseFee——尚未成交的委托
	// 没有可平的仓位，也就没有平仓手续费。
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
		m.IMR = m.IMR.Add(o.Cost.Margin)

		tier, err := s.pendingTier(o, inst)
		if err != nil {
			return CrossMetrics{}, err
		}
		markPx := s.markOf(o.Order.InstID, o.Order.Px)
		m.MMR = m.MMR.Add(notional(inst, o.Cost.OpenSz, markPx).Mul(tier.MMR))
	}

	m.Equity = m.CashBal.Add(m.Upl)
	if den := m.MMR.Add(m.CloseFee); !den.IsZero() {
		m.MgnRatio = div(m.Equity, den)
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
// 故只建模到证据所及之处。见 docs/okx-rules.md §11。
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

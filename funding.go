package okxsim

import (
	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// Funding 是一次资金费结算的输入。
//
// 费率不由模拟器产生，也不该由它产生——费率是市场结果，回测预测它就和预测价格
// 一样没有意义。
//
// 但这带来一个实打实的限制：OKX 的历史资金费率**只保留约 3 个月**
// （实测四个合约全部截止在同一天，距今 97 天，各约 294 条，是平台级窗口）。
// 超出该窗口的回测取不到真实费率，只能接第三方数据源、用常数近似，
// 或不计资金费——最后一种会系统性高估多头持仓的收益。
//
// 本结算逻辑已按真实账单逐条验证，但它只在调用方能提供费率时才有意义。
//
// 结算周期不由模拟器决定——它随合约而异（实测采样 49 个永续，16 个是 4 小时、
// 33 个是 8 小时），且会调整。由调用方在该结算的那一步给出，比让模拟器自行
// 推算周期更可靠。
type Funding struct {
	// Rate 资金费率，正数表示多头付空头。
	//
	// 各合约的上下限不同（实测有 ±0.01、±0.0075、±0.00375 三档），
	// 钳制由数据源负责，模拟器原样使用给定的费率。
	Rate decimal.Decimal

	// Px 结算价；留空则用本步的标记价。
	Px decimal.Decimal
}

// FundingResult 是一个仓位上的一次资金费结算结果。
type FundingResult struct {
	InstID   string
	PosSide  types.PosSide
	Rate     decimal.Decimal
	Px       decimal.Decimal
	Notional decimal.Decimal
	Amount   decimal.Decimal // 负数表示支付，正数表示收取
	Ts       int64
}

// settleFunding 对某合约上的全部仓位结算一次资金费。
//
// 公式经真实账单核对：资金费 = ctVal × sz × ctMult × 结算价 × 费率。
// 样本 sz=49.18、px=2454.22、rate=0.0001，算得 1.206985396，与账单逐位相同。
//
// 逐仓的资金费从【仓位保证金】里扣，不动现金余额。这一点由账单确证：
// 连续多期结算中 balChg 恒为 0、bal 纹丝不动，而 posBalChg 恰为资金费金额、
// posBal 逐次递减。把它记到现金余额上会让逐仓权益与强平价都算错。
func (s *Simulator) settleFunding(instID string, f Funding, markPx decimal.Decimal,
	ts int64) ([]FundingResult, error) {

	inst, err := s.cfg.RefData.Instrument(instID)
	if err != nil {
		return nil, err
	}
	px := f.Px
	if !px.IsPositive() {
		px = markPx
	}
	if !px.IsPositive() {
		return nil, okxerr.New(okxerr.CodeParamError,
			"%s 结算资金费时没有可用的价格", instID)
	}

	var out []FundingResult
	for _, side := range s.cfg.PosMode.PosSides() {
		key := positionKey{instID, side}
		pos, ok := s.pos[key]
		if !ok || pos.IsEmpty() {
			continue
		}

		nom := notional(inst, pos.AbsPos(), px)
		amt := nom.Mul(f.Rate)
		// 正费率下多头支付、空头收取
		if pos.IsLong() {
			amt = amt.Neg()
		}

		pos.Funding = pos.Funding.Add(amt)
		pos.Margin = pos.Margin.Add(amt)
		if pos.Margin.IsNegative() {
			// 保证金被资金费吃穿。真实情况下强平会先一步触发，
			// 此处夹到零并留待风控处理，不让它变成负数污染后续计算。
			pos.Margin = decimal.Zero
		}
		pos.UTime = ts
		s.pos[key] = pos

		out = append(out, FundingResult{
			InstID: instID, PosSide: side, Rate: f.Rate, Px: px,
			Notional: nom, Amount: amt, Ts: ts,
		})
	}
	return out, nil
}

// SettleFunding 在当前状态下结算一次资金费。
//
// 内置撮合的使用者应当把 Funding 放进 Bar 交给 Advance，让结算与撮合的先后
// 关系保持确定；本方法供不使用内置撮合、自行推进时钟的调用方使用。
func (s *Simulator) SettleFunding(instID string, f Funding, ts int64) ([]FundingResult, error) {
	return s.settleFunding(instID, f, s.marks[instID], ts)
}

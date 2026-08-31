package okxsim

import (
	"encoding/json"
	"strconv"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/shopspring/decimal"
)

// 本文件提供与 OKX REST 响应字段级同构的视图类型。
//
// 它们存在的理由是让「100% 模拟」这句话可被检验：使用者能拿模拟器的输出与真实
// API 的响应做逐字段 diff，已经对接过 OKX 的代码也能少改甚至不改地跑回测。
//
// 序列化沿用 OKX 的线格式——所有数值都是字符串，无值的字段是空串而非零值或
// null。这不是风格取舍：OKX 用空串区分「无此值」与「值为零」，把 imr 的空串
// 写成 "0" 会让下游误以为初始保证金真的是零。
//
// 模拟器拿不到的字段一律留空，与 OKX 表示「无此值」的方式一致。当前留空的有：
// posId、idxPx、last、bePx、uplLastPx、uplRatioLastPx、adl、usdPx、
// notionalUsd、tradeId——它们需要指数价、最新成交价、全市场减仓排名等
// 模拟器职责之外的数据。

// numStr 把数值按 OKX 的线格式输出；zeroEmpty 为真时零值输出空串。
func numStr(d decimal.Decimal, zeroEmpty bool) string {
	if zeroEmpty && d.IsZero() {
		return ""
	}
	return d.String()
}

func millisStr(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// PositionView 是与 OKX GET /api/v5/account/positions 响应字段级同构的仓位视图。
type PositionView struct {
	InstType    string `json:"instType"`
	InstID      string `json:"instId"`
	PosID       string `json:"posId"`
	PosSide     string `json:"posSide"`
	MgnMode     string `json:"mgnMode"`
	Pos         string `json:"pos"`
	AvailPos    string `json:"availPos"`
	PosCcy      string `json:"posCcy"`
	Ccy         string `json:"ccy"`
	AvgPx       string `json:"avgPx"`
	BePx        string `json:"bePx"`
	MarkPx      string `json:"markPx"`
	LiqPx       string `json:"liqPx"`
	IdxPx       string `json:"idxPx"`
	Last        string `json:"last"`
	Lever       string `json:"lever"`
	Margin      string `json:"margin"`
	MgnRatio    string `json:"mgnRatio"`
	Imr         string `json:"imr"`
	Mmr         string `json:"mmr"`
	Upl         string `json:"upl"`
	UplRatio    string `json:"uplRatio"`
	RealizedPnl string `json:"realizedPnl"`
	Pnl         string `json:"pnl"`
	Fee         string `json:"fee"`
	FundingFee  string `json:"fundingFee"`
	LiqPenalty  string `json:"liqPenalty"`
	NotionalUsd string `json:"notionalUsd"`
	Adl         string `json:"adl"`
	UsdPx       string `json:"usdPx"`
	CTime       string `json:"cTime"`
	UTime       string `json:"uTime"`
	TradeID     string `json:"tradeId"`
}

// newPositionView 由仓位与其风险指标装配 OKX 形态的视图。
func newPositionView(p Position, inst refdata.Instrument, m Metrics) PositionView {
	v := PositionView{
		InstType: inst.InstType.String(),
		InstID:   p.InstID,
		PosSide:  p.PosSide.String(),
		MgnMode:  p.MgnMode.String(),
		Pos:      p.Pos.String(),
		AvailPos: p.Pos.String(),
		Ccy:      inst.SettleCcy,
		AvgPx:    p.AvgPx.String(),
		Lever:    p.Lever.String(),
		Margin:   p.Margin.String(),
		// OKX 的 realizedPnl 是净额，不是毛盈亏
		RealizedPnl: p.NetRealizedPnl().String(),
		Fee:         p.Fee.String(),
		FundingFee:  p.Funding.String(),
		CTime:       millisStr(p.CTime),
		UTime:       millisStr(p.UTime),
	}
	// 逐仓的 imr 恒为空串，保证金看 margin —— 与 OKX 一致。
	// 反向合约的 posCcy 是标的币；正向合约恒为空。
	if inst.IsInverse() {
		v.PosCcy = inst.CtValCcy
	}

	v.LiqPenalty = numStr(p.LiqPenalty, true)
	v.MarkPx = numStr(m.MarkPx, true)
	v.LiqPx = numStr(m.LiqPx, true)
	v.MgnRatio = numStr(m.MgnRatio, true)
	v.Mmr = numStr(m.MMR, true)
	v.Upl = numStr(m.UPL, false)
	v.UplRatio = numStr(m.UPLRatio, false)
	return v
}

// BalanceView 是与 OKX GET /api/v5/account/balance 的 details 字段级同构的余额视图。
type BalanceView struct {
	Ccy       string `json:"ccy"`
	Eq        string `json:"eq"`
	CashBal   string `json:"cashBal"`
	AvailBal  string `json:"availBal"`
	AvailEq   string `json:"availEq"`
	FrozenBal string `json:"frozenBal"`
	OrdFrozen string `json:"ordFrozen"`
	IsoEq     string `json:"isoEq"`
	IsoUpl    string `json:"isoUpl"`
	Upl       string `json:"upl"`
	DisEq     string `json:"disEq"`
}

func newBalanceView(b Balance) BalanceView {
	return BalanceView{
		Ccy:       b.Ccy,
		Eq:        b.Eq.String(),
		CashBal:   b.CashBal.String(),
		AvailBal:  b.AvailBal.String(),
		AvailEq:   b.AvailEq.String(),
		FrozenBal: b.FrozenBal.String(),
		OrdFrozen: b.OrdFrozen.String(),
		IsoEq:     b.IsoEq.String(),
		IsoUpl:    b.Upl.String(),
		Upl:       b.Upl.String(),
	}
}

// PositionViews 返回 OKX 形态的仓位列表，可直接与
// GET /api/v5/account/positions 的 data 数组逐字段比对。
func (s *Simulator) PositionViews() ([]PositionView, error) {
	positions := s.Positions()
	out := make([]PositionView, 0, len(positions))
	for _, p := range positions {
		inst, err := s.cfg.RefData.Instrument(p.InstID)
		if err != nil {
			return nil, err
		}
		m, err := s.MetricsOf(p.InstID, p.PosSide)
		if err != nil {
			return nil, err
		}
		out = append(out, newPositionView(p, inst, m))
	}
	return out, nil
}

// BalanceViews 返回 OKX 形态的余额列表，可直接与
// GET /api/v5/account/balance 的 details 数组逐字段比对。
func (s *Simulator) BalanceViews() ([]BalanceView, error) {
	bals, err := s.Balances()
	if err != nil {
		return nil, err
	}
	out := make([]BalanceView, 0, len(bals))
	for _, b := range bals {
		out = append(out, newBalanceView(b))
	}
	return out, nil
}

// MarshalPositions 以 OKX 响应信封的形态输出全部仓位。
//
// 输出可直接与 GET /api/v5/account/positions 的原始响应做文本 diff。
func (s *Simulator) MarshalPositions() ([]byte, error) {
	views, err := s.PositionViews()
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Code string         `json:"code"`
		Msg  string         `json:"msg"`
		Data []PositionView `json:"data"`
	}{Code: refdata.CodeOK, Data: views})
}

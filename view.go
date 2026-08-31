package okxsim

import (
	"strconv"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
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
	InstType       string `json:"instType"`
	InstID         string `json:"instId"`
	PosID          string `json:"posId"`
	PosSide        string `json:"posSide"`
	MgnMode        string `json:"mgnMode"`
	Pos            string `json:"pos"`
	AvailPos       string `json:"availPos"`
	PosCcy         string `json:"posCcy"`
	Ccy            string `json:"ccy"`
	AvgPx          string `json:"avgPx"`
	BePx           string `json:"bePx"`
	MarkPx         string `json:"markPx"`
	LiqPx          string `json:"liqPx"`
	IdxPx          string `json:"idxPx"`
	Last           string `json:"last"`
	Lever          string `json:"lever"`
	Margin         string `json:"margin"`
	MgnRatio       string `json:"mgnRatio"`
	Imr            string `json:"imr"`
	Mmr            string `json:"mmr"`
	Upl            string `json:"upl"`
	UplRatio       string `json:"uplRatio"`
	UplLastPx      string `json:"uplLastPx"`
	UplRatioLastPx string `json:"uplRatioLastPx"`
	RealizedPnl    string `json:"realizedPnl"`
	Pnl            string `json:"pnl"`
	Fee            string `json:"fee"`
	FundingFee     string `json:"fundingFee"`
	LiqPenalty     string `json:"liqPenalty"`
	NotionalUsd    string `json:"notionalUsd"`
	Adl            string `json:"adl"`
	UsdPx          string `json:"usdPx"`
	CTime          string `json:"cTime"`
	UTime          string `json:"uTime"`
	TradeID        string `json:"tradeId"`
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
		// OKX 的 realizedPnl 是净额，不是毛盈亏
		RealizedPnl: p.NetRealizedPnl().String(),
		Fee:         p.Fee.String(),
		FundingFee:  p.Funding.String(),
		CTime:       millisStr(p.CTime),
		UTime:       millisStr(p.UTime),
	}
	// margin 与 imr 恰好互补，实测确认：逐仓给 margin、imr 为空串（保证金已划入
	// 仓位，看 margin 即可）；全仓给 imr、margin 为空串（那笔钱从未离开现金余额）。
	if p.MgnMode == types.MgnCross {
		v.Imr = numStr(m.IMR, true)
	} else {
		v.Margin = p.Margin.String()
	}
	// posCcy 在衍生品上恒为空——正向与反向都是。它只在币币杠杆的仓位上才有值。
	//
	// 早先按「反向合约的 posCcy 是标的币」建模，那是从字段名推来的假设而非实测；
	// v0.9.0 的全字段对拍上，BTC-USD-SWAP 的两个仓位都把它照出来了：本库给 USD，
	// OKX 给空串。

	// 零与空串必须照 OKX 的实际约定来，两者含义不同。实测：仓位的 liqPenalty /
	// pnl / fundingFee 恒为数值（没有就是 "0"），而 imr / liqPx 没有时是空串。
	// 早先把「零」一律抹成空串，字段级对拍一跑就露馅了。
	v.LiqPenalty = p.LiqPenalty.String()
	// OKX 的 pnl 是【平仓订单累计收益额】，即毛盈亏；realizedPnl 才是净额。
	// 实测 realizedPnl(-0.24456) = pnl(0) + fee(-0.24456)，两者恰好是本库的
	// RealizedPnl 与 NetRealizedPnl()。
	v.Pnl = p.RealizedPnl.String()
	v.MarkPx = numStr(m.MarkPx, true)
	v.BePx = numStr(m.BePx, true)
	v.LiqPx = numStr(m.LiqPx, true)
	v.MgnRatio = numStr(m.MgnRatio, true)
	v.Mmr = numStr(m.MMR, true)
	v.Upl = numStr(m.UPL, false)
	v.UplRatio = numStr(m.UPLRatio, false)
	v.UplLastPx = numStr(m.UplLastPx, false)
	v.UplRatioLastPx = numStr(m.UplRatioLastPx, false)
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
	Imr       string `json:"imr"`
	Mmr       string `json:"mmr"`
	MgnRatio  string `json:"mgnRatio"`
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
		IsoUpl:    b.IsoUpl.String(),
		Upl:       b.Upl.String(),
		// imr / mmr 实测恒为数值，没有全仓持仓时是 "0" 而不是空串；
		// mgnRatio 才是没有全仓持仓时返回空串。三者的约定并不一致，照实抄。
		Imr:      b.IMR.String(),
		Mmr:      b.MMR.String(),
		MgnRatio: numStr(b.MgnRatio, true),
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
		v := newPositionView(p, inst, m)
		// 最新价与指数价是行情而非仓位状态，纯函数 newPositionView 拿不到，
		// 在这里补。没推送过就留空——OKX 用空串表示「无此值」，不能拿标记价顶替。
		v.Last = numStr(s.last[p.InstID], true)
		v.IdxPx = numStr(s.index[p.InstID], true)
		out = append(out, v)
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

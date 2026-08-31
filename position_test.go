package okxsim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func d(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	v, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("非法测试数值 %q: %v", s, err)
	}
	return v
}

func eq(t *testing.T, got decimal.Decimal, want, field string) {
	t.Helper()
	if !got.Equal(dec(want)) {
		t.Errorf("%s = %s, 期望 %s", field, got, want)
	}
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// near 在给定容差内比较，用于与 OKX 真实返回值对拍——
// 两边的除法舍入策略未必逐位相同，要求逐位相等会把精度差异误报成公式错误。
func near(t *testing.T, got, want decimal.Decimal, tol, field string) {
	t.Helper()
	diff := got.Sub(want).Abs()
	if diff.GreaterThan(dec(tol)) {
		t.Errorf("%s = %s, 期望 %s, 差值 %s 超出容差 %s", field, got, want, diff, tol)
	}
}

// btcSwap 取内置快照里的 BTC-USDT-SWAP：ctVal=0.01 BTC, ctMult=1, linear。
func btcSwap(t *testing.T) refdata.Instrument {
	t.Helper()
	i, err := refdata.MustEmbedded().Instrument("BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("取合约规格失败: %v", err)
	}
	return i
}

const takerRate = "-0.0005"

func netFill(side types.Side, sz, px string) Fill {
	return Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: side,
		PosSide: types.PosNet, Sz: decimal.RequireFromString(sz),
		Px: decimal.RequireFromString(px), ExecType: types.Taker, Ts: 1000,
	}
}

func emptyNetPos() Position {
	return Position{
		InstID: "BTC-USDT-SWAP", MgnMode: types.MgnIsolated, PosSide: types.PosNet,
		Lever: decimal.NewFromInt(10),
	}
}

func apply(t *testing.T, pos Position, f Fill) FillResult {
	t.Helper()
	return applyFill(pos, f, btcSwap(t), dec(takerRate), types.NetMode)
}

func TestOpenPosition(t *testing.T) {
	r := apply(t, emptyNetPos(), netFill(types.Buy, "10", "70000"))

	eq(t, r.After.Pos, "10", "持仓")
	eq(t, r.After.AvgPx, "70000", "开仓均价")
	eq(t, r.OpenedSz, "10", "开仓张数")
	eq(t, r.ClosedSz, "0", "平仓张数")
	eq(t, r.Pnl, "0", "已实现盈亏")
	if !r.After.IsLong() {
		t.Error("应为多头")
	}
	if r.After.CTime != 1000 {
		t.Errorf("建仓时刻 = %d", r.After.CTime)
	}

	// 手续费 = 名义价值 × 费率 = 0.01×10×1×70000 × -0.0005 = -3.5
	eq(t, r.Fee, "-3.5", "手续费")
}

// TestAddToPositionWeightsAvgPx 加仓按张数加权平均更新均价。
func TestAddToPositionWeightsAvgPx(t *testing.T) {
	pos := apply(t, emptyNetPos(), netFill(types.Buy, "10", "70000")).After
	r := apply(t, pos, netFill(types.Buy, "30", "80000"))

	eq(t, r.After.Pos, "40", "加仓后持仓")
	// (70000×10 + 80000×30) / 40 = 77500
	eq(t, r.After.AvgPx, "77500", "加权后的均价")
	eq(t, r.OpenedSz, "30", "开仓张数")
	eq(t, r.Pnl, "0", "加仓不产生已实现盈亏")
}

// TestPartialCloseKeepsAvgPx 部分平仓只结算盈亏，不改变开仓均价。
//
// 这是 OKX 的实际行为，也是与「重新计算剩余仓位均价」这一常见误解的分界。
func TestPartialCloseKeepsAvgPx(t *testing.T) {
	pos := apply(t, emptyNetPos(), netFill(types.Buy, "40", "70000")).After
	r := apply(t, pos, netFill(types.Sell, "15", "75000"))

	eq(t, r.After.Pos, "25", "平仓后持仓")
	eq(t, r.After.AvgPx, "70000", "部分平仓后均价不应变化")
	eq(t, r.ClosedSz, "15", "平仓张数")
	eq(t, r.OpenedSz, "0", "平仓不产生开仓张数")
	// 盈亏 = 0.01×15×1 × (75000-70000) = 750
	eq(t, r.Pnl, "750", "已实现盈亏")
}

func TestFullCloseResetsAvgPx(t *testing.T) {
	pos := apply(t, emptyNetPos(), netFill(types.Buy, "40", "70000")).After
	r := apply(t, pos, netFill(types.Sell, "40", "75000"))

	eq(t, r.After.Pos, "0", "全平后持仓")
	eq(t, r.After.AvgPx, "0", "全平后均价应清零")
	eq(t, r.ClosedSz, "40", "平仓张数")
	eq(t, r.Pnl, "2000", "已实现盈亏") // 0.01×40×(75000-70000)
	if !r.After.IsEmpty() {
		t.Error("应为空仓")
	}
	if r.Reversed {
		t.Error("恰好平光不应算作反手")
	}
}

// TestReversal 反手：卖出量超过多头持仓时，先平光再反向开空，均价重置为成交价。
//
// 这是买卖模式特有的边界，也是仓位核算最容易出错的地方——常见错误是把整笔
// 成交都当成平仓（盈亏算多了），或都当成开仓（漏掉了已实现盈亏）。
func TestReversal(t *testing.T) {
	pos := apply(t, emptyNetPos(), netFill(types.Buy, "40", "70000")).After
	r := apply(t, pos, netFill(types.Sell, "100", "75000"))

	if !r.Reversed {
		t.Error("应当标记为反手")
	}
	eq(t, r.ClosedSz, "40", "平掉的是原有的 40 张，不是全部 100 张")
	eq(t, r.OpenedSz, "60", "反向开出剩余的 60 张")
	// 盈亏只按平掉的 40 张算：0.01×40×(75000-70000) = 2000
	eq(t, r.Pnl, "2000", "已实现盈亏只计平掉的部分")

	eq(t, r.After.Pos, "-60", "反手后为 60 张空头")
	eq(t, r.After.AvgPx, "75000", "新仓位以成交价为均价")
	if !r.After.IsShort() {
		t.Error("反手后应为空头")
	}
	if r.After.CTime != 1000 {
		t.Errorf("反手应重置建仓时刻，实际 %d", r.After.CTime)
	}

	// 手续费按整笔 100 张计，与开平方向无关
	// 0.01×100×75000 × -0.0005 = -37.5
	eq(t, r.Fee, "-37.5", "手续费按整笔成交计")
}

// TestReversalFromShort 空头方向的反手，与多头对称。
func TestReversalFromShort(t *testing.T) {
	pos := apply(t, emptyNetPos(), netFill(types.Sell, "40", "70000")).After
	eq(t, pos.Pos, "-40", "开空后持仓")

	r := apply(t, pos, netFill(types.Buy, "100", "65000"))

	if !r.Reversed {
		t.Error("应当标记为反手")
	}
	eq(t, r.ClosedSz, "40", "平掉的空头张数")
	eq(t, r.OpenedSz, "60", "反向开出的多头张数")
	// 空头盈亏 = 0.01×40×(70000-65000) = 2000
	eq(t, r.Pnl, "2000", "空头下跌获利")
	eq(t, r.After.Pos, "60", "反手后为 60 张多头")
	eq(t, r.After.AvgPx, "65000", "新仓位均价")
}

func TestShortPositionPnl(t *testing.T) {
	pos := apply(t, emptyNetPos(), netFill(types.Sell, "20", "70000")).After
	r := apply(t, pos, netFill(types.Buy, "20", "72000"))

	// 空头上涨亏损：0.01×20×(70000-72000) = -400
	eq(t, r.Pnl, "-400", "空头亏损")
	eq(t, r.After.Pos, "0", "平光")
}

// TestLongShortModeHasNoReversal 开平仓模式下 Pos 恒为正，方向由 PosSide 给出。
func TestLongShortModeHasNoReversal(t *testing.T) {
	inst := btcSwap(t)
	pos := Position{
		InstID: "BTC-USDT-SWAP", MgnMode: types.MgnIsolated, PosSide: types.PosLong,
		Lever: decimal.NewFromInt(10),
	}

	open := Fill{InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosLong, Sz: dec("40"), Px: dec("70000"),
		ExecType: types.Taker, Ts: 1000}
	r := applyFill(pos, open, inst, dec(takerRate), types.LongShortMode)
	eq(t, r.After.Pos, "40", "开多后 Pos 应为正数")
	if !r.After.IsLong() {
		t.Error("PosSide=long 应判定为多头")
	}

	closeF := open
	closeF.Side = types.Sell
	closeF.Sz = dec("15")
	closeF.Px = dec("75000")
	r = applyFill(r.After, closeF, inst, dec(takerRate), types.LongShortMode)
	eq(t, r.After.Pos, "25", "平多后 Pos 仍为正数")
	eq(t, r.Pnl, "750", "平多盈亏")

	// 空头仓位：Pos 为正，SignedPos 为负
	sp := Position{InstID: "BTC-USDT-SWAP", MgnMode: types.MgnIsolated,
		PosSide: types.PosShort, Lever: decimal.NewFromInt(10)}
	openShort := Fill{InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Sell,
		PosSide: types.PosShort, Sz: dec("30"), Px: dec("70000"),
		ExecType: types.Taker, Ts: 1000}
	r = applyFill(sp, openShort, inst, dec(takerRate), types.LongShortMode)
	eq(t, r.After.Pos, "30", "开空后 Pos 应为正数")
	eq(t, r.After.SignedPos(), "-30", "SignedPos 应为负数")
	if !r.After.IsShort() {
		t.Error("PosSide=short 应判定为空头")
	}
}

func TestMakerFeeUsesMakerRate(t *testing.T) {
	f := netFill(types.Buy, "10", "70000")
	f.ExecType = types.Maker
	r := applyFill(emptyNetPos(), f, btcSwap(t), dec("-0.0002"), types.NetMode)

	// 0.01×10×70000 × -0.0002 = -1.4
	eq(t, r.Fee, "-1.4", "挂单手续费")
}

func TestCumulativeTotals(t *testing.T) {
	pos := apply(t, emptyNetPos(), netFill(types.Buy, "10", "70000")).After
	pos = apply(t, pos, netFill(types.Sell, "10", "72000")).After

	eq(t, pos.RealizedPnl, "200", "累计已实现盈亏") // 0.01×10×2000
	// 两笔手续费：0.01×10×70000×-0.0005 = -3.5；0.01×10×72000×-0.0005 = -3.6
	eq(t, pos.Fee, "-7.1", "累计手续费")
}

// ---- 与真实成交对拍 ----

type conformanceFixture struct {
	Instrument       json.RawMessage `json:"instrument"`
	PositionBefore   map[string]any  `json:"position_before"`
	CloseOrderDetail map[string]any  `json:"close_order_detail"`
}

func loadFixture(t *testing.T, name string) conformanceFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", name))
	if err != nil {
		t.Fatalf("读取对拍夹具失败: %v", err)
	}
	var f conformanceFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("解析对拍夹具失败: %v", err)
	}
	return f
}

// TestAgainstRealClose 用 OKX 模拟盘上一笔真实的平仓成交复算，逐项与真实返回比对。
//
// 这条测试的价值在于它不是拿我自己推导的期望值去验我自己的实现——两边都错的话
// 手写单测发现不了。夹具里的数字全部来自 OKX 的实际返回。
func TestAgainstRealClose(t *testing.T) {
	fx := loadFixture(t, "close-long-isolated-linear.json")

	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatalf("解析合约规格失败: %v", err)
	}
	if inst.InstID != "ETH-USDT-SWAP" {
		t.Fatalf("夹具合约 = %s", inst.InstID)
	}

	str := func(m map[string]any, k string) string {
		v, ok := m[k].(string)
		if !ok {
			t.Fatalf("夹具缺少字段 %s", k)
		}
		return v
	}

	openAvgPx := str(fx.PositionBefore, "avgPx")
	posSz := str(fx.PositionBefore, "pos")
	closePx := str(fx.CloseOrderDetail, "avgPx")
	closeSz := str(fx.CloseOrderDetail, "accFillSz") // 注意是累计成交量，不是 fillSz
	realPnl := str(fx.CloseOrderDetail, "pnl")
	realFee := str(fx.CloseOrderDetail, "fee")

	// 重建平仓前的仓位
	pos := Position{
		InstID: inst.InstID, MgnMode: types.MgnIsolated, PosSide: types.PosLong,
		Pos: dec(posSz), AvgPx: dec(openAvgPx), Lever: dec(str(fx.PositionBefore, "lever")),
	}

	f := Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Sell,
		PosSide: types.PosLong, Sz: dec(closeSz), Px: dec(closePx),
		ExecType: types.Taker, Ts: 1,
	}
	r := applyFill(pos, f, inst, dec(takerRate), types.LongShortMode)

	t.Logf("持仓 %s 张，开仓均价 %s，平仓均价 %s", posSz, openAvgPx, closePx)

	// 差值容许到 1e-9：OKX 与本实现的除法舍入策略未必逐位相同，
	// 要求逐位相等会把精度差异误报成公式错误。实测差值在 1e-15 量级。
	near(t, r.Pnl, dec(realPnl), "1e-9", "已实现盈亏")
	near(t, r.Fee, dec(realFee), "1e-9", "手续费")

	t.Logf("已实现盈亏 本实现=%s OKX=%s 差值=%s", r.Pnl, realPnl, r.Pnl.Sub(dec(realPnl)))
	t.Logf("手续费     本实现=%s OKX=%s 差值=%s", r.Fee, realFee, r.Fee.Sub(dec(realFee)))

	eq(t, r.After.Pos, "0", "全平后持仓")
	eq(t, r.ClosedSz, closeSz, "平仓张数")
	if r.Reversed {
		t.Error("全平不应算作反手")
	}
}

// TestFillSzVsAccFillSz 守卫一个字段语义陷阱：OKX 订单的 fillSz 是最新一笔成交的
// 数量，accFillSz 才是累计成交量。拿 fillSz 当成交量会让仓位核算严重出错。
func TestFillSzVsAccFillSz(t *testing.T) {
	fx := loadFixture(t, "close-long-isolated-linear.json")

	fillSz, _ := fx.CloseOrderDetail["fillSz"].(string)
	accFillSz, _ := fx.CloseOrderDetail["accFillSz"].(string)
	sz, _ := fx.CloseOrderDetail["sz"].(string)

	if fillSz == accFillSz {
		t.Skip("该夹具的订单只有一笔成交，两字段恰好相同，无法体现差异")
	}
	if accFillSz != sz {
		t.Errorf("accFillSz(%s) 应等于全部成交后的 sz(%s)", accFillSz, sz)
	}
	t.Logf("字段陷阱确认: fillSz=%s（最新一笔） accFillSz=%s（累计）", fillSz, accFillSz)
}

// TestNetRealizedPnlMatchesOKXSemantics OKX 的 realizedPnl 是净额，
// 不是毛已实现盈亏。
//
// 该式由真实数据确证：一个持有七周的仓位，pnl=182.04988519038076、
// fee=-10.066836395、fundingFee=-75.53442700159925、liqPenalty=0，
// 四者相加恰为 OKX 给出的 realizedPnl=96.44862179378151，差值为 0。
//
// 字段名容易误导——单看 realizedPnl 会以为是毛盈亏。把毛额填进去，
// 长期持仓的对账会差出全部的手续费与资金费。
func TestNetRealizedPnlMatchesOKXSemantics(t *testing.T) {
	fx := loadFixture(t, "close-long-isolated-linear.json")
	p := fx.PositionBefore
	str := func(k string) string {
		v, _ := p[k].(string)
		return v
	}
	if str("realizedPnl") == "" {
		t.Skip("夹具里没有 realizedPnl")
	}

	pos := Position{
		RealizedPnl: dec(str("pnl")),
		Fee:         dec(str("fee")),
		Funding:     dec(str("fundingFee")),
		LiqPenalty:  dec(str("liqPenalty")),
	}
	near(t, pos.NetRealizedPnl(), dec(str("realizedPnl")), "1e-9", "净已实现盈亏")

	// 毛额与净额必须明显不同，否则这条测试没有区分力
	if pos.RealizedPnl.Sub(pos.NetRealizedPnl()).Abs().LessThan(dec("1")) {
		t.Fatal("该样本的毛额与净额过于接近，无法区分两种口径")
	}
	t.Logf("毛 %s，净 %s（OKX %s）", pos.RealizedPnl, pos.NetRealizedPnl(), str("realizedPnl"))
}

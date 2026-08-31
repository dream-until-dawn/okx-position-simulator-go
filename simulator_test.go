package okxsim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func newSim(t *testing.T, posMode types.PosMode) *Simulator {
	t.Helper()
	s, err := New(Config{
		PosMode:      posMode,
		RefData:      refdata.MustEmbedded(),
		DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("新建模拟器失败: %v", err)
	}
	if err := s.Deposit("USDT", dec("10000")); err != nil {
		t.Fatalf("入金失败: %v", err)
	}
	return s
}

func mustFill(t *testing.T, s *Simulator, f Fill) FillResult {
	t.Helper()
	r, err := s.Fill(f)
	if err != nil {
		t.Fatalf("成交失败: %v", err)
	}
	return r
}

func TestSimulatorOpenAndClose(t *testing.T) {
	s := newSim(t, types.NetMode)

	r := mustFill(t, s, netFill(types.Buy, "4", "78088.1605"))
	// 名义价值 0.01×4×78088.1605 = 3123.52642，5 倍杠杆 -> 保证金 624.705284
	eq(t, r.After.Margin, "624.705284", "开仓保证金")
	// 现金减少「保证金 + 手续费」：10000 − 624.705284 − 1.56176321
	eq(t, s.CashBal("USDT"), "9373.73295279", "开仓后现金")

	r = mustFill(t, s, netFill(types.Sell, "4", "78090.5"))
	eq(t, r.After.Margin, "0", "全平后保证金应归零")
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); ok {
		t.Error("全平后仓位应被移除")
	}
	// 全程净变化 = 盈亏 + 手续费
	pnl := r.Pnl
	fee := dec("-1.56176321").Add(r.Fee)
	eq(t, s.CashBal("USDT"), dec("10000").Add(pnl).Add(fee).String(), "全平后现金")
}

// ---- 与真实账户的资金流转对拍 ----

type cashflowFixture struct {
	Steps []struct {
		Label    string         `json:"label"`
		Balance  map[string]any `json:"balance"`
		Position map[string]any `json:"position"`
		Order    map[string]any `json:"order"`
	} `json:"steps"`
}

func loadCashflow(t *testing.T) cashflowFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "isolated-cashflow.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var f cashflowFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	return f
}

// TestReplayRealCashflow 把真实账户上的一串操作原样重放，逐步比对余额与保证金。
//
// 序列为「开 4 张 -> 平 1 张 -> 平 3 张」，每一步的期望值都取自 OKX 的实际返回。
// 这条测试覆盖了资金流转的全部关键点：开仓扣保证金与手续费、部分平仓按张数比例
// 释放保证金、平仓结算盈亏，以及全程守恒。
//
// 注：夹具采自模拟盘，而内置快照取自生产环境。本测试只依赖 ctVal 与 ctMult，
// 这两项在两个环境中相同；tickSz 等有差异的字段不参与资金流转的计算。
func TestReplayRealCashflow(t *testing.T) {
	fx := loadCashflow(t)

	s, err := New(Config{
		PosMode:      types.LongShortMode,
		RefData:      refdata.MustEmbedded(),
		DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("新建模拟器失败: %v", err)
	}

	str := func(m map[string]any, k string) string {
		v, _ := m[k].(string)
		return v
	}

	var started bool
	for _, st := range fx.Steps {
		switch {
		case st.Order != nil:
			side := types.Side(str(st.Order, "side"))
			f := Fill{
				InstID: str(st.Order, "instId"), TdMode: types.TdIsolated,
				Side: side, PosSide: types.PosSide(str(st.Order, "posSide")),
				Sz: dec(str(st.Order, "accFillSz")), Px: dec(str(st.Order, "avgPx")),
				ExecType: types.Taker, Ts: 1,
			}
			r := mustFill(t, s, f)

			// 逐笔核对盈亏与手续费
			near(t, r.Fee, dec(str(st.Order, "fee")), "1e-9",
				st.Label+" 手续费")
			near(t, r.Pnl, dec(str(st.Order, "pnl")), "1e-9",
				st.Label+" 已实现盈亏")

		case st.Balance != nil:
			cash := dec(str(st.Balance, "cashBal"))
			if !started {
				// 用真实的起始余额初始化
				if err := s.Deposit("USDT", cash); err != nil {
					t.Fatalf("入金失败: %v", err)
				}
				started = true
				continue
			}
			near(t, s.CashBal("USDT"), cash, "1e-9", st.Label+" 现金余额")

			if len(st.Position) > 0 {
				pos, ok := s.PositionOf(str(st.Position, "instId"),
					types.PosSide(str(st.Position, "posSide")))
				if !ok {
					t.Fatalf("%s: 模拟器里没有对应仓位", st.Label)
				}
				near(t, pos.Pos, dec(str(st.Position, "pos")), "1e-9", st.Label+" 持仓")
				near(t, pos.AvgPx, dec(str(st.Position, "avgPx")), "1e-9", st.Label+" 开仓均价")
				near(t, pos.Margin, dec(str(st.Position, "margin")), "1e-9", st.Label+" 保证金")
				t.Logf("%s: pos=%s margin=%s cash=%s", st.Label, pos.Pos, pos.Margin, s.CashBal("USDT"))
			} else {
				if len(s.Positions()) != 0 {
					t.Errorf("%s: 真实账户已无持仓，模拟器仍有 %d 个", st.Label, len(s.Positions()))
				}
				t.Logf("%s: 无持仓 cash=%s", st.Label, s.CashBal("USDT"))
			}
		}
	}
}

// TestPartialCloseReleasesMarginProRata 部分平仓按张数比例释放保证金。
//
// 实测：4 张仓位保证金 624.705284，平掉 1 张后剩 468.528963，恰为四分之三。
func TestPartialCloseReleasesMarginProRata(t *testing.T) {
	s := newSim(t, types.NetMode)

	open := mustFill(t, s, netFill(types.Buy, "4", "78088.1605"))
	eq(t, open.After.Margin, "624.705284", "开仓保证金")

	closed := mustFill(t, s, netFill(types.Sell, "1", "78078.2"))
	eq(t, closed.After.Margin, "468.528963", "平掉四分之一后的保证金")

	// 释放的部分恰为四分之一
	released := open.After.Margin.Sub(closed.After.Margin)
	eq(t, released, "156.176321", "释放的保证金")
	eq(t, released, div(open.After.Margin, decimal.NewFromInt(4)).String(), "释放量应为四分之一")
}

func TestBalanceComposition(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78088.1605"))

	if err := s.SetMark("BTC-USDT-SWAP", dec("78078.34")); err != nil {
		t.Fatalf("设置标记价失败: %v", err)
	}
	b, err := s.Balance("USDT")
	if err != nil {
		t.Fatalf("查询余额失败: %v", err)
	}

	// upl = 0.01×4×(78078.34 − 78088.1605) = -0.39282
	eq(t, b.Upl, "-0.39282", "未实现盈亏")
	// isoEq = margin + upl
	eq(t, b.IsoEq, dec("624.705284").Add(dec("-0.39282")).String(), "逐仓权益")
	eq(t, b.FrozenBal, b.IsoEq.String(), "冻结余额应等于逐仓权益")
	eq(t, b.Eq, b.CashBal.Add(b.IsoEq).String(), "币种权益应等于现金加逐仓权益")
	eq(t, b.AvailBal, b.CashBal.String(), "无挂单时可用余额等于现金余额")
}

// ---- 错误路径 ----

func TestInsufficientBalance(t *testing.T) {
	s, err := New(Config{RefData: refdata.MustEmbedded(), DefaultLever: decimal.NewFromInt(5)})
	if err != nil {
		t.Fatalf("新建模拟器失败: %v", err)
	}
	if err := s.Deposit("USDT", dec("100")); err != nil {
		t.Fatalf("入金失败: %v", err)
	}

	_, err = s.Fill(netFill(types.Buy, "10", "78000"))
	if !okxerr.HasCode(err, okxerr.CodeInsufficientBal) {
		t.Errorf("余额不足的错误 = %v，期望 51008", err)
	}
	// 失败的成交不应留下任何痕迹
	if len(s.Positions()) != 0 {
		t.Error("成交失败后不应产生仓位")
	}
	eq(t, s.CashBal("USDT"), "100", "成交失败后现金不应变动")
}

func TestPosSideMustMatchPosMode(t *testing.T) {
	net := newSim(t, types.NetMode)
	f := netFill(types.Buy, "1", "78000")
	f.PosSide = types.PosLong
	if _, err := net.Fill(f); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("买卖模式下用 long 的错误 = %v，期望 51000", err)
	}

	ls := newSim(t, types.LongShortMode)
	f = netFill(types.Buy, "1", "78000")
	f.PosSide = types.PosNet
	if _, err := ls.Fill(f); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("开平仓模式下用 net 的错误 = %v，期望 51000", err)
	}
}

func TestUnknownInstrument(t *testing.T) {
	s := newSim(t, types.NetMode)
	f := netFill(types.Buy, "1", "78000")
	f.InstID = "NOPE-USDT-SWAP"
	if _, err := s.Fill(f); !okxerr.HasCode(err, okxerr.CodeInstNotExist) {
		t.Errorf("未知合约的错误 = %v，期望 51001", err)
	}
}

func TestInvalidSizeRejected(t *testing.T) {
	s := newSim(t, types.NetMode)
	f := netFill(types.Buy, "0.015", "78000") // lotSz = 0.01
	if _, err := s.Fill(f); !okxerr.HasCode(err, okxerr.CodeNotLotSizeMultiple) {
		t.Errorf("非整数倍数量的错误 = %v，期望 51121", err)
	}
}

func TestCrossModeNotYetSupported(t *testing.T) {
	s := newSim(t, types.NetMode)
	f := netFill(types.Buy, "1", "78000")
	f.TdMode = types.TdCross
	if _, err := s.Fill(f); err == nil {
		t.Error("全仓模式尚未支持，应当明确拒绝而不是给出错误的结果")
	}
}

func TestSetLeverageRespectsTierCap(t *testing.T) {
	s := newSim(t, types.NetMode)

	if err := s.SetLeverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosNet,
		decimal.NewFromInt(50)); err != nil {
		t.Errorf("设置 50 倍杠杆失败: %v", err)
	}
	eq(t, s.Leverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosNet), "50", "生效的杠杆")

	err := s.SetLeverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosNet,
		decimal.NewFromInt(1000))
	if !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("超过档位上限的杠杆错误 = %v，期望 51000", err)
	}
}

func TestNewRequiresRefData(t *testing.T) {
	if _, err := New(Config{}); !okxerr.HasCode(err, okxerr.CodeParamEmpty) {
		t.Errorf("缺少 RefData 的错误 = %v，期望 50014", err)
	}
}

func TestWithdraw(t *testing.T) {
	s := newSim(t, types.NetMode)

	if err := s.Withdraw("USDT", dec("3000")); err != nil {
		t.Fatalf("出金失败: %v", err)
	}
	eq(t, s.CashBal("USDT"), "7000", "出金后余额")

	if err := s.Withdraw("USDT", dec("99999")); !okxerr.HasCode(err, okxerr.CodeInsufficientBal) {
		t.Errorf("超额出金的错误 = %v，期望 51008", err)
	}
}

func mustEmbedded(t *testing.T) *refdata.Snapshot {
	t.Helper()
	s, err := refdata.Embedded()
	if err != nil {
		t.Fatalf("载入内置快照失败: %v", err)
	}
	return s
}

// TestSetPositionRestoresExactState 整体置入仓位应当原样恢复全部状态，
// 包括那些无法由均价反推的累计量。
//
// 按均价重放一笔成交只能复现持仓与保证金：均价是加权结果，从它反推不出之前
// 发生过哪些成交，累计手续费与累计资金费因而无从复现。回测引擎断点续跑、
// 对拍工具搬运真实仓位，都需要整体置入。
func TestSetPositionRestoresExactState(t *testing.T) {
	s := newSim(t, types.NetMode)

	want := Position{
		InstID: "BTC-USDT-SWAP", MgnMode: types.MgnIsolated, PosSide: types.PosNet,
		Pos: dec("12.34"), AvgPx: dec("77123.4"), Lever: dec("20"),
		Margin: dec("475.951278"), RealizedPnl: dec("182.049885"),
		Fee: dec("-10.066836"), Funding: dec("-75.534427"), LiqPenalty: dec("-1.5"),
		CTime: 1000, UTime: 2000,
	}
	if err := s.SetPosition(want); err != nil {
		t.Fatalf("置入仓位失败: %v", err)
	}

	got, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	if !ok {
		t.Fatal("置入后查不到仓位")
	}
	eq(t, got.Pos, want.Pos.String(), "持仓")
	eq(t, got.AvgPx, want.AvgPx.String(), "开仓均价")
	eq(t, got.Margin, want.Margin.String(), "保证金")
	eq(t, got.Fee, want.Fee.String(), "累计手续费")
	eq(t, got.Funding, want.Funding.String(), "累计资金费")
	eq(t, got.LiqPenalty, want.LiqPenalty.String(), "累计爆仓罚金")
	eq(t, got.RealizedPnl, want.RealizedPnl.String(), "累计毛已实现盈亏")
	if got.CTime != want.CTime || got.UTime != want.UTime {
		t.Errorf("时间戳未保留: cTime=%d uTime=%d", got.CTime, got.UTime)
	}

	// 置入后继续演化应当从这个状态接着走
	r := mustFill(t, s, netFill(types.Sell, "2.34", "78000"))
	eq(t, r.After.Pos, "10", "减仓后持仓")
	eq(t, r.After.AvgPx, want.AvgPx.String(), "部分平仓不改变均价")
	// 累计手续费在原有基础上继续累加
	if !r.After.Fee.LessThan(want.Fee) {
		t.Errorf("累计手续费未在原有基础上累加：%s -> %s", want.Fee, r.After.Fee)
	}
}

func TestSetPositionValidates(t *testing.T) {
	s := newSim(t, types.NetMode)

	base := Position{
		InstID: "BTC-USDT-SWAP", MgnMode: types.MgnIsolated, PosSide: types.PosNet,
		Pos: dec("1"), AvgPx: dec("78000"), Lever: dec("5"),
	}

	bad := base
	bad.InstID = ""
	if err := s.SetPosition(bad); !okxerr.HasCode(err, okxerr.CodeParamEmpty) {
		t.Errorf("缺少 instId 的错误 = %v，期望 50014", err)
	}
	bad = base
	bad.InstID = "NOPE-USDT-SWAP"
	if err := s.SetPosition(bad); !okxerr.HasCode(err, okxerr.CodeInstNotExist) {
		t.Errorf("未知合约的错误 = %v，期望 51001", err)
	}
	bad = base
	bad.Pos = dec("1.005") // lotSz = 0.01
	if err := s.SetPosition(bad); !okxerr.HasCode(err, okxerr.CodeNotLotSizeMultiple) {
		t.Errorf("非整数倍张数的错误 = %v，期望 51121", err)
	}
	bad = base
	bad.AvgPx = decimal.Zero
	if err := s.SetPosition(bad); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("零均价的错误 = %v，期望 51000", err)
	}
	bad = base
	bad.Margin = dec("-1")
	if err := s.SetPosition(bad); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("负保证金的错误 = %v，期望 51000", err)
	}

	// 置入空仓等同于删除
	if err := s.SetPosition(base); err != nil {
		t.Fatal(err)
	}
	empty := base
	empty.Pos = decimal.Zero
	if err := s.SetPosition(empty); err != nil {
		t.Fatalf("置入空仓失败: %v", err)
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); ok {
		t.Error("置入空仓应当删除该仓位")
	}
}

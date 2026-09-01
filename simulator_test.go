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

	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("78078.34")); err != nil {
		t.Fatalf("设置标记价失败: %v", err)
	}
	b, err := s.BalanceOf("USDT")
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

func TestSetLeverageRespectsTierCap(t *testing.T) {
	s := newSim(t, types.NetMode)

	if err := s.SetLeverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosNet,
		decimal.NewFromInt(50)); err != nil {
		t.Errorf("设置 50 倍杠杆失败: %v", err)
	}
	eq(t, s.Leverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosNet), "50", "生效的杠杆")

	err := s.SetLeverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosNet,
		decimal.NewFromInt(1000))
	if !okxerr.HasCode(err, okxerr.CodeLeverTooHigh) {
		t.Errorf("超过档位上限的杠杆错误 = %v，期望 59102", err)
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

// TestAdjustMarginFloorCountsUnrealizedLoss 锁定「浮亏吃掉可减额」。
//
// 下限是「减完之后【仓位权益】不得低于开仓初始保证金」，而不是保证金本身。
// 早先只在一个有浮盈的仓位上验过，于是漏掉了这一项——v0.9.0 最后一轮对拍时，
// 一个浮亏的仓位上减掉全部 room 被 OKX 以 59301 拒绝，才把它照出来。
func TestAdjustMarginFloorCountsUnrealizedLoss(t *testing.T) {
	newPos := func(t *testing.T, markPx string) *Simulator {
		t.Helper()
		s := newSim(t, types.NetMode)
		mustFill(t, s, netFill(types.Buy, "4", "78000"))
		if err := s.SetMarkPx("BTC-USDT-SWAP", dec(markPx)); err != nil {
			t.Fatal(err)
		}
		// 追加 100，制造可减空间
		if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet,
			types.MarginAdd, dec("100")); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// 无浮盈浮亏：可减额就是刚追加的那 100
	s := newPos(t, "78000")
	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet,
		types.MarginReduce, dec("100")); err != nil {
		t.Errorf("没有浮亏时应当能把追加的 100 全减回去: %v", err)
	}

	// 浮亏 40（价格从 78000 跌到 77000，0.01×4 张）：可减额只剩 60
	s = newPos(t, "77000")
	m, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, m.UPL, "-40", "浮亏")

	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet,
		types.MarginReduce, dec("100")); err == nil {
		t.Error("有 40 浮亏时不该还能减掉全部 100")
	} else if !okxerr.HasCode(err, okxerr.CodeMarginAdjustExceeds) {
		t.Errorf("错误码 = %v，期望 59301", err)
	}
	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet,
		types.MarginReduce, dec("60")); err != nil {
		t.Errorf("减 60（= 100 − 40 浮亏）应当可以: %v", err)
	}

	// 浮盈不会放大可减额——下限只由开仓初始保证金决定
	s = newPos(t, "79000")
	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet,
		types.MarginReduce, dec("140")); err == nil {
		t.Error("浮盈不应让可减额超过追加的部分加上原有空间")
	}
}

// TestInstrumentAccessor 模拟器要能给出合约规格。
//
// 回测引擎每根 K 线都要用 lotSz 取整张数、用 tickSz 取整价格。让调用方自己另留
// 一份 RefData 不只是麻烦——两份一旦不是同一个（比如其中一份被自动刷新换掉），
// 取整用的规则就和模拟器实际用的不是一套，而这种偏差不报错，只会让结果悄悄不对。
func TestInstrumentAccessor(t *testing.T) {
	s := newSim(t, types.NetMode)

	inst, err := s.Instrument("BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("取合约规格失败: %v", err)
	}
	eq(t, inst.LotSz, "0.01", "lotSz")
	if inst.InstID != "BTC-USDT-SWAP" {
		t.Errorf("instId = %q", inst.InstID)
	}
	// 拿到之后取整方法直接可用
	eq(t, inst.RoundSize(dec("3.14159")), "3.14", "按 lotSz 取整")

	if _, err := s.Instrument("NOPE-USDT-SWAP"); !okxerr.HasCode(err, okxerr.CodeInstNotExist) {
		t.Errorf("未知合约的错误 = %v，期望 51001", err)
	}
}

// TestMetricsAt 假设价格下的风险指标，且不污染行情表。
//
// 压力测试要问「价格到 X 时爆不爆」。此前只能改掉标记价、算完再改回来——那是有
// 状态的做法，中途出错会把行情表留在错误的值上，改标记价本身也可能触发别的东西。
func TestMetricsAt(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("78000")); err != nil {
		t.Fatal(err)
	}

	base, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, base.UPL, "0", "当前浮盈")

	// 假设跌到 77000
	at, err := s.MetricsAt("BTC-USDT-SWAP", types.PosNet, dec("77000"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, at.UPL, "-40", "假设价下的浮盈")
	if !at.MgnRatio.LessThan(base.MgnRatio) {
		t.Errorf("跌价后的保证金率 %s 应低于当前的 %s", at.MgnRatio, base.MgnRatio)
	}

	// 行情表不该被污染
	eq(t, s.MarkPx("BTC-USDT-SWAP"), "78000", "标记价不应被 MetricsAt 改动")
	again, _ := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, again.UPL, "0", "再查一次仍是当前价下的结果")

	// 跌到强平价之下应当判定为可强平
	deep, err := s.MetricsAt("BTC-USDT-SWAP", types.PosNet, base.LiqPx.Sub(dec("100")))
	if err != nil {
		t.Fatal(err)
	}
	if !deep.IsLiquidatable() {
		t.Errorf("跌破强平价 %s 后应判定为可强平，实际保证金率 %s", base.LiqPx, deep.MgnRatio)
	}

	if _, err := s.MetricsAt("BTC-USDT-SWAP", types.PosNet, dec("0")); err == nil {
		t.Error("非正数的标记价应当被拒绝")
	}
}

// TestAdjustMarginReducibleAgainstRealBound 把可减额上界与真实账户的行为对齐。
//
//	可减额上界 = 仓位保证金 − 按【开仓均价】算的初始保证金 + min(0, 未实现盈亏)
//
// 这条曾长期存疑：早先用慢速二分测出「上界比本式略高 1e-2 量级」。**那是测量假象。**
// 二分每次成功都改变状态、来回加减耗时数秒，期间浮亏一直在漂。
//
// 2026-09-01 换了测法才定案：读一次仓位、立刻算出上界，然后两次调用夹逼——先试高的
// （期望被拒，被拒不改变状态），再试低的（期望通过），窗口压到约 0.4 秒。
// 两个规模（200 张与 1800 张）、多轮重复，`×1.0004` 一律被拒、`×0.9996` 一律通过；
// 小仓收紧到 `±0.005%` 仍精确夹住。
//
// 顺带**排除了费率项**：若公式漏了「减去名义 × 吃单费率」，那一项在小仓是上界的
// 0.254%，而夹逼区间是 0.005%——紧了 50 倍，`×0.9996` 本该失败却通过了。
func TestAdjustMarginReducibleAgainstRealBound(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "conformance",
		"isolated-margin-reducible.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx struct {
		Trials []struct {
			Sz     string `json:"sz"`
			Margin string `json:"margin"`
			OpenIM string `json:"openIM"`
			Upl    string `json:"upl"`
			Pred   string `json:"pred"`
			Hi     string `json:"hi"`
			HiOK   bool   `json:"hiOK"`
			Lo     string `json:"lo"`
			LoOK   bool   `json:"loOK"`
		} `json:"bracket_0.04pct"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	if len(fx.Trials) < 4 {
		t.Fatalf("只有 %d 组夹逼样本，夹具应当有 4 组", len(fx.Trials))
	}

	var checked int
	for _, tr := range fx.Trials {
		// 夹具自身的一致性：高的被拒、低的通过，两侧都要成立才算夹住
		if tr.HiOK || !tr.LoOK {
			t.Errorf("夹具里这组没夹住：高额 %s 通过=%v，低额 %s 通过=%v",
				tr.Hi, tr.HiOK, tr.Lo, tr.LoOK)
			continue
		}
		// 本式复算：margin − openIM + min(0, upl)
		margin, openIM, upl := dec(tr.Margin), dec(tr.OpenIM), dec(tr.Upl)
		loss := decimal.Min(decimal.Zero, upl)
		near(t, margin.Sub(openIM).Add(loss), dec(tr.Pred), "1e-18",
			"可减额 = 保证金 − 开仓初始保证金 + min(0, 浮盈)")
		// 真实上界落在 (lo, hi] 之间，而本式的预测也必须落在同一区间
		if p := dec(tr.Pred); p.LessThan(dec(tr.Lo)) || p.GreaterThan(dec(tr.Hi)) {
			t.Errorf("本式预测 %s 落在夹逼区间 (%s, %s] 之外", p, tr.Lo, tr.Hi)
		}
		checked++
	}
	if checked < 4 {
		t.Fatalf("只核了 %d 组，样本不足以支撑结论", checked)
	}
	t.Logf("核了 %d 组真实夹逼样本", checked)
}

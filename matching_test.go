package okxsim

import (
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func bar(last, high, low string, ts int64) Bar {
	return Bar{
		InstID: "BTC-USDT-SWAP", Last: dec(last),
		High: dec(high), Low: dec(low), Ts: ts,
	}
}

func mustAdvance(t *testing.T, s *Simulator, b Bar) StepResult {
	t.Helper()
	r, err := s.Advance(b)
	if err != nil {
		t.Fatalf("推进行情失败: %v", err)
	}
	return r
}

// TestLimitOrderRestsThenFillsAsMaker 挂住的限价单被价格触及后成交，且按 maker 计费。
//
// 成交角色由模拟器判定是内置撮合最实在的价值：交给调用方填写是常见的错误来源，
// 而它直接决定手续费。
func TestLimitOrderRestsThenFillsAsMaker(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	// 买单挂在市价下方，不会立即成交
	pr, err := s.PlaceOrder(limitOrder("o1", types.Buy, "4", "70000"))
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if pr.State != types.OrdLive {
		t.Fatalf("委托状态 = %q，期望挂住", pr.State)
	}
	if len(s.PendingOrders("BTC-USDT-SWAP")) != 1 {
		t.Fatal("委托未挂入簿中")
	}

	// 价格未触及，不成交
	step := mustAdvance(t, s, bar("72000", "73000", "71000", 2))
	if len(step.Fills) != 0 {
		t.Errorf("价格未触及委托价，不应成交: %+v", step.Fills)
	}

	// 最低价触及委托价，成交
	step = mustAdvance(t, s, bar("71000", "72000", "69500", 3))
	if len(step.Fills) != 1 {
		t.Fatalf("成交笔数 = %d，期望 1", len(step.Fills))
	}
	fr := step.Fills[0]

	// 按委托价成交，且按 maker 费率计费
	// 0.01×4×70000 = 2800，maker -0.0002 -> -0.56
	eq(t, fr.After.AvgPx, "70000", "成交价应为委托价")
	eq(t, fr.Fee, "-0.56", "挂住后成交应按 maker 费率")
	eq(t, fr.After.Pos, "4", "成交后持仓")

	// 冻结应已自动解除
	if len(s.PendingOrders("BTC-USDT-SWAP")) != 0 {
		t.Error("成交后委托应从簿中移除")
	}
	b, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	if !b.OrdFrozen.IsZero() {
		t.Errorf("成交后仍有冻结 %s", b.OrdFrozen)
	}
}

// TestMarketableLimitFillsAsTaker 下单即可成交的限价单按 taker 计费。
func TestMarketableLimitFillsAsTaker(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	// 买单挂在市价之上，立即成交
	pr, err := s.PlaceOrder(limitOrder("o1", types.Buy, "4", "79000"))
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if pr.State != types.OrdFilled {
		t.Fatalf("委托状态 = %q，期望立即成交", pr.State)
	}
	if len(pr.Fills) != 1 {
		t.Fatalf("成交笔数 = %d", len(pr.Fills))
	}
	fr := pr.Fills[0]

	// 成交价取最新价，对买方不劣于其限价——限价买单不会以高于限价的价格成交
	eq(t, fr.After.AvgPx, "78000", "应以最新价成交，优于限价")
	// 0.01×4×78000 = 3120，taker -0.0005 -> -1.56
	eq(t, fr.Fee, "-1.56", "立即成交应按 taker 费率")
	if len(s.PendingOrders("BTC-USDT-SWAP")) != 0 {
		t.Error("立即成交的委托不应留在簿中")
	}
}

func TestMarketOrderFillsAtLast(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	o := limitOrder("o1", types.Buy, "4", "0")
	o.OrdType = types.OrdMarket
	o.Px = decimal.Zero
	pr, err := s.PlaceOrder(o)
	if err != nil {
		t.Fatalf("市价单失败: %v", err)
	}
	if pr.State != types.OrdFilled {
		t.Fatalf("市价单状态 = %q", pr.State)
	}
	eq(t, pr.Fills[0].After.AvgPx, "78000", "市价单按最新价成交")
	eq(t, pr.Fills[0].Fee, "-1.56", "市价单按 taker 费率")
}

// TestPostOnlyCanceledWhenMarketable 只挂单委托若会立即成交则被撤销，不成交。
func TestPostOnlyCanceledWhenMarketable(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	o := limitOrder("o1", types.Buy, "4", "79000") // 会立即成交
	o.OrdType = types.OrdPostOnly
	pr, err := s.PlaceOrder(o)
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if pr.State != types.OrdCanceled {
		t.Errorf("只挂单委托会立即成交时应被撤销，实际状态 %q", pr.State)
	}
	if len(pr.Fills) != 0 {
		t.Error("只挂单委托不应产生成交")
	}
	if len(s.PendingOrders("")) != 0 {
		t.Error("被撤销的委托不应留在簿中")
	}

	// 挂在市价下方则正常挂住
	o2 := limitOrder("o2", types.Buy, "4", "70000")
	o2.OrdType = types.OrdPostOnly
	pr, err = s.PlaceOrder(o2)
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != types.OrdLive {
		t.Errorf("不会立即成交的只挂单委托应当挂住，实际 %q", pr.State)
	}
}

// TestImmediateOrdersDoNotRest 立即成交类委托无法成交时直接撤销，不挂入簿中。
func TestImmediateOrdersDoNotRest(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	for _, ot := range []types.OrdType{types.OrdIOC, types.OrdFOK} {
		o := limitOrder("o-"+string(ot), types.Buy, "4", "70000") // 无法立即成交
		o.OrdType = ot
		pr, err := s.PlaceOrder(o)
		if err != nil {
			t.Fatalf("%s 下单失败: %v", ot, err)
		}
		if pr.State != types.OrdCanceled {
			t.Errorf("%s 无法立即成交时应被撤销，实际 %q", ot, pr.State)
		}
	}
	if len(s.PendingOrders("")) != 0 {
		t.Error("立即成交类委托不应挂入簿中")
	}
}

// TestFillOrderPriority 同一步内多笔可成交时按下单先后处理，与真实的时间优先一致。
func TestFillOrderPriority(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	first := limitOrder("b", types.Buy, "1", "70000")
	first.Ts = 10
	second := limitOrder("a", types.Buy, "1", "71000") // 委托 ID 更靠前但下单更晚
	second.Ts = 20
	if _, err := s.PlaceOrder(first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceOrder(second); err != nil {
		t.Fatal(err)
	}

	step := mustAdvance(t, s, bar("69000", "78000", "69000", 30))
	if len(step.Fills) != 2 {
		t.Fatalf("成交笔数 = %d，期望 2", len(step.Fills))
	}
	// 先下的先成交，即便其委托 ID 排序更靠后
	eq(t, step.Fills[0].After.AvgPx, "70000", "应先成交下单更早的那笔")
}

// TestReduceOnlyRejectsOpening 只减仓委托含开仓量时应被拒。
func TestReduceOnlyRejectsOpening(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	o := limitOrder("o1", types.Buy, "4", "70000")
	o.ReduceOnly = true
	if _, err := s.PlaceOrder(o); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("空仓时的只减仓开单错误 = %v，期望 51000", err)
	}

	// 有多头持仓后，反向的只减仓委托应当被接受
	mo := limitOrder("m", types.Buy, "4", "0")
	mo.OrdType = types.OrdMarket
	if _, err := s.PlaceOrder(mo); err != nil {
		t.Fatal(err)
	}
	c := limitOrder("o2", types.Sell, "2", "86000")
	c.ReduceOnly = true
	pr, err := s.PlaceOrder(c)
	if err != nil {
		t.Fatalf("只减仓平仓委托被拒: %v", err)
	}
	if pr.State != types.OrdLive {
		t.Errorf("委托状态 = %q", pr.State)
	}
	// 平仓方向不产生冻结
	if !pr.Cost.Frozen.IsZero() {
		t.Errorf("平仓委托不应冻结资金，实际 %s", pr.Cost.Frozen)
	}
}

// TestManualFillWithOrdIDReleasesFreeze 引擎自行撮合时，Fill 带上 OrdID 即可解除冻结。
func TestManualFillWithOrdIDReleasesFreeze(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	pr, err := s.PlaceOrder(limitOrder("o1", types.Buy, "4", "70000"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	if before.OrdFrozen.IsZero() {
		t.Fatal("挂单后应有冻结")
	}

	// 引擎自行判定成交，直接灌入并带上委托 ID
	if _, err := s.Fill(Fill{
		OrdID: "o1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet, Sz: dec("4"), Px: dec("70000"),
		ExecType: types.Maker, Ts: 2,
	}); err != nil {
		t.Fatalf("手工成交失败: %v", err)
	}

	after, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	if !after.OrdFrozen.IsZero() {
		t.Errorf("带 OrdID 的成交应解除冻结，实际仍有 %s", after.OrdFrozen)
	}
	if len(s.PendingOrders("")) != 0 {
		t.Error("成交后委托应从簿中移除")
	}
	_ = pr
}

// TestFillBalanceCheckUsesAvailable 余额校验的约束是可用余额而非现金余额。
//
// 被其他挂单冻结的部分已有归属，不能再拿来支撑新的成交——否则挂着一笔占用
// 大半资金的单，还能再吃下一笔同样大的成交。
func TestFillBalanceCheckUsesAvailable(t *testing.T) {
	s, err := New(Config{
		PosMode: types.NetMode, RefData: mustEmbedded(t), DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit("USDT", dec("2000")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastPx("BTC-USDT-SWAP", dec("78000")); err != nil {
		t.Fatal(err)
	}

	// 挂一笔占用约 1560 的单，剩余可用约 436
	if _, err := s.PlaceOrder(limitOrder("o1", types.Buy, "10", "70000")); err != nil {
		t.Fatalf("挂单失败: %v", err)
	}

	// 另一笔需要约 1560 的成交应当被拒——现金还有 2000，但可用不足
	_, err = s.Fill(Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosNet, Sz: dec("10"), Px: dec("78000"),
		ExecType: types.Taker, Ts: 2,
	})
	if !okxerr.HasCode(err, okxerr.CodeInsufficientBal) {
		t.Errorf("可用余额不足的错误 = %v，期望 51008", err)
	}

	// 而挂单自身成交时，它自己的冻结应被排除，不该挡下自己
	step := mustAdvance(t, s, bar("69000", "78000", "69000", 3))
	if len(step.Fills) != 1 {
		t.Fatalf("挂单应当成交，实际成交 %d 笔，撤销 %v", len(step.Fills), step.Canceled)
	}
}

func TestAdvanceValidatesBar(t *testing.T) {
	s := newSim(t, types.NetMode)

	if _, err := s.Advance(Bar{Last: dec("78000")}); err == nil {
		t.Error("缺少 instId 应当报错")
	}
	if _, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP"}); err == nil {
		t.Error("缺少最新价应当报错")
	}
	if _, err := s.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: dec("78000"),
		High: dec("77000"), Low: dec("79000"),
	}); err == nil {
		t.Error("最低价高于最高价应当报错")
	}
}

// TestAdvanceUpdatesMarkAndLast 推进行情应同时更新标记价与最新价。
func TestAdvanceUpdatesMarkAndLast(t *testing.T) {
	s := newSim(t, types.NetMode)

	mustAdvance(t, s, bar("78000", "78500", "77500", 1))
	eq(t, s.LastPx("BTC-USDT-SWAP"), "78000", "最新价")
	eq(t, s.MarkPx("BTC-USDT-SWAP"), "78000", "未给标记价时以最新价代替")

	b := bar("78000", "78500", "77500", 2)
	b.MarkPx = dec("78010")
	mustAdvance(t, s, b)
	eq(t, s.MarkPx("BTC-USDT-SWAP"), "78010", "给了标记价时应采用之")
	eq(t, s.LastPx("BTC-USDT-SWAP"), "78000", "最新价不受标记价影响")
}

func TestCancelOrderReleasesFreeze(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	before, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceOrder(limitOrder("o1", types.Buy, "4", "70000")); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelOrder("o1"); err != nil {
		t.Fatalf("撤单失败: %v", err)
	}
	after, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, after.AvailBal, before.AvailBal.String(), "撤单后可用余额应恢复")

	// 撤销后该委托不应再被行情触发
	step := mustAdvance(t, s, bar("69000", "78000", "69000", 2))
	if len(step.Fills) != 0 {
		t.Error("已撤销的委托不应成交")
	}
}

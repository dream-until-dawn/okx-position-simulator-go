package okxsim

import (
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// liqSim 造一个高杠杆仓位，使强平距离足够近、便于在测试里精确触发。
func liqSim(t *testing.T, side types.Side, lever string) (*Simulator, Metrics) {
	t.Helper()
	s := newSim(t, types.NetMode)
	if err := s.SetLeverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosNet, dec(lever)); err != nil {
		t.Fatalf("设置杠杆失败: %v", err)
	}
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))
	mustFill(t, s, netFill(side, "10", "78000"))

	m, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

// TestLiquidationTriggersAtLiqPx 标记价越过强平价即触发强平。
func TestLiquidationTriggersAtLiqPx(t *testing.T) {
	s, m := liqSim(t, types.Buy, "100")
	t.Logf("开仓后 liqPx=%s bkPx=%s mgnRatio=%s", m.LiqPx, m.BkPx, m.MgnRatio.Round(4))

	// 停在强平价上方，不该触发
	safe := m.LiqPx.Add(dec("50"))
	step := mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: safe, High: safe, Low: safe, Ts: 2})
	if len(step.Liquidations) != 0 {
		t.Fatalf("强平价上方不应触发，实际 %+v", step.Liquidations)
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); !ok {
		t.Fatal("仓位不该消失")
	}

	// 跌破强平价，触发
	hit := m.LiqPx.Sub(dec("1"))
	step = mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, High: hit, Low: hit, Ts: 3})
	if len(step.Liquidations) != 1 {
		t.Fatalf("跌破强平价应触发一次强平，实际 %d 次", len(step.Liquidations))
	}
	liq := step.Liquidations[0]
	if liq.Kind != LiqFull {
		t.Errorf("首档仓位应当直接全平，实际 %q", liq.Kind)
	}
	eq(t, liq.Sz, "10", "被平掉的张数")
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); ok {
		t.Error("强平后仓位应被移除")
	}
	t.Logf("强平：成交价 %s，损失保证金 %s，穿仓 %s",
		liq.Px, liq.Loss, liq.Bankrupt)
}

// TestLiquidationLosesEntireMargin 逐仓强平的结果是保证金全额损失，现金余额不变。
//
// 保证金在开仓时就已从现金划走，强平只是让它归零；触发时权益尚存的
// 「维持保证金 + 平仓手续费」缓冲归风险准备金，不退还持仓方——
// OKX 的仓位结构里有 liqPenalty 一项，正是这笔钱的去处。
func TestLiquidationLosesEntireMargin(t *testing.T) {
	s, m := liqSim(t, types.Buy, "100")
	pos, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	margin := pos.Margin
	cashBefore := s.CashBal("USDT")

	hit := m.LiqPx.Sub(dec("1"))
	step := mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, High: hit, Low: hit, Ts: 3})
	if len(step.Liquidations) != 1 {
		t.Fatalf("应触发强平")
	}

	eq(t, step.Liquidations[0].Loss, margin.String(), "损失应为全部保证金")
	eq(t, s.CashBal("USDT"), cashBefore.String(), "强平不应改变现金余额")

	b, err := s.Balance("USDT")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, b.IsoEq, "0", "强平后逐仓权益归零")
	eq(t, b.Eq, cashBefore.String(), "币种权益应只剩现金")
}

// TestLiquidationCancelsPendingOrders 强平前先撤单。
//
// 挂单占用的保证金必须先释放，否则会把本可用于维持仓位的资金白白锁着。
func TestLiquidationCancelsPendingOrders(t *testing.T) {
	s, m := liqSim(t, types.Buy, "100")

	// 挂一笔远离市价的买单，它会一直挂着
	if _, err := s.PlaceOrder(limitOrder("o1", types.Buy, "1", "50000")); err != nil {
		t.Fatalf("挂单失败: %v", err)
	}
	if len(s.OpenOrders("")) != 1 {
		t.Fatal("委托未挂上")
	}

	hit := m.LiqPx.Sub(dec("1"))
	step := mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, High: hit, Low: hit, Ts: 3})
	if len(step.Liquidations) != 1 {
		t.Fatalf("应触发强平，实际 %+v", step)
	}
	if got := step.Liquidations[0].CanceledOrders; len(got) != 1 || got[0] != "o1" {
		t.Errorf("强平应撤销挂单，实际 %v", got)
	}
	if len(s.OpenOrders("")) != 0 {
		t.Error("强平后不应还有挂单")
	}
	b, err := s.Balance("USDT")
	if err != nil {
		t.Fatal(err)
	}
	if !b.OrdFrozen.IsZero() {
		t.Errorf("撤单后冻结应释放，实际 %s", b.OrdFrozen)
	}
}

// TestLiquidationBankruptOnGap 行情跳空穿过破产价时产生穿仓。
//
// 一根 K 线内价格从强平价上方直接跳到破产价下方，中间没有可成交的价位，
// 亏损因而超出保证金。这是回测中真实存在的情形。
func TestLiquidationBankruptOnGap(t *testing.T) {
	s, m := liqSim(t, types.Buy, "100")
	t.Logf("liqPx=%s bkPx=%s", m.LiqPx, m.BkPx)

	// 直接跳到破产价下方
	gap := m.BkPx.Sub(dec("200"))
	step := mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: gap, High: gap, Low: gap, Ts: 3})
	if len(step.Liquidations) != 1 {
		t.Fatalf("应触发强平")
	}
	liq := step.Liquidations[0]
	if !liq.IsBankrupt() {
		t.Errorf("跳空穿过破产价应产生穿仓，实际穿仓金额 %s", liq.Bankrupt)
	}
	// 成交价应当是跳空后的标记价，而不是已经无法成交的破产价
	eq(t, liq.Px, gap.String(), "跳空时按标记价成交")
	t.Logf("穿仓金额 %s（保证金 %s）", liq.Bankrupt, liq.Loss)

	// 正常触发时不应有穿仓
	s2, m2 := liqSim(t, types.Buy, "100")
	hit := m2.LiqPx.Sub(dec("1"))
	step2 := mustAdvance(t, s2, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, High: hit, Low: hit, Ts: 3})
	if step2.Liquidations[0].IsBankrupt() {
		t.Errorf("常态强平不应穿仓，实际 %s", step2.Liquidations[0].Bankrupt)
	}
}

// TestLiquidationShortSide 空头方向的强平，与多头对称。
func TestLiquidationShortSide(t *testing.T) {
	s, m := liqSim(t, types.Sell, "100")
	t.Logf("空头 liqPx=%s bkPx=%s", m.LiqPx, m.BkPx)

	if !m.LiqPx.GreaterThan(dec("78000")) {
		t.Fatalf("空头强平价应高于开仓价，实际 %s", m.LiqPx)
	}
	// 价格上涨越过强平价
	hit := m.LiqPx.Add(dec("1"))
	step := mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, High: hit, Low: hit, Ts: 3})
	if len(step.Liquidations) != 1 {
		t.Fatalf("空头应被强平，实际 %+v", step)
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); ok {
		t.Error("强平后仓位应被移除")
	}
}

// TestNoLiquidationWhenHealthy 保证金率高于 1 时不触发。
func TestNoLiquidationWhenHealthy(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))
	mustFill(t, s, netFill(types.Buy, "4", "78000")) // 默认 5 倍杠杆，很安全

	for i, px := range []string{"77000", "76000", "75000", "74000"} {
		step := mustAdvance(t, s, Bar{
			InstID: "BTC-USDT-SWAP", Last: dec(px), High: dec(px), Low: dec(px),
			Ts: int64(2 + i)})
		if len(step.Liquidations) != 0 {
			m, _ := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
			t.Fatalf("价格 %s 时不应强平（保证金率 %s）", px, m.MgnRatio)
		}
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); !ok {
		t.Error("仓位不该消失")
	}
}

// TestLiquidationUsesMarkNotLast 强平判定用标记价而非最新成交价。
//
// 用最新价会让插针把本不该爆的仓位扫掉——这是强平判定的通行做法，OKX 亦然。
func TestLiquidationUsesMarkNotLast(t *testing.T) {
	s, m := liqSim(t, types.Buy, "100")

	// 最新价插到强平价下方，但标记价保持在上方
	safe := m.LiqPx.Add(dec("50"))
	spike := m.LiqPx.Sub(dec("100"))
	step := mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: spike, High: safe, Low: spike,
		MarkPx: safe, Ts: 2})
	if len(step.Liquidations) != 0 {
		t.Errorf("标记价未越线时不应强平，实际 %+v", step.Liquidations)
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); !ok {
		t.Error("插针不该扫掉仓位")
	}
}

// TestLiquidationAfterFundingErodesMargin 资金费侵蚀保证金后可触发强平。
//
// 这条串起了资金费与强平：长期持有的仓位即便价格没怎么动，也可能被资金费耗到爆仓。
func TestLiquidationAfterFundingErodesMargin(t *testing.T) {
	s, _ := liqSim(t, types.Buy, "100")
	pos, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	t.Logf("开仓保证金 %s", pos.Margin)

	// 价格纹丝不动，只靠资金费消耗保证金
	var step StepResult
	for i := int64(0); i < 40; i++ {
		b := bar("78000", "78000", "78000", 2+i)
		b.Funding = &Funding{Rate: dec("0.005"), Px: dec("78000")}
		step = mustAdvance(t, s, b)
		if len(step.Liquidations) > 0 {
			t.Logf("第 %d 期资金费后触发强平", i+1)
			break
		}
	}
	if len(step.Liquidations) == 0 {
		p, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
		m, _ := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
		t.Fatalf("资金费应最终耗到强平，当前保证金 %s 保证金率 %s", p.Margin, m.MgnRatio)
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); ok {
		t.Error("强平后仓位应被移除")
	}
}

func TestLiquidationInLongShortMode(t *testing.T) {
	s := newSim(t, types.LongShortMode)
	if err := s.SetLeverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosLong,
		decimal.NewFromInt(100)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLeverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosShort,
		decimal.NewFromInt(100)); err != nil {
		t.Fatal(err)
	}
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	long := Fill{InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosLong, Sz: dec("5"), Px: dec("78000"), ExecType: types.Taker, Ts: 1}
	short := long
	short.Side = types.Sell
	short.PosSide = types.PosShort
	mustFill(t, s, long)
	mustFill(t, s, short)

	lm, err := s.MetricsOf("BTC-USDT-SWAP", types.PosLong)
	if err != nil {
		t.Fatal(err)
	}
	// 价格下跌只应爆掉多头，空头反而获利
	hit := lm.LiqPx.Sub(dec("1"))
	step := mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, High: hit, Low: hit, Ts: 2})
	if len(step.Liquidations) != 1 {
		t.Fatalf("应只爆多头一侧，实际 %d 次", len(step.Liquidations))
	}
	if step.Liquidations[0].PosSide != types.PosLong {
		t.Errorf("被强平的应是多头，实际 %q", step.Liquidations[0].PosSide)
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosShort); !ok {
		t.Error("空头不该被牵连")
	}
}

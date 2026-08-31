package okxsim

import (
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
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
	t.Logf("强平：成交价 %s，损失保证金 %s，罚金 %s，超出 %s",
		liq.Px, liq.Loss, liq.Penalty, liq.Excess)
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

// TestLiquidationExcessScalesWithGap 亏损超出保证金的幅度随跳空幅度增大。
//
// 常态强平也会有小幅超出——触发与成交之间价格总会再走一点。真实观测印证了这点：
// 一次 BTC 空头强平的超出为 0.0602，占保证金 1.1715 的 5%，OKX 以一笔单独的
// 账单退了回来。所以「常态超出为零」是错的期望。
//
// 有意义的判据是量级：跳空穿过破产价时的超出应当远大于常态滑移，
// 这才是回测中值得警觉的信号。
func TestLiquidationExcessScalesWithGap(t *testing.T) {
	// 常态：刚跌破强平价
	s1, m1 := liqSim(t, types.Buy, "100")
	hit := m1.LiqPx.Sub(dec("1"))
	normal := mustAdvance(t, s1, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, High: hit, Low: hit, Ts: 3}).Liquidations[0]

	// 跳空：直接跌到破产价下方
	s2, m2 := liqSim(t, types.Buy, "100")
	gap := m2.BkPx.Sub(dec("200"))
	gapped := mustAdvance(t, s2, Bar{
		InstID: "BTC-USDT-SWAP", Last: gap, High: gap, Low: gap, Ts: 3}).Liquidations[0]

	t.Logf("常态：成交价 %s 超出 %s；跳空：成交价 %s 超出 %s（保证金 %s）",
		normal.Px, normal.Excess, gapped.Px, gapped.Excess, normal.Loss)

	if !gapped.Excess.GreaterThan(normal.Excess.Mul(decimal.NewFromInt(10))) {
		t.Errorf("跳空的超出 %s 应远大于常态的 %s", gapped.Excess, normal.Excess)
	}
	// 两种情形下持仓方的损失都封顶为保证金
	eq(t, normal.Loss, gapped.Loss.String(), "损失均封顶为保证金")
	// 成交价始终是触发时的市价
	eq(t, gapped.Px, gap.String(), "按触发时的市价成交")
}

// TestLiquidationPenaltyIsMaintenanceMargin 爆仓罚金等于名义价值乘以维持保证金率。
//
// 实测：一次 BTC 空头强平的 liqPenalty 为 0.47139948，名义价值 117.84987，
// 两者之比恰为 0.004，正是该档位的 mmr。
func TestLiquidationPenaltyIsMaintenanceMargin(t *testing.T) {
	s, m := liqSim(t, types.Buy, "100")
	hit := m.LiqPx.Sub(dec("1"))
	liq := mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, High: hit, Low: hit, Ts: 3}).Liquidations[0]

	inst, err := refdata.MustEmbedded().Instrument("BTC-USDT-SWAP")
	if err != nil {
		t.Fatal(err)
	}
	nom := notional(inst, liq.Sz, liq.Px)
	want := nom.Mul(m.MMRRate).Neg()
	eq(t, liq.Penalty, want.String(), "爆仓罚金")
	if liq.Penalty.IsZero() {
		t.Error("罚金不应为零——OKX 会照收，它是 liqPenalty 字段的来源")
	}
	t.Logf("名义价值 %s × mmr %s = 罚金 %s", nom, m.MMRRate, liq.Penalty)
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

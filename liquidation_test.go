package okxsim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		InstID: "BTC-USDT-SWAP", Last: safe, MarkPx: safe, High: safe, Low: safe, Ts: 2})
	if len(step.Liquidations) != 0 {
		t.Fatalf("强平价上方不应触发，实际 %+v", step.Liquidations)
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); !ok {
		t.Fatal("仓位不该消失")
	}

	// 跌破强平价，触发
	hit := m.LiqPx.Sub(dec("1"))
	step = mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, MarkPx: hit, High: hit, Low: hit, Ts: 3})
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
		InstID: "BTC-USDT-SWAP", Last: hit, MarkPx: hit, High: hit, Low: hit, Ts: 3})
	if len(step.Liquidations) != 1 {
		t.Fatalf("应触发强平")
	}

	eq(t, step.Liquidations[0].Loss, margin.String(), "损失应为全部保证金")
	eq(t, s.CashBal("USDT"), cashBefore.String(), "强平不应改变现金余额")

	b, err := s.BalanceOf("USDT")
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
	if len(s.PendingOrders("")) != 1 {
		t.Fatal("委托未挂上")
	}

	hit := m.LiqPx.Sub(dec("1"))
	step := mustAdvance(t, s, Bar{
		InstID: "BTC-USDT-SWAP", Last: hit, MarkPx: hit, High: hit, Low: hit, Ts: 3})
	if len(step.Liquidations) != 1 {
		t.Fatalf("应触发强平，实际 %+v", step)
	}
	if got := step.Liquidations[0].CanceledOrders; len(got) != 1 || got[0] != "o1" {
		t.Errorf("强平应撤销挂单，实际 %v", got)
	}
	if len(s.PendingOrders("")) != 0 {
		t.Error("强平后不应还有挂单")
	}
	b, err := s.BalanceOf("USDT")
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
		InstID: "BTC-USDT-SWAP", Last: hit, MarkPx: hit, High: hit, Low: hit, Ts: 3}).Liquidations[0]

	// 跳空：直接跌到破产价下方
	s2, m2 := liqSim(t, types.Buy, "100")
	gap := m2.BkPx.Sub(dec("200"))
	gapped := mustAdvance(t, s2, Bar{
		InstID: "BTC-USDT-SWAP", Last: gap, MarkPx: gap, High: gap, Low: gap, Ts: 3}).Liquidations[0]

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
		InstID: "BTC-USDT-SWAP", Last: hit, MarkPx: hit, High: hit, Low: hit, Ts: 3}).Liquidations[0]

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
		InstID: "BTC-USDT-SWAP", Last: hit, MarkPx: hit, High: hit, Low: hit, Ts: 3})
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
			InstID: "BTC-USDT-SWAP", Last: dec(px), MarkPx: dec(px), High: dec(px), Low: dec(px),
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
		InstID: "BTC-USDT-SWAP", Last: hit, MarkPx: hit, High: hit, Low: hit, Ts: 2})
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

// tieredFixture 是 testdata/conformance/liquidation-tiered-isolated.json 的结构。
type tieredFixture struct {
	Instrument     json.RawMessage `json:"instrument"`
	Tiers          json.RawMessage `json:"tiers"`
	FeeRate        struct{ Maker, Taker string }
	PositionBefore struct {
		InstID, PosSide, Pos, AvgPx, Lever, Margin, LiqPx string
	} `json:"positionBefore"`
	Bills []struct {
		SubType   string `json:"subType"`
		Sz        string `json:"sz"`
		Px        string `json:"px"`
		Pnl       string `json:"pnl"`
		Fee       string `json:"fee"`
		PosBalChg string `json:"posBalChg"`
		PosBal    string `json:"posBal"`
		BalChg    string `json:"balChg"`
	} `json:"bills"`
}

// TestTieredLiquidationAgainstRealEvent 用一次【真实的阶梯减仓】逐项对拍。
//
// 阶梯减仓此前是本项目仅存的几条未实测路径之一——触发它需要跨越多个档位的仓位，
// 在模拟盘上布好二档纵深的仓位等了一段时间，行情自己走了过去。
//
// 真实序列是 部分强平(101) -> 全部强平(105) -> 穿仓补偿(108)：第一次减到一档上限
// 后价格继续走，第二次才全平。这正是本库实现的链路。
func TestTieredLiquidationAgainstRealEvent(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "conformance",
		"liquidation-tiered-isolated.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx tieredFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatal(err)
	}
	var tiers []refdata.PositionTier
	if err := json.Unmarshal(fx.Tiers, &tiers); err != nil {
		t.Fatal(err)
	}
	tbl, err := refdata.NewTierTable(refdata.TierKey{
		InstType: types.InstSwap, MgnMode: types.MgnIsolated,
		Family: inst.InstFamily}, tiers)
	if err != nil {
		t.Fatal(err)
	}
	snap := refdata.NewSnapshotBuilder(1).AddInstruments(inst).AddTierTable(tbl).
		SetFeeSchedule(refdata.DefaultFeeSchedule().WithRate(types.InstSwap,
			refdata.FeeRate{Maker: dec(fx.FeeRate.Maker), Taker: dec(fx.FeeRate.Taker)})).
		Build()

	s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit("USDT", dec("100000")); err != nil {
		t.Fatal(err)
	}
	pb := fx.PositionBefore
	if err := s.SetPosition(Position{
		InstID: pb.InstID, MgnMode: types.MgnIsolated, PosSide: types.PosShort,
		Pos: dec(pb.Pos), AvgPx: dec(pb.AvgPx), Lever: dec(pb.Lever),
		Margin: dec(pb.Margin),
	}); err != nil {
		t.Fatal(err)
	}

	// 两次真实的强平价与本库的两步一一对应
	var stages []struct {
		SubType   string
		Sz        string
		Px        string
		Pnl       string
		Fee       string
		PosBalChg string
	}
	for _, bl := range fx.Bills {
		if bl.SubType == "101" || bl.SubType == "105" {
			stages = append(stages, struct {
				SubType   string
				Sz        string
				Px        string
				Pnl       string
				Fee       string
				PosBalChg string
			}{bl.SubType, bl.Sz, bl.Px, bl.Pnl, bl.Fee, bl.PosBalChg})
		}
	}
	if len(stages) != 2 {
		t.Fatalf("夹具里应有两段强平，实为 %d 段", len(stages))
	}

	var totalLoss, totalExcess decimal.Decimal
	for i, st := range stages {
		// 把标记价推到真实成交价，触发这一段
		step, err := s.Advance(Bar{
			InstID: pb.InstID, Last: dec(st.Px), High: dec(st.Px), Low: dec(st.Px),
			MarkPx: dec(st.Px), Ts: int64(i + 1),
		})
		if err != nil {
			t.Fatalf("第 %d 段推进失败: %v", i+1, err)
		}
		if len(step.Liquidations) != 1 {
			t.Fatalf("第 %d 段应当发生一次强平，实为 %d 次", i+1, len(step.Liquidations))
		}
		l := step.Liquidations[0]

		wantKind := LiqPartial
		if st.SubType == "105" {
			wantKind = LiqFull
		}
		if l.Kind != wantKind {
			t.Errorf("第 %d 段的类型 = %s，OKX 的 subType=%s 对应 %s",
				i+1, l.Kind, st.SubType, wantKind)
		}
		eq(t, l.Sz, st.Sz, fmt.Sprintf("第 %d 段的张数", i+1))
		near(t, l.Pnl, dec(st.Pnl), "0.0000001", fmt.Sprintf("第 %d 段的盈亏", i+1))
		near(t, l.Fee, dec(st.Fee), "0.0000001", fmt.Sprintf("第 %d 段的手续费", i+1))

		// posBalChg = 盈亏 + 手续费 + 罚金，是仓位保证金的净变化
		wantChg := dec(st.PosBalChg)
		gotChg := l.Pnl.Add(l.Fee).Add(l.Penalty)
		near(t, gotChg, wantChg, "0.0000001",
			fmt.Sprintf("第 %d 段的仓位保证金变化（含罚金）", i+1))

		totalLoss = totalLoss.Add(l.Loss)
		totalExcess = totalExcess.Add(l.Excess)
	}

	// 损失封顶为仓位保证金，超出部分退回——真实账单里是一条 subType=108 的补偿
	near(t, totalLoss, dec(pb.Margin), "0.0000001", "两段合计的损失应恰好等于仓位保证金")
	var wantExcess decimal.Decimal
	for _, bl := range fx.Bills {
		if bl.SubType == "108" {
			wantExcess = dec(bl.Sz)
		}
	}
	near(t, totalExcess, wantExcess, "0.0000001", "由风险准备金退回的超额部分")

	if _, ok := s.PositionOf(pb.InstID, types.PosShort); ok {
		t.Error("两段之后仓位应当已全平")
	}
}

// TestTieredReductionPenaltyUsesLowerTier 锁定罚金按【减仓后】的档位收。
//
// 这是真实事件与本库原实现唯一对不上的地方：减仓前在二档（mmr 0.015）、减仓后在
// 一档（0.01），实际罚金 170.225 / 名义 17022.5 = 0.01。按减仓前算会多收 85.11。
func TestTieredReductionPenaltyUsesLowerTier(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "conformance",
		"liquidation-tiered-isolated.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx tieredFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	for _, bl := range fx.Bills {
		if bl.SubType != "101" {
			continue
		}
		// 罚金 = −(posBalChg − pnl − fee)
		pen := dec(bl.PosBalChg).Sub(dec(bl.Pnl)).Sub(dec(bl.Fee)).Neg()
		nom := dec(bl.Sz).Mul(dec("0.1")).Mul(dec(bl.Px))
		rate := div(pen, nom)
		eq(t, rate.Round(6), "0.01", "阶梯减仓的罚金率（= 减仓后的一档，不是减仓前的二档）")
		// 按减仓前的 0.015 会多收多少
		eq(t, nom.Mul(dec("0.015")).Sub(pen).Round(4), "85.1125", "按减仓前算会多出的罚金")
		return
	}
	t.Fatal("夹具里没有 subType=101 的部分强平账单")
}

// TestCrossLiquidationAgainstRealEvent 回放一次真实的全仓强平。
//
// 2026-09-01 的 ETH-USD-SWAP 全仓多头爆仓，账单链与逐仓**不是同一套**：
//
//	                  逐仓          全仓
//	阶梯减仓          subType 101   subType 100
//	全部强平          subType 105   subType 104
//	损失落点          仓位保证金    现金余额
//	账单的 pnl 字段   纯盈亏        **含罚金**
//
// 最后一行是解析上的陷阱：逐仓账单的 pnl 与公式逐位相同，罚金要靠保证金对账反推；
// 全仓账单的 pnl 把罚金折了进去。本测试用的是**拆开之后**的量，拆法记在夹具里。
//
// 这次事件还照出了一处此前算错的：罚金率按【本笔平掉的张数】自己查档。减 17577 张
// 用二档 0.005，而减仓后的 8000 张在一档 0.004——「按减仓后档位」被排除。
func TestCrossLiquidationAgainstRealEvent(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "liquidation-cross.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx struct {
		Instrument json.RawMessage `json:"instrument"`
		Tiers      json.RawMessage `json:"tiers"`
		CashBefore string          `json:"cashBefore"`
		AvgPx      string          `json:"avgPx"`
		PosBefore  string          `json:"posBefore"`
		Bills      []struct {
			SubType string `json:"subType"`
			Sz      string `json:"sz"`
			BalChg  string `json:"balChg"`
			Bal     string `json:"bal"`
		} `json:"bills"`
		Decomposed []struct {
			Sz          string `json:"sz"`
			Px          string `json:"px"`
			RealizedPnl string `json:"realizedPnl"`
			Penalty     string `json:"penalty"`
			BillFee     string `json:"billFee"`
			PenaltyTier string `json:"penaltyTier"`
		} `json:"decomposed"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	if len(fx.Decomposed) != 2 {
		t.Fatalf("夹具应当有两段（减仓 + 全平），实为 %d 段", len(fx.Decomposed))
	}
	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatal(err)
	}
	var tiers []refdata.PositionTier
	if err := json.Unmarshal(fx.Tiers, &tiers); err != nil {
		t.Fatal(err)
	}
	tbl, err := refdata.NewTierTable(refdata.TierKey{
		InstType: types.InstSwap, MgnMode: types.MgnCross, Family: inst.InstFamily}, tiers)
	if err != nil {
		t.Fatal(err)
	}
	snap := refdata.NewSnapshotBuilder(1).AddInstruments(inst).AddTierTable(tbl).
		SetFeeSchedule(refdata.DefaultFeeSchedule()).Build()
	s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit(inst.SettleCcy, dec(fx.CashBefore)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPosition(Position{
		InstID: inst.InstID, MgnMode: types.MgnCross, PosSide: types.PosLong,
		Pos: dec(fx.PosBefore), AvgPx: dec(fx.AvgPx), Lever: dec("66"),
	}); err != nil {
		t.Fatal(err)
	}

	// 第一段：阶梯减仓，25577 -> 8000（一档上限）
	st := fx.Decomposed[0]
	if err := s.SetMarkPx(inst.InstID, dec(st.Px)); err != nil {
		t.Fatal(err)
	}
	liqs, err := s.CheckLiquidation(inst.InstID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(liqs) != 1 {
		t.Fatalf("这一步应当只有一笔阶梯减仓，实为 %d 笔", len(liqs))
	}
	l := liqs[0]
	if l.Kind != LiqPartial {
		t.Errorf("应当是阶梯减仓，实为 %v", l.Kind)
	}
	eq(t, l.Sz, st.Sz, "减掉的张数")
	near(t, l.Pnl, dec(st.RealizedPnl), "1e-15", "减仓的真实盈亏")
	near(t, l.Fee, dec(st.BillFee), "1e-15", "减仓的手续费")
	near(t, l.Penalty, dec(st.Penalty), "1e-15",
		"减仓的罚金——按【平掉的张数】查档，第"+st.PenaltyTier+"档")
	p, _ := s.PositionOf(inst.InstID, types.PosLong)
	eq(t, p.Pos, "8000", "减到一档上限")
	near(t, s.CashBal(inst.SettleCcy), dec(fx.Bills[0].Bal), "1e-15",
		"减仓后的现金应与账单的 bal 一致")

	// 第二段：仍未获救，全平
	st = fx.Decomposed[1]
	if err := s.SetMarkPx(inst.InstID, dec(st.Px)); err != nil {
		t.Fatal(err)
	}
	liqs, err = s.CheckLiquidation(inst.InstID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(liqs) != 1 {
		t.Fatalf("这一步应当只有一笔全平，实为 %d 笔", len(liqs))
	}
	l = liqs[0]
	if l.Kind != LiqFull {
		t.Errorf("应当是全部强平，实为 %v", l.Kind)
	}
	eq(t, l.Sz, "8000", "全平的张数")
	near(t, l.Penalty, dec(st.Penalty), "1e-15",
		"全平的罚金——8000 张落在第"+st.PenaltyTier+"档")
	if q, ok := s.PositionOf(inst.InstID, types.PosLong); ok && !q.IsEmpty() {
		t.Errorf("应当已全平，实剩 %s 张", q.Pos)
	}

	// 封顶：现金恰好归零，跌破的部分记为超额
	eq(t, s.CashBal(inst.SettleCcy), "0", "现金封顶归零，不会变成负数")
	if !l.Excess.IsPositive() {
		t.Errorf("这次是穿仓，应当有超额由风险准备金承担，实为 %s", l.Excess)
	}
	near(t, l.Excess, dec(fx.Bills[1].Bal).Neg(), "1e-15",
		"超额应等于账单里那个负余额，随后由 subType 108 补回")
	// 损失总额恰好等于该币种原有的现金
	var loss decimal.Decimal
	for _, x := range []Liquidation{{Loss: dec(fx.Bills[0].BalChg).Neg()}, {Loss: l.Loss}} {
		loss = loss.Add(x.Loss)
	}
	near(t, loss, dec(fx.CashBefore), "1e-15", "损失总额封顶为该币种的现金")
}

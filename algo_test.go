package okxsim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func algoSim(t *testing.T) *Simulator {
	t.Helper()
	s := newSim(t, types.LongShortMode)
	// 先给一根行情，让参考价有着落
	if _, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("78000"),
		High: dec("78000"), Low: dec("78000"), Ts: 1}); err != nil {
		t.Fatalf("推进行情失败: %v", err)
	}
	return s
}

func placeTrigger(t *testing.T, s *Simulator, id, triggerPx, ordPx string) {
	t.Helper()
	a := AlgoOrder{
		AlgoID: id, InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosLong, OrdType: types.AlgoTrigger,
		Sz: dec("1"), TriggerPx: dec(triggerPx),
	}
	if ordPx != "" {
		a.OrdPx = dec(ordPx)
	}
	if _, err := s.PlaceAlgoOrder(a); err != nil {
		t.Fatalf("挂算法单失败: %v", err)
	}
}

// TestAlgoOrderFreezesNothing 锁定算法委托不占用任何资金。
//
// 这是算法单与普通委托最实质的区别，也是实测确认的：在模拟盘挂四张计划委托，
// availBal / ordFrozen / imr / mmr 全部纹丝不动。把它当普通挂单去冻结资金，
// 回测里会凭空少掉一大块可用额度。
func TestAlgoOrderFreezesNothing(t *testing.T) {
	s := algoSim(t)
	before, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}

	for i, px := range []string{"70000", "72000", "85000", "90000"} {
		placeTrigger(t, s, string(rune('a'+i)), px, "")
	}
	if n := len(s.PendingAlgos("")); n != 4 {
		t.Fatalf("应有 4 笔算法委托，实为 %d", n)
	}

	after, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		got, want decimal.Decimal
	}{
		{"可用余额", after.AvailBal, before.AvailBal},
		{"挂单冻结", after.OrdFrozen, before.OrdFrozen},
		{"初始保证金", after.IMR, before.IMR},
		{"维持保证金", after.MMR, before.MMR},
		{"冻结余额", after.FrozenBal, before.FrozenBal},
	} {
		if !c.got.Equal(c.want) {
			t.Errorf("挂了四张算法单后 %s 从 %s 变成 %s，算法单不应占用任何资金",
				c.name, c.want, c.got)
		}
	}
}

// TestTriggerFiresAndFillsAtTriggerPx 计划委托触发后当场以触发价成交。
//
// ordPx 为 -1（市价）时成交价取触发价——触发那一刻的市价就是触发价，这是本模型
// 能给出的最准的价。
func TestTriggerFiresAndFillsAtTriggerPx(t *testing.T) {
	s := algoSim(t)
	placeTrigger(t, s, "buy-the-dip", "76000", "")

	// 没触及：不动
	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("77000"),
		High: dec("77500"), Low: dec("76500"), Ts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 0 {
		t.Fatalf("最低价 76500 未触及 76000，不应触发，实际 %d 笔", len(step.AlgoTriggers))
	}

	// 触及：本步就该成交
	step, err = s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("76200"),
		High: dec("77000"), Low: dec("75800"), Ts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 1 {
		t.Fatalf("最低价 75800 已触及 76000，应触发一笔，实际 %d 笔", len(step.AlgoTriggers))
	}
	tr := step.AlgoTriggers[0]
	if tr.Reason != "" {
		t.Fatalf("不该有失败原因: %s", tr.Reason)
	}
	eq(t, tr.Px, "76000", "触发价")
	if tr.Leg != "trigger" {
		t.Errorf("触发的腿 = %q，期望 trigger", tr.Leg)
	}
	if tr.Fill == nil {
		t.Fatal("市价委托应当当场成交")
	}
	eq(t, tr.Fill.After.AvgPx, "76000", "成交价应等于触发价——开仓均价即为它")
	if len(step.Fills) != 1 {
		t.Errorf("本步成交应出现在 Fills 里，实际 %d 笔", len(step.Fills))
	}
	if len(s.PendingAlgos("")) != 0 {
		t.Error("触发后该算法委托应当消失")
	}
	p, ok := s.PositionOf("BTC-USDT-SWAP", types.PosLong)
	if !ok {
		t.Fatal("触发后应当建仓")
	}
	eq(t, p.Pos, "1", "持仓张数")
	eq(t, p.AvgPx, "76000", "开仓均价")
}

// TestTriggerWithLimitPxPlacesOrder 触发后带委托价的转成限价挂单，走正常撮合。
func TestTriggerWithLimitPxPlacesOrder(t *testing.T) {
	s := algoSim(t)
	placeTrigger(t, s, "a1", "76000", "75000")

	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("76200"),
		High: dec("77000"), Low: dec("75800"), Ts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 1 || step.AlgoTriggers[0].Reason != "" {
		t.Fatalf("应触发一笔且无失败原因，实际 %+v", step.AlgoTriggers)
	}
	if step.AlgoTriggers[0].Fill != nil {
		t.Error("限价委托 75000 高于本步最低价 75800，不该当场成交")
	}
	pend := s.PendingOrders("")
	if len(pend) != 1 {
		t.Fatalf("应留下一笔限价挂单，实际 %d 笔", len(pend))
	}
	eq(t, pend[0].Order.Px, "75000", "挂单价")
	if pend[0].OrdID != "a1:trigger" {
		t.Errorf("生成的委托 ID = %q，应能看出它来自哪笔算法委托的哪条腿", pend[0].OrdID)
	}
	// 这笔挂单占用资金，与算法单阶段的零占用形成对照
	b, _ := s.BalanceOf("USDT")
	if !b.OrdFrozen.IsPositive() {
		t.Error("转成普通挂单后应当开始冻结资金")
	}
}

// TestConditionalKeepsOnlyOneLeg 锁定 conditional 只认一条腿。
//
// 实测同时提交 tp 与 sl 两组参数时，OKX 只保留 sl，另一组被丢弃且不报错。
// 要两条腿并存须用 oco。
func TestConditionalKeepsOnlyOneLeg(t *testing.T) {
	s := algoSim(t)
	mustPos(t, s, "2", "78000")

	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "c1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosLong, OrdType: types.AlgoConditional,
		Sz: dec("2"), ReduceOnly: true,
		TpTriggerPx: dec("82000"), SlTriggerPx: dec("74000"),
	}); err != nil {
		t.Fatalf("挂止盈止损失败: %v", err)
	}

	// 涨到 82000：止盈那条腿已被丢弃，不该触发
	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("83000"),
		High: dec("83000"), Low: dec("81000"), Ts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 0 {
		t.Errorf("conditional 只保留止损那条腿，涨到 82000 不该触发，实际 %+v",
			step.AlgoTriggers)
	}

	// 跌到 74000：止损那条腿触发
	step, err = s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("74500"),
		High: dec("76000"), Low: dec("73500"), Ts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 1 || step.AlgoTriggers[0].Leg != "sl" {
		t.Fatalf("应触发止损腿，实际 %+v", step.AlgoTriggers)
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosLong); ok {
		t.Error("止损成交后仓位应被平掉")
	}
}

// TestOCOFirstLegVoidsTheOther 锁定 oco 的两条腿只能生效一条。
func TestOCOFirstLegVoidsTheOther(t *testing.T) {
	s := algoSim(t)
	mustPos(t, s, "2", "78000")

	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "o1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosLong, OrdType: types.AlgoOCO,
		Sz: dec("2"), ReduceOnly: true,
		TpTriggerPx: dec("82000"), SlTriggerPx: dec("74000"),
	}); err != nil {
		t.Fatalf("挂 OCO 失败: %v", err)
	}

	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("83000"),
		High: dec("83000"), Low: dec("81000"), Ts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 1 || step.AlgoTriggers[0].Leg != "tp" {
		t.Fatalf("应触发止盈腿，实际 %+v", step.AlgoTriggers)
	}
	eq(t, step.AlgoTriggers[0].Px, "82000", "止盈触发价")
	if len(s.PendingAlgos("")) != 0 {
		t.Error("一条腿触发后整笔 OCO 都应作废")
	}
}

// TestTrailingStopRatchets 锁定移动止损的棘轮：触发价只朝一个方向走。
//
// 实测 40 次采样中，平多的卖单其触发价零回退、平空的买单零回升。
func TestTrailingStopRatchets(t *testing.T) {
	s := algoSim(t)
	mustPos(t, s, "2", "78000")

	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "m1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosLong, OrdType: types.AlgoMoveStop,
		Sz: dec("2"), ReduceOnly: true, CallbackRatio: dec("0.05"),
	}); err != nil {
		t.Fatalf("挂移动止损失败: %v", err)
	}
	// 挂单瞬间的触发价就是「当时价格 × (1 − 回调)」，不需要先出现一段有利行情
	pa, ok := s.PendingAlgoOf("m1")
	if !ok {
		t.Fatal("查不到该算法委托")
	}
	eq(t, pa.TriggerPx, "74100", "挂单时的触发价 = 78000 × 0.95")

	// 涨：触发价跟着往上棘轮
	if _, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("80000"),
		High: dec("80000"), Low: dec("79000"), Ts: 2}); err != nil {
		t.Fatal(err)
	}
	pa1, _ := s.PendingAlgoOf("m1")
	eq(t, pa1.TriggerPx, "76000", "涨到 80000 后触发价 = 80000 × 0.95")

	// 回落但未触及：触发价不回退
	if _, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("77000"),
		High: dec("78000"), Low: dec("76500"), Ts: 3}); err != nil {
		t.Fatal(err)
	}
	pa2, _ := s.PendingAlgoOf("m1")
	eq(t, pa2.TriggerPx, "76000", "回落时触发价不应回退")

	// 跌破：触发
	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("75500"),
		High: dec("77000"), Low: dec("75000"), Ts: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 1 || step.AlgoTriggers[0].Leg != "move" {
		t.Fatalf("跌破 76000 应触发移动止损，实际 %+v", step.AlgoTriggers)
	}
	eq(t, step.AlgoTriggers[0].Px, "76000", "移动止损的触发价")
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosLong); ok {
		t.Error("移动止损成交后仓位应被平掉")
	}
}

// TestTrailingStopShortSide 平空的移动止损跟最低价往下棘轮。
func TestTrailingStopShortSide(t *testing.T) {
	s := algoSim(t)
	if _, err := s.Fill(Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Sell,
		PosSide: types.PosShort, Sz: dec("2"), Px: dec("78000"),
		ExecType: types.Taker, Ts: 1,
	}); err != nil {
		t.Fatalf("开空失败: %v", err)
	}
	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "m2", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosShort, OrdType: types.AlgoMoveStop,
		Sz: dec("2"), ReduceOnly: true, CallbackRatio: dec("0.05"),
	}); err != nil {
		t.Fatalf("挂移动止损失败: %v", err)
	}
	pa0, _ := s.PendingAlgoOf("m2")
	eq(t, pa0.TriggerPx, "81900", "挂单时的触发价 = 78000 × 1.05")

	// 跌：触发价跟着往下棘轮
	if _, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("74000"),
		High: dec("77000"), Low: dec("74000"), Ts: 2}); err != nil {
		t.Fatal(err)
	}
	pa1, _ := s.PendingAlgoOf("m2")
	eq(t, pa1.TriggerPx, "77700", "跌到 74000 后触发价 = 74000 × 1.05")

	// 反弹但未触及：不回升
	if _, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("76000"),
		High: dec("77000"), Low: dec("75000"), Ts: 3}); err != nil {
		t.Fatal(err)
	}
	pa2, _ := s.PendingAlgoOf("m2")
	eq(t, pa2.TriggerPx, "77700", "反弹时触发价不应回升")
}

// TestTriggerPxTypeMark 触发价类型为 mark 时按标记价判，而不是最新价。
//
// 两者可以差得很远：插针时最新价扫过触发价而标记价没动，用错价格会让本不该触发的
// 委托被扫掉——这与强平判据用标记价而非最新价是同一个道理。
func TestTriggerPxTypeMark(t *testing.T) {
	s := algoSim(t)
	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "mk", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosLong, OrdType: types.AlgoTrigger,
		Sz: dec("1"), TriggerPx: dec("76000"), TriggerPxType: types.TriggerMark,
	}); err != nil {
		t.Fatalf("挂算法单失败: %v", err)
	}

	// 最新价插针到 75000，但标记价稳在 77500：不该触发
	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("77000"),
		High: dec("78000"), Low: dec("75000"), MarkPx: dec("77500"), Ts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 0 {
		t.Errorf("最新价插针不应触发按标记价判的委托，实际 %+v", step.AlgoTriggers)
	}

	// 标记价真的跌下去：触发
	step, err = s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("76500"),
		High: dec("77500"), Low: dec("76000"), MarkPx: dec("75800"), Ts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 1 {
		t.Fatalf("标记价跌破 76000 应触发，实际 %+v", step.AlgoTriggers)
	}
}

// TestTriggerPxTypeIndexNeedsIdxPx 指数价未提供时应当说明原因，而不是悄悄跳过。
//
// 本库不拿最新价或标记价去顶替指数价，那是另一个价格，顶替会让触发点系统性偏移。
func TestTriggerPxTypeIndexNeedsIdxPx(t *testing.T) {
	s := algoSim(t)
	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "ix", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosLong, OrdType: types.AlgoTrigger,
		Sz: dec("1"), TriggerPx: dec("76000"), TriggerPxType: types.TriggerIndex,
	}); err != nil {
		t.Fatalf("挂算法单失败: %v", err)
	}

	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("75000"),
		High: dec("77000"), Low: dec("74000"), Ts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 1 || step.AlgoTriggers[0].Reason == "" {
		t.Fatalf("缺少指数价时应给出原因而不是悄悄跳过，实际 %+v", step.AlgoTriggers)
	}
	if len(s.PendingAlgos("")) != 1 {
		t.Error("无从判断时该委托应当留着，而不是被消耗掉")
	}

	// 给了指数价就能判
	step, err = s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("75000"),
		High: dec("77000"), Low: dec("74000"), IdxPx: dec("75500"), Ts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 1 || step.AlgoTriggers[0].Reason != "" {
		t.Fatalf("给了指数价 75500 应当触发，实际 %+v", step.AlgoTriggers)
	}
}

// TestAlgoOrderCancel 撤销算法委托。
func TestAlgoOrderCancel(t *testing.T) {
	s := algoSim(t)
	placeTrigger(t, s, "x1", "76000", "")
	if err := s.CancelAlgoOrder("x1"); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if len(s.PendingAlgos("")) != 0 {
		t.Error("撤销后不应还有算法委托")
	}
	if err := s.CancelAlgoOrder("x1"); err == nil {
		t.Error("重复撤销应当报错")
	}

	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("74000"),
		High: dec("77000"), Low: dec("73000"), Ts: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(step.AlgoTriggers) != 0 {
		t.Error("已撤销的委托不该再触发")
	}
}

// TestAlgoTriggerReportsShortfall 触发时资金不足要说清楚，而不是静默失败。
func TestAlgoTriggerReportsShortfall(t *testing.T) {
	s := algoSim(t)
	// 把钱几乎提光，留下的不够开这笔仓
	if err := s.Withdraw("USDT", dec("9990")); err != nil {
		t.Fatalf("出金失败: %v", err)
	}
	placeTrigger(t, s, "poor", "76000", "")

	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("75000"),
		High: dec("77000"), Low: dec("74000"), Ts: 2})
	if err != nil {
		t.Fatalf("推进行情不应失败: %v", err)
	}
	if len(step.AlgoTriggers) != 1 {
		t.Fatalf("应触发一笔，实际 %d", len(step.AlgoTriggers))
	}
	if step.AlgoTriggers[0].Reason == "" {
		t.Error("资金不足应当在 Reason 里说清楚")
	}
	if step.AlgoTriggers[0].Fill != nil {
		t.Error("下单失败时不应有成交")
	}
	if _, ok := s.PositionOf("BTC-USDT-SWAP", types.PosLong); ok {
		t.Error("下单失败时不应建仓")
	}
}

func mustPos(t *testing.T, s *Simulator, sz, px string) {
	t.Helper()
	if _, err := s.Fill(Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosLong, Sz: dec(sz), Px: dec(px),
		ExecType: types.Taker, Ts: 1,
	}); err != nil {
		t.Fatalf("建仓失败: %v", err)
	}
}

// TestAlgoTriggerDescribe 触发与「触发后下单失败」必须能分辨。
//
// 两者混为一谈会让使用者以为「没触发」，实际是触发了却下不出单——这是回测里
// 最难查的一类问题：什么都没发生，也不知道为什么。
func TestAlgoTriggerDescribe(t *testing.T) {
	s := algoSim(t)
	placeTrigger(t, s, "ok1", "76000", "")
	step, err := s.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("76000"),
		High: dec("77000"), Low: dec("75800"), Ts: 2})
	if err != nil {
		t.Fatal(err)
	}
	got := step.AlgoTriggers[0].String()
	for _, want := range []string{"ok1", "计划委托", "76000", "当场成交"} {
		if !contains(got, want) {
			t.Errorf("描述 %q 应当包含 %q", got, want)
		}
	}
	if !contains(step.Describe(), "ok1") {
		t.Errorf("StepResult.Describe 应当带上算法委托的触发: %s", step.Describe())
	}

	// 下单失败的那一支要说清楚是「触发了但没下成」
	s2 := algoSim(t)
	if err := s2.Withdraw("USDT", dec("9990")); err != nil {
		t.Fatal(err)
	}
	placeTrigger(t, s2, "poor1", "76000", "")
	step2, err := s2.Advance(Bar{InstID: "BTC-USDT-SWAP", Last: dec("76000"),
		High: dec("77000"), Low: dec("75800"), Ts: 2})
	if err != nil {
		t.Fatal(err)
	}
	got2 := step2.AlgoTriggers[0].String()
	if !contains(got2, "触发") || !contains(got2, "下单失败") {
		t.Errorf("描述 %q 应当同时说明「触发了」与「下单失败」", got2)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// algoFixture 是 testdata/conformance/algo-orders.json 的结构。
type algoFixture struct {
	TriggerLifecycle struct {
		Before      map[string]any `json:"before"`
		After       map[string]any `json:"after"`
		LinkedOrder map[string]any `json:"linkedOrder"`
	} `json:"triggerLifecycle"`
	Conditional   map[string]any `json:"conditional"`
	OCO           map[string]any `json:"oco"`
	TrailingTrack struct {
		CallbackRatio string `json:"callbackRatio"`
		Samples       []struct {
			Last string `json:"last"`
			Hi   string `json:"hi"`
			Lo   string `json:"lo"`
			Long *struct {
				State         string `json:"state"`
				MoveTriggerPx string `json:"moveTriggerPx"`
			} `json:"long"`
			Short *struct {
				State         string `json:"state"`
				MoveTriggerPx string `json:"moveTriggerPx"`
			} `json:"short"`
		} `json:"samples"`
	} `json:"trailingTrack"`
}

func loadAlgoFixture(t *testing.T) algoFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "algo-orders.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx algoFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	return fx
}

// TestAlgoAgainstRealOKX 用模拟盘上的真实响应锁定算法委托的行为。
//
// 这些是行为规则而非数值公式，因此比对的是状态机与字段形态：触发前后 state 怎么变、
// 触发是否生成一笔普通委托、conditional 保留了哪条腿、oco 保留了几条。
func TestAlgoAgainstRealOKX(t *testing.T) {
	fx := loadAlgoFixture(t)

	t.Run("触发生成一笔普通委托", func(t *testing.T) {
		if got := fx.TriggerLifecycle.Before["state"]; got != "live" {
			t.Errorf("触发前 state = %v，期望 live", got)
		}
		if got := fx.TriggerLifecycle.After["state"]; got != "effective" {
			t.Errorf("触发后 state = %v，期望 effective", got)
		}
		ordID, _ := fx.TriggerLifecycle.After["ordId"].(string)
		if ordID == "" {
			t.Fatal("触发后应当带出一笔普通委托的 ID")
		}
		lo := fx.TriggerLifecycle.LinkedOrder
		if lo["ordType"] != "market" || lo["state"] != "filled" {
			t.Errorf("生成的委托应是已成交的市价单，实为 ordType=%v state=%v",
				lo["ordType"], lo["state"])
		}
		// 真实成交价比触发价低一个 tickSz——那是本库不建模的滑点。
		// 差距若变大，说明 OKX 的撮合行为变了，值得重新看一眼。
		trig := dec(fx.TriggerLifecycle.After["triggerPx"].(string))
		avg := dec(lo["avgPx"].(string))
		if diff := trig.Sub(avg).Abs(); diff.GreaterThan(dec("0.1")) {
			t.Errorf("成交价 %s 与触发价 %s 相差 %s，超出一个 tickSz 的量级——"+
				"本库以触发价成交的假设可能不再成立", avg, trig, diff)
		}
	})

	t.Run("conditional 只保留一条腿", func(t *testing.T) {
		c := fx.Conditional
		if c["tpTriggerPx"] != "" {
			t.Errorf("conditional 的止盈腿应被丢弃，实为 %v", c["tpTriggerPx"])
		}
		if c["slTriggerPx"] == "" {
			t.Error("conditional 应保留止损腿")
		}
	})

	t.Run("oco 两条腿都在", func(t *testing.T) {
		o := fx.OCO
		if o["tpTriggerPx"] == "" || o["slTriggerPx"] == "" {
			t.Errorf("oco 应保留两条腿，实为 tp=%v sl=%v",
				o["tpTriggerPx"], o["slTriggerPx"])
		}
	})
}

// TestTrailingStopAgainstRealTrack 用 40 次真实采样锁定移动止损的棘轮。
//
// 采样是 4 秒一次，而 OKX 逐笔跟踪，因此我按采样估的极值必然不如它极端：
// 平多那条腿的真实触发价应当【不低于】我的估计，平空那条应当【不高于】。
// 这个方向性本身就是「跟极值」的证据——若它跟的是别的东西，偏差不会单边。
func TestTrailingStopAgainstRealTrack(t *testing.T) {
	fx := loadAlgoFixture(t)
	ratio := dec(fx.TrailingTrack.CallbackRatio)
	one := decimal.NewFromInt(1)
	if len(fx.TrailingTrack.Samples) < 10 {
		t.Fatalf("采样太少（%d），锁不住", len(fx.TrailingTrack.Samples))
	}

	var prevLong, prevShort decimal.Decimal
	for i, sm := range fx.TrailingTrack.Samples {
		if sm.Long != nil && sm.Long.State == "live" && sm.Long.MoveTriggerPx != "" {
			got := dec(sm.Long.MoveTriggerPx)
			// 本库的公式：极值 × (1 − ratio)
			est := dec(sm.Hi).Mul(one.Sub(ratio))
			if got.LessThan(est.Sub(dec("0.01"))) {
				t.Errorf("第 %d 次采样：平多的触发价 %s 低于按采样最高价算出的 %s，"+
					"跟极值这条规则不成立", i, got, est)
			}
			if prevLong.IsPositive() && got.LessThan(prevLong) {
				t.Errorf("第 %d 次采样：平多的触发价从 %s 回退到 %s，棘轮不应回退",
					i, prevLong, got)
			}
			prevLong = got
		}
		if sm.Short != nil && sm.Short.State == "live" && sm.Short.MoveTriggerPx != "" {
			got := dec(sm.Short.MoveTriggerPx)
			est := dec(sm.Lo).Mul(one.Add(ratio))
			if got.GreaterThan(est.Add(dec("0.01"))) {
				t.Errorf("第 %d 次采样：平空的触发价 %s 高于按采样最低价算出的 %s，"+
					"跟极值这条规则不成立", i, got, est)
			}
			if prevShort.IsPositive() && got.GreaterThan(prevShort) {
				t.Errorf("第 %d 次采样：平空的触发价从 %s 回升到 %s，棘轮不应回升",
					i, prevShort, got)
			}
			prevShort = got
		}
	}
}

// TestCloseFractionOnlyAcceptsOne 锁定 closeFraction 的取值。
//
// 它的名字像个比例，实际不是：实测 0.5、0.3、2 一律被 OKX 以 51000
// 「Parameter closeFraction error」拒绝，只有 1 通过。**本库照样只收 1**——
// 让回测里写错的策略拿到与实盘同一个错误，比悄悄按比例平掉更有用。
//
// 库里原先把这条缺口描述成「未建模按仓位比例平仓」，那个前提本身就是错的：
// OKX 根本不提供按比例平仓。
func TestCloseFractionOnlyAcceptsOne(t *testing.T) {
	for _, bad := range []string{"0.5", "0.3", "2", "0.99"} {
		s := newSim(t, types.NetMode)
		mustFill(t, s, netFill(types.Buy, "2", "78000"))
		_, err := s.PlaceAlgoOrder(AlgoOrder{
			AlgoID: "a", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
			Side: types.Sell, PosSide: types.PosNet, OrdType: types.AlgoConditional,
			TpTriggerPx: dec("80000"), CloseFraction: dec(bad), Ts: 1,
		})
		if !okxerr.HasCode(err, okxerr.CodeParamError) {
			t.Errorf("closeFraction=%s 应当被拒，实为 %v", bad, err)
		}
	}
	// 与 sz 互斥
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "2", "78000"))
	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "b", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosNet, OrdType: types.AlgoConditional,
		TpTriggerPx: dec("80000"), CloseFraction: dec("1"), Sz: dec("1"), Ts: 1,
	}); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("closeFraction 与 sz 同时给应当被拒，实为 %v", err)
	}
}

// TestCloseFractionResolvesSizeAtTrigger 锁定这一条里最要紧的：张数何时确定。
//
// 实测：下单时持仓 200 张，随后加到 300 张，触发后成交 **300 张**（accFillSz=300），
// 不是下单时的 200。下单时算法委托的 sz 读回是空串。
//
// 对网格这类持仓不断变动的策略是实质差别——按下单时定量会在加仓之后留下一截
// 平不掉的尾巴，而那截尾巴在回测里会一直带着，直到策略以为自己已经空仓。
func TestCloseFractionResolvesSizeAtTrigger(t *testing.T) {
	s := newSim(t, types.NetMode)
	if err := s.Deposit("USDT", dec("500000")); err != nil {
		t.Fatal(err)
	}
	mustFill(t, s, netFill(types.Buy, "2", "78000"))
	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "tp", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosNet, OrdType: types.AlgoConditional,
		TpTriggerPx: dec("79000"), CloseFraction: dec("1"), Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// 下单之后加仓：真实行为是触发时按【当时】的持仓平掉
	mustFill(t, s, netFill(types.Buy, "3", "78500"))
	p, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, p.Pos, "5", "加仓后的持仓")

	res, err := s.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: dec("79200"), High: dec("79300"),
		Low: dec("78400"), MarkPx: dec("79200"), Ts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AlgoTriggers) != 1 {
		t.Fatalf("应当触发一笔，实为 %d 笔", len(res.AlgoTriggers))
	}
	if len(res.Fills) != 1 {
		t.Fatalf("应当成交一笔，实为 %d 笔", len(res.Fills))
	}
	eq(t, res.Fills[0].Fill.Sz, "5",
		"应当按【触发时】的持仓 5 张平掉，而不是下单时的 2 张")
	eq(t, res.Fills[0].ClosedSz, "5", "全部平掉")
	q, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	if ok && !q.IsEmpty() {
		t.Errorf("closeFraction=1 应当全平，实剩 %s 张", q.Pos)
	}
}

// TestCloseFractionSkipsWhenFlat 触发时已经没有持仓就什么都不做。
//
// 不报错：这与「触发后的限价单挂不出去」同样处理——算法委托的职责是触发，
// 触发之后的世界已经变了是常态，不是异常。
func TestCloseFractionSkipsWhenFlat(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "2", "78000"))
	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "tp", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosNet, OrdType: types.AlgoConditional,
		TpTriggerPx: dec("79000"), CloseFraction: dec("1"), Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	mustFill(t, s, netFill(types.Sell, "2", "78500")) // 策略自己先平了

	res, err := s.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: dec("79200"), High: dec("79300"),
		Low: dec("78400"), MarkPx: dec("79200"), Ts: 2,
	})
	if err != nil {
		t.Fatalf("已空仓时触发不该报错: %v", err)
	}
	if len(res.Fills) != 0 {
		t.Errorf("已空仓，不该产生成交，实为 %d 笔", len(res.Fills))
	}
}

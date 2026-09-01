package okxsim

import (
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func req(side types.Side, sz, px string) OrderReq {
	return OrderReq{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: side,
		PosSide: types.PosNet, Sz: dec(sz), Px: dec(px),
	}
}

func limitOrder(ordID string, side types.Side, sz, px string) Order {
	return Order{
		OrdID: ordID, InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: side, PosSide: types.PosNet, OrdType: types.OrdLimit,
		Sz: dec(sz), Px: dec(px),
	}
}

// TestOrderCostOpen 开仓挂单的冻结 = 张数 × (每张保证金 + 每张 taker 手续费)。
//
// 实测取自模拟盘：4 张 BTC-USDT-SWAP，挂单价 70339.23，5 倍杠杆，
// 可用余额减少 564.120624600000，与本式差值恰为 0。
//
// 本测试用的是内置快照（取自生产环境，tickSz=0.1），而实测时模拟盘的
// tickSz 是 0.01——两个环境的规则数据本就不同。70339.23 在生产精度下会被
// 取整为 70339.2，故期望值按同一公式重算；公式本身未变。
func TestOrderCostOpen(t *testing.T) {
	s := newSim(t, types.NetMode)

	c, err := s.OrderCost(req(types.Buy, "4", "70339.2"))
	if err != nil {
		t.Fatalf("计算挂单成本失败: %v", err)
	}

	// 名义价值 0.01×4×70339.2 = 2813.568
	eq(t, c.Notional, "2813.568", "名义价值")
	eq(t, c.Margin, "562.7136", "冻结保证金") // /5
	eq(t, c.Fee, "1.406784", "预冻结手续费")   // ×0.0005
	eq(t, c.Frozen, "564.120384", "合计冻结")
	eq(t, c.OpenSz, "4", "开仓张数")
	eq(t, c.CloseSz, "0", "平仓张数")
	if c.Ccy != "USDT" {
		t.Errorf("冻结币种 = %q", c.Ccy)
	}

	// 直接核对实测值：同一公式在模拟盘的 70339.23 上应给出 564.1206246
	n := dec("0.01").Mul(dec("4")).Mul(dec("70339.23"))
	got := div(n, dec("5")).Add(n.Mul(dec("0.0005")))
	eq(t, got, "564.1206246", "同一公式在实测价位上的冻结额")
}

// TestOrderCostRoundsPrice 报价与下单必须按同一个取整后的价格计算，
// 否则「按报价刚好挂得起」的委托会在真下单时被拒。
func TestOrderCostRoundsPrice(t *testing.T) {
	s := newSim(t, types.NetMode)

	// tickSz=0.1，买单向下取整到 70339.2
	quoted, err := s.OrderCost(req(types.Buy, "4", "70339.23"))
	if err != nil {
		t.Fatal(err)
	}
	aligned, err := s.OrderCost(req(types.Buy, "4", "70339.2"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, quoted.Frozen, aligned.Frozen.String(), "超精度价格的报价应与取整后一致")

	pr, err := s.PlaceOrder(limitOrder("o1", types.Buy, "4", "70339.23"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, pr.Cost.Frozen, quoted.Frozen.String(), "实际冻结应与报价一致")

	orders := s.PendingOrders("BTC-USDT-SWAP")
	if len(orders) != 1 {
		t.Fatalf("挂单数 = %d", len(orders))
	}
	eq(t, orders[0].Order.Px, "70339.2", "挂住的委托价应已按 tickSz 取整")
}

// TestOrderCostUsesTakerRate 预冻结一律按 taker 费率，即便该委托必然作为 maker 成交。
//
// 实测那笔远离市价、必然挂住的限价单也是按 taker 冻结的——OKX 取保守值。
func TestOrderCostUsesTakerRate(t *testing.T) {
	s := newSim(t, types.NetMode)

	c, err := s.OrderCost(req(types.Buy, "4", "70339.2"))
	if err != nil {
		t.Fatal(err)
	}
	// taker 0.0005 -> 1.406784；若误用 maker 0.0002 会得到 0.5627136
	eq(t, c.Fee, "1.406784", "预冻结手续费应按 taker 费率")
}

// TestOrderCostCloseIsFree 平仓方向的挂单不冻结任何资金，实测确认。
func TestOrderCostCloseIsFree(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))

	c, err := s.OrderCost(req(types.Sell, "2", "86000"))
	if err != nil {
		t.Fatalf("计算平仓挂单成本失败: %v", err)
	}
	if !c.Frozen.IsZero() || !c.Margin.IsZero() || !c.Fee.IsZero() {
		t.Errorf("平仓挂单不应产生冻结，实际 %+v", c)
	}
	eq(t, c.CloseSz, "2", "平仓张数")
	eq(t, c.OpenSz, "0", "开仓张数")
}

// TestOrderCostReversalSplitsSize 反手委托拆成平仓与开仓两段，只对开仓段冻结。
func TestOrderCostReversalSplitsSize(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))

	// 卖 10 张：先平掉 4 张，再反向开 6 张
	c, err := s.OrderCost(req(types.Sell, "10", "78000"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, c.CloseSz, "4", "平仓段张数")
	eq(t, c.OpenSz, "6", "开仓段张数")
	// 只对 6 张冻结：0.01×6×78000 = 4680，/5 = 936，费 4680×0.0005 = 2.34
	eq(t, c.Margin, "936", "只对开仓段冻结保证金")
	eq(t, c.Fee, "2.34", "只对开仓段预冻结手续费")
	eq(t, c.Frozen, "938.34", "合计冻结")
}

func TestOrderCostAffordable(t *testing.T) {
	s := newSim(t, types.NetMode)

	c, err := s.OrderCost(req(types.Buy, "4", "70339.2"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Affordable(dec("564.120384")) {
		t.Error("恰好等于冻结额时应当挂得起")
	}
	if c.Affordable(dec("564.120383")) {
		t.Error("差一分钱时不应挂得起")
	}
}

// TestMaxSizePriceRule 取价规则：买入按委托价，卖出取委托价与标记价中更保守的一侧。
//
// 七组价格实测全部命中该规则。这个不对称是 OKX 自身的行为——委托价高于标记价的
// 买单本会立即以标记价附近成交、所需保证金更少，OKX 仍按委托价计算。
func TestMaxSizePriceRule(t *testing.T) {
	s := newSim(t, types.NetMode)
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("78000")); err != nil {
		t.Fatal(err)
	}

	// 委托价低于标记价：买入按委托价（张数更多），卖出按标记价（更保守）
	low, err := s.MaxSize("BTC-USDT-SWAP", types.TdIsolated, dec("70000"))
	if err != nil {
		t.Fatal(err)
	}
	if !low.MaxBuy.GreaterThan(low.MaxSell) {
		t.Errorf("委托价低于标记价时，maxBuy(%s) 应大于 maxSell(%s)", low.MaxBuy, low.MaxSell)
	}

	// 委托价高于标记价：两侧都按委托价，因为它更保守
	high, err := s.MaxSize("BTC-USDT-SWAP", types.TdIsolated, dec("86000"))
	if err != nil {
		t.Fatal(err)
	}
	if !high.MaxBuy.Equal(high.MaxSell) {
		t.Errorf("委托价高于标记价时两侧应相等，实际 maxBuy=%s maxSell=%s",
			high.MaxBuy, high.MaxSell)
	}
	if !high.MaxBuy.LessThan(low.MaxBuy) {
		t.Errorf("价格越高可开张数应越少，实际 %s vs %s", high.MaxBuy, low.MaxBuy)
	}

	// 委托价等于标记价：两侧相等
	same, err := s.MaxSize("BTC-USDT-SWAP", types.TdIsolated, dec("78000"))
	if err != nil {
		t.Fatal(err)
	}
	if !same.MaxBuy.Equal(same.MaxSell) {
		t.Errorf("委托价等于标记价时两侧应相等，实际 %s vs %s", same.MaxBuy, same.MaxSell)
	}
}

// TestMaxSizeValue 可开张数 = 可用余额 / (每张保证金 + 每张手续费)，按 lotSz 向下取整。
func TestMaxSizeValue(t *testing.T) {
	s, err := New(Config{
		PosMode: types.NetMode, RefData: mustEmbedded(t), DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit("USDT", dec("10000")); err != nil {
		t.Fatal(err)
	}

	m, err := s.MaxSize("BTC-USDT-SWAP", types.TdIsolated, dec("78000"))
	if err != nil {
		t.Fatal(err)
	}
	// 每张 = 0.01×78000/5 + 0.01×78000×0.0005 = 156 + 0.39 = 156.39
	// 10000 / 156.39 = 63.9427…  -> 按 lotSz 0.01 向下取整 = 63.94
	eq(t, m.MaxBuy, "63.94", "最大可买张数")

	// 用算出的张数挂单应当恰好挂得起，再多一档就挂不起
	c, err := s.OrderCost(req(types.Buy, m.MaxBuy.String(), "78000"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Affordable(dec("10000")) {
		t.Errorf("按 MaxSize 算出的张数应当挂得起，冻结 %s", c.Frozen)
	}
	c, err = s.OrderCost(req(types.Buy, m.MaxBuy.Add(dec("0.01")).String(), "78000"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Affordable(dec("10000")) {
		t.Errorf("比 MaxSize 多一档就不该挂得起，冻结 %s", c.Frozen)
	}
}

// TestPlaceOrderFreezesFunds 挂单后可用余额减少，撤单后恢复。
func TestPlaceOrderFreezesFunds(t *testing.T) {
	s := newSim(t, types.NetMode)

	before, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}

	pr, err := s.PlaceOrder(limitOrder("o1", types.Buy, "4", "70339.2"))
	if err != nil {
		t.Fatalf("挂单失败: %v", err)
	}
	if pr.State != types.OrdLive {
		t.Errorf("委托状态 = %q，期望挂住", pr.State)
	}
	cost := pr.Cost
	eq(t, cost.Frozen, "564.120384", "冻结额")

	after, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	// 现金不动，可用余额减少冻结额
	eq(t, after.CashBal, before.CashBal.String(), "挂单不动用现金")
	eq(t, after.AvailBal, before.AvailBal.Sub(cost.Frozen).String(), "可用余额减少冻结额")
	// ordFrozen 只是保证金部分，不含手续费——与 OKX 的字段语义一致
	eq(t, after.OrdFrozen, "562.7136", "ordFrozen 只含保证金")
	eq(t, after.FrozenBal, cost.Frozen.String(), "frozenBal 含保证金与手续费")

	if err := s.CancelOrder("o1"); err != nil {
		t.Fatalf("撤单失败: %v", err)
	}
	restored, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, restored.AvailBal, before.AvailBal.String(), "撤单后可用余额应恢复")
	eq(t, restored.OrdFrozen, "0", "撤单后 ordFrozen 归零")
}

// TestMaxSizeAccountsForPendingOrders 有未成交委托时可开张数应当相应减少。
//
// 实测：挂出 4 张后 OKX 的 maxBuy 从 33.76 降到 29.76。
func TestMaxSizeAccountsForPendingOrders(t *testing.T) {
	s := newSim(t, types.NetMode)
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("78000")); err != nil {
		t.Fatal(err)
	}

	before, err := s.MaxSize("BTC-USDT-SWAP", types.TdIsolated, dec("70000"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceOrder(limitOrder("o1", types.Buy, "4", "70000")); err != nil {
		t.Fatal(err)
	}
	after, err := s.MaxSize("BTC-USDT-SWAP", types.TdIsolated, dec("70000"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.MaxBuy.LessThan(before.MaxBuy) {
		t.Errorf("有挂单后可开张数应当减少，实际 %s -> %s", before.MaxBuy, after.MaxBuy)
	}
	// 减少的量应当约等于已挂的 4 张
	drop := before.MaxBuy.Sub(after.MaxBuy)
	if drop.Sub(dec("4")).Abs().GreaterThan(dec("0.02")) {
		t.Errorf("可开张数减少 %s，期望约 4 张", drop)
	}
}

func TestPlaceOrderInsufficientBalance(t *testing.T) {
	// 显式声明买卖模式：这条验的是余额不足，与持仓方式无关，
	// 而默认值自 v1.1.0 起是开平仓模式。
	s, err := New(Config{PosMode: types.NetMode, RefData: mustEmbedded(t),
		DefaultLever: decimal.NewFromInt(5)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit("USDT", dec("100")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceOrder(limitOrder("o1", types.Buy, "4", "78000")); !okxerr.HasCode(err, okxerr.CodeInsufficientBal) {
		t.Errorf("余额不足的错误 = %v，期望 51008", err)
	}
	if len(s.PendingOrders("")) != 0 {
		t.Error("挂单失败不应留下记录")
	}
}

func TestPlaceOrderDuplicateID(t *testing.T) {
	s := newSim(t, types.NetMode)
	if _, err := s.PlaceOrder(limitOrder("o1", types.Buy, "1", "70000")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceOrder(limitOrder("o1", types.Buy, "1", "70000")); err == nil {
		t.Error("重复的委托 ID 应当被拒")
	}
	if err := s.CancelOrder("不存在"); err == nil {
		t.Error("撤销不存在的委托应当报错")
	}
}

// TestPreviewFillDoesNotMutate 预演不得改变任何状态，且结果必须与真实成交一致。
func TestPreviewFillDoesNotMutate(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "70000"))

	cashBefore := s.CashBal("USDT")
	posBefore, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)

	f := netFill(types.Sell, "10", "75000")
	preview, err := s.PreviewFill(f)
	if err != nil {
		t.Fatalf("预演失败: %v", err)
	}

	// 状态不得改变
	eq(t, s.CashBal("USDT"), cashBefore.String(), "预演不应动用现金")
	posAfter, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, posAfter.Pos, posBefore.Pos.String(), "预演不应改变持仓")
	eq(t, posAfter.Margin, posBefore.Margin.String(), "预演不应改变保证金")

	// 预演结果应与真实成交一致
	real, err := s.Fill(f)
	if err != nil {
		t.Fatalf("成交失败: %v", err)
	}
	if !preview.Reversed || !real.Reversed {
		t.Error("这笔委托应当触发反手")
	}
	eq(t, preview.Pnl, real.Pnl.String(), "预演的盈亏应与真实成交一致")
	eq(t, preview.Fee, real.Fee.String(), "预演的手续费应与真实成交一致")
	eq(t, preview.After.Pos, real.After.Pos.String(), "预演的持仓应与真实成交一致")
	eq(t, preview.After.AvgPx, real.After.AvgPx.String(), "预演的均价应与真实成交一致")
	eq(t, preview.After.Margin, real.After.Margin.String(), "预演的保证金应与真实成交一致")
}

func TestOrderCostErrors(t *testing.T) {
	s := newSim(t, types.NetMode)

	bad := req(types.Buy, "4", "70000")
	bad.InstID = "NOPE-USDT-SWAP"
	if _, err := s.OrderCost(bad); !okxerr.HasCode(err, okxerr.CodeInstNotExist) {
		t.Errorf("未知合约的错误 = %v，期望 51001", err)
	}

	bad = req(types.Buy, "0.015", "70000")
	if _, err := s.OrderCost(bad); !okxerr.HasCode(err, okxerr.CodeNotLotSizeMultiple) {
		t.Errorf("非整数倍数量的错误 = %v，期望 51121", err)
	}

	bad = req(types.Buy, "4", "0")
	if _, err := s.OrderCost(bad); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("零价格的错误 = %v，期望 51000", err)
	}
}

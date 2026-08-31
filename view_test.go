package okxsim

import (
	"encoding/json"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// ---- 逐仓保证金增减 ----

func TestAdjustMarginAdd(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78112.5655"))

	cashBefore := s.CashBal("USDT")
	pos, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	marginBefore := pos.Margin

	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet, types.MarginAdd, dec("200")); err != nil {
		t.Fatalf("追加保证金失败: %v", err)
	}

	// 一比一划转，经实测确认
	eq(t, s.CashBal("USDT"), cashBefore.Sub(dec("200")).String(), "追加后现金")
	pos, _ = s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, pos.Margin, marginBefore.Add(dec("200")).String(), "追加后保证金")
	eq(t, pos.Lever, "5", "追加保证金不改变杠杆设置")
}

// TestAdjustMarginMakesPositionSafer 追加保证金后强平价应当远离、保证金率应当上升。
func TestAdjustMarginMakesPositionSafer(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78112.5655"))
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("78116.13")); err != nil {
		t.Fatal(err)
	}

	before, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet, types.MarginAdd, dec("200")); err != nil {
		t.Fatal(err)
	}
	after, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}

	if !after.LiqPx.LessThan(before.LiqPx) {
		t.Errorf("多头追加保证金后强平价应当下移，实际 %s -> %s", before.LiqPx, after.LiqPx)
	}
	if !after.MgnRatio.GreaterThan(before.MgnRatio) {
		t.Errorf("追加保证金后保证金率应当上升，实际 %s -> %s", before.MgnRatio, after.MgnRatio)
	}
	t.Logf("强平价 %s -> %s，保证金率 %s -> %s",
		before.LiqPx.Round(2), after.LiqPx.Round(2),
		before.MgnRatio.Round(4), after.MgnRatio.Round(4))
}

// TestAdjustMarginReduceFloor 减少保证金的下限是开仓时的初始保证金。
//
// 越过下限时 OKX 返回 59301，本实现与之一致。实测序列：4 张仓位追加 200 后
// 保证金 824.9，减 100 剩 724.9 可以，再减 200 会低于开仓初始保证金 624.900524
// 而被拒。
func TestAdjustMarginReduceFloor(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78112.5655"))

	pos, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	floor := pos.Margin // 开仓时的初始保证金
	eq(t, floor, "624.900524", "开仓初始保证金")

	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet, types.MarginAdd, dec("200")); err != nil {
		t.Fatal(err)
	}
	// 减到恰好等于下限是允许的
	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet, types.MarginReduce, dec("200")); err != nil {
		t.Fatalf("减到下限被拒: %v", err)
	}
	pos, _ = s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, pos.Margin, floor.String(), "减到下限后的保证金")

	// 再减一分钱就该被拒
	err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet, types.MarginReduce, dec("0.01"))
	if !okxerr.HasCode(err, okxerr.CodeMarginAdjustExceeds) {
		t.Errorf("越过下限的错误 = %v，期望 59301", err)
	}
	// 被拒后状态不应改变
	pos, _ = s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, pos.Margin, floor.String(), "被拒后保证金不应变动")
}

func TestAdjustMarginErrors(t *testing.T) {
	s := newSim(t, types.NetMode)

	// 无持仓
	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet, types.MarginAdd, dec("100")); err == nil {
		t.Error("对不存在的仓位调整保证金应当报错")
	}

	mustFill(t, s, netFill(types.Buy, "1", "78000"))

	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet, "bogus", dec("100")); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("非法方向的错误 = %v，期望 51000", err)
	}
	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet, types.MarginAdd, dec("-1")); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("负数金额的错误 = %v，期望 51000", err)
	}
	if err := s.AdjustMargin("BTC-USDT-SWAP", types.PosNet, types.MarginAdd, dec("999999")); !okxerr.HasCode(err, okxerr.CodeInsufficientBal) {
		t.Errorf("余额不足的错误 = %v，期望 51008", err)
	}
}

// ---- 与 OKX 响应字段级同构 ----

// TestPositionViewMatchesOKXShape 视图的字段集必须与 OKX 的仓位响应一致。
//
// 字段名取自真实响应，逐一核对——少一个字段，使用者拿它替换真实 API 时就会
// 在那个字段上拿到零值而非空串，静默地把「无此值」变成「值为零」。
func TestPositionViewMatchesOKXShape(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("77500")); err != nil {
		t.Fatal(err)
	}

	views, err := s.PositionViews()
	if err != nil {
		t.Fatalf("生成仓位视图失败: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("视图数 = %d，期望 1", len(views))
	}

	b, err := json.Marshal(views[0])
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	// 取自 OKX 真实响应的字段名
	want := []string{
		"instType", "instId", "posId", "posSide", "mgnMode", "pos", "availPos",
		"posCcy", "ccy", "avgPx", "bePx", "markPx", "liqPx", "idxPx", "last",
		"lever", "margin", "mgnRatio", "imr", "mmr", "upl", "uplRatio",
		"realizedPnl", "pnl", "fee", "fundingFee", "liqPenalty", "notionalUsd",
		"adl", "usdPx", "cTime", "uTime", "tradeId",
	}
	for _, f := range want {
		if _, ok := got[f]; !ok {
			t.Errorf("视图缺少 OKX 字段 %q", f)
		}
	}

	// 所有值必须是字符串——OKX 的 JSON 里数值都是字符串
	for k, v := range got {
		if _, ok := v.(string); !ok {
			t.Errorf("字段 %q 的值类型为 %T，OKX 的线格式里数值都是字符串", k, v)
		}
	}
}

// TestPositionViewEmptyMeansNoValue 无值的字段必须是空串而非 "0"。
//
// OKX 用空串区分「无此值」与「值为零」。逐仓的 imr 恒为空串，把它写成 "0"
// 会让下游误以为初始保证金真的是零。
func TestPositionViewEmptyMeansNoValue(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))

	views, err := s.PositionViews()
	if err != nil {
		t.Fatal(err)
	}
	v := views[0]

	if v.Imr != "" {
		t.Errorf("逐仓的 imr = %q，应为空串", v.Imr)
	}
	if v.PosCcy != "" {
		t.Errorf("正向合约的 posCcy = %q，应为空串", v.PosCcy)
	}
	// 模拟器拿不到的字段一律留空。
	// bePx 曾在此列，v0.5.0 起可算——公式已由真实仓位标定，见 breakEvenPx。
	for name, val := range map[string]string{
		"posId": v.PosID, "idxPx": v.IdxPx, "last": v.Last,
		"adl": v.Adl, "usdPx": v.UsdPx, "tradeId": v.TradeID,
	} {
		if val != "" {
			t.Errorf("模拟器无从得知的字段 %s = %q，应为空串", name, val)
		}
	}

	// 能算的字段必须有值
	if v.Margin == "" || v.AvgPx == "" || v.Pos == "" || v.Lever == "" || v.BePx == "" {
		t.Errorf("可计算的字段不应为空: %+v", v)
	}
}

func TestBalanceViewShape(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))

	views, err := s.BalanceViews()
	if err != nil {
		t.Fatalf("生成余额视图失败: %v", err)
	}
	if len(views) != 1 || views[0].Ccy != "USDT" {
		t.Fatalf("余额视图 = %+v", views)
	}

	b, _ := json.Marshal(views[0])
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ccy", "eq", "cashBal", "availBal", "availEq",
		"frozenBal", "ordFrozen", "isoEq", "isoUpl", "upl", "disEq"} {
		if _, ok := got[f]; !ok {
			t.Errorf("余额视图缺少 OKX 字段 %q", f)
		}
	}
}

func TestInverseViewHasPosCcy(t *testing.T) {
	s, err := New(Config{
		PosMode: types.NetMode, RefData: mustEmbedded(t),
		DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit("BTC", dec("1")); err != nil {
		t.Fatal(err)
	}
	f := Fill{
		InstID: "BTC-USD-SWAP", TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosNet, Sz: dec("1"), Px: dec("78000"),
		ExecType: types.Taker, Ts: 1,
	}
	if _, err := s.Fill(f); err != nil {
		t.Fatalf("反向合约成交失败: %v", err)
	}
	views, err := s.PositionViews()
	if err != nil {
		t.Fatal(err)
	}
	if views[0].PosCcy != "USD" {
		t.Errorf("反向合约的 posCcy = %q，期望标的币 USD", views[0].PosCcy)
	}
	if views[0].Ccy != "BTC" {
		t.Errorf("反向合约的保证金币种 = %q，期望 BTC", views[0].Ccy)
	}
}

// TestCrossPositionViewFieldShape 锁定全仓仓位视图的字段形态。
//
// margin 与 imr 恰好互补：逐仓给 margin、imr 空，全仓给 imr、margin 空。
// 这不是设计取舍，是照 OKX 的实际返回抄的——字段级同构是本项目的验收标准之一。
func TestCrossPositionViewFieldShape(t *testing.T) {
	fx := loadCrossFixture(t)
	s, err := New(Config{PosMode: types.LongShortMode, RefData: crossSnapshot(t, fx)})
	if err != nil {
		t.Fatalf("新建模拟器失败: %v", err)
	}
	if err := s.Deposit("USDT", dec("10000")); err != nil {
		t.Fatalf("入金失败: %v", err)
	}
	if err := s.SetPosition(Position{
		InstID: "ETH-USDT-SWAP", MgnMode: types.MgnCross, PosSide: types.PosLong,
		Pos: dec("2"), AvgPx: dec("2445.6"), Lever: dec("10"),
	}); err != nil {
		t.Fatalf("置入仓位失败: %v", err)
	}
	s.SetMarkPx("ETH-USDT-SWAP", dec("2445.25"))

	views, err := s.PositionViews()
	if err != nil {
		t.Fatal(err)
	}
	v := views[0]
	if v.Margin != "" {
		t.Errorf("全仓的 margin = %q，应为空串——那笔钱从未离开现金余额", v.Margin)
	}
	if v.Imr == "" {
		t.Error("全仓的 imr 应有值")
	}
	if v.MgnMode != "cross" {
		t.Errorf("mgnMode = %q", v.MgnMode)
	}
	// 现金远厚于仓位，强平价够不着，OKX 此时返回空串
	if v.LiqPx != "" {
		t.Errorf("强平价够不着时应为空串，实为 %q", v.LiqPx)
	}

	bvs, err := s.BalanceViews()
	if err != nil {
		t.Fatal(err)
	}
	b := bvs[0]
	if b.Imr == "" || b.Mmr == "" || b.MgnRatio == "" {
		t.Errorf("有全仓持仓时币种级的 imr/mmr/mgnRatio 都应有值: %+v", b)
	}
	if b.IsoEq != "0" {
		t.Errorf("没有逐仓持仓时 isoEq = %q，应为 0", b.IsoEq)
	}
}

// TestPositionViewCoversMeasuredFieldShape 锁定视图里「零」与「空串」的区分。
//
// OKX 两者含义不同，而且并不一致：仓位的 liqPenalty / pnl / fundingFee 没有时是
// "0"，imr / liqPx 没有时是空串；余额的 imr / mmr 是 "0"，mgnRatio 却是空串。
// 早先一律按「零就留空」处理，字段级对拍一跑就露馅了——这条测试把当时的教训钉住。
func TestPositionViewCoversMeasuredFieldShape(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))

	views, err := s.PositionViews()
	if err != nil {
		t.Fatal(err)
	}
	v := views[0]
	for name, got := range map[string]string{
		"liqPenalty": v.LiqPenalty, "pnl": v.Pnl, "fundingFee": v.FundingFee,
	} {
		if got == "" {
			t.Errorf("%s 没有值时 OKX 给的是 \"0\" 而非空串，实为空串", name)
		}
	}
	if v.Imr != "" {
		t.Errorf("逐仓的 imr = %q，OKX 给的是空串", v.Imr)
	}

	bvs, err := s.BalanceViews()
	if err != nil {
		t.Fatal(err)
	}
	b := bvs[0]
	if b.Imr == "" || b.Mmr == "" {
		t.Errorf("余额的 imr/mmr 没有全仓持仓时 OKX 给的是 \"0\"，实为 %q/%q", b.Imr, b.Mmr)
	}
	if b.MgnRatio != "" {
		t.Errorf("没有全仓持仓时 mgnRatio OKX 给的是空串，实为 %q", b.MgnRatio)
	}
}

// TestUplByLastPx 锁定按最新价与按标记价的两套浮盈。
//
// OKX 两套都给：强平判据用标记价那套，而回测通常以成交价撮合，与回测口径对得上的
// 是最新价那套。两者在插针时能差出一亏一赚——实测同一仓位 upl 为 -34.50 而
// uplLastPx 为 +0.50。
func TestUplByLastPx(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))

	// 标记价下跌、最新价上涨：两套应当一亏一赚
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("77000")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastPx("BTC-USDT-SWAP", dec("79000")); err != nil {
		t.Fatal(err)
	}
	m, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, m.UPL, "-40", "按标记价的浮盈")
	eq(t, m.UplLastPx, "40", "按最新价的浮盈")
	if !m.UPLRatio.IsNegative() || !m.UplRatioLastPx.IsPositive() {
		t.Errorf("两套收益率应当一负一正，实为 %s 与 %s", m.UPLRatio, m.UplRatioLastPx)
	}

	// 没推送过最新价时留空，不拿标记价顶替
	s2 := newSim(t, types.NetMode)
	mustFill(t, s2, netFill(types.Buy, "4", "78000"))
	m2, err := s2.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	if !m2.UplLastPx.IsZero() {
		t.Errorf("没有最新价时不应凭空算出 uplLastPx，实为 %s", m2.UplLastPx)
	}
}

// TestSetIndexPx 指数价是独立的一条行情，不与最新价、标记价混用。
func TestSetIndexPx(t *testing.T) {
	s := newSim(t, types.NetMode)
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("77000")); err != nil {
		t.Fatal(err)
	}
	if s.IndexPx("BTC-USDT-SWAP").IsPositive() {
		t.Error("只设了标记价，指数价不该有值")
	}
	if err := s.SetIndexPx("BTC-USDT-SWAP", dec("77100")); err != nil {
		t.Fatal(err)
	}
	eq(t, s.IndexPx("BTC-USDT-SWAP"), "77100", "指数价")
	eq(t, s.MarkPx("BTC-USDT-SWAP"), "77000", "标记价不应被指数价冲掉")
	if err := s.SetIndexPx("BTC-USDT-SWAP", dec("0")); err == nil {
		t.Error("非正数的指数价应当被拒绝")
	}
}

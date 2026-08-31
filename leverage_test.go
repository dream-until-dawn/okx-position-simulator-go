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

// leverFixture 是 testdata/conformance/leverage-position-limit.json 的结构。
type leverFixture struct {
	Instrument json.RawMessage `json:"instrument"`
	Tiers      json.RawMessage `json:"tiers"`
	ByLever    []struct {
		Lever  string `json:"lever"`
		MaxPos string `json:"maxPos"`
	} `json:"byLever"`
}

func leverSnapshot(t *testing.T) (leverFixture, *refdata.Snapshot, refdata.Instrument) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "leverage-position-limit.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx leverFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatalf("解析合约规格失败: %v", err)
	}
	var tiers []refdata.PositionTier
	if err := json.Unmarshal(fx.Tiers, &tiers); err != nil {
		t.Fatalf("解析档位表失败: %v", err)
	}
	sb := refdata.NewSnapshotBuilder(1).AddInstruments(inst).
		SetFeeSchedule(refdata.DefaultFeeSchedule())
	for _, mode := range []types.MgnMode{types.MgnIsolated, types.MgnCross} {
		tbl, err := refdata.NewTierTable(refdata.TierKey{
			InstType: types.InstSwap, MgnMode: mode, Family: inst.InstFamily}, tiers)
		if err != nil {
			t.Fatalf("构造档位表失败: %v", err)
		}
		sb.AddTierTable(tbl)
	}
	return fx, sb.Build(), inst
}

func leverSim(t *testing.T, snap *refdata.Snapshot) *Simulator {
	t.Helper()
	s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit("USDT", dec("1000000")); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMaxSizeAtLever 锁定「选定杠杆也就选定了持仓量的天花板」。
//
// 档位表的杠杆上限逐档递减，因此能用该杠杆的最高那一档，其 maxSz 即为上限。
// 实测 MASK-USDT-SWAP：50x -> 1000、40x -> 2500、25x -> 66000，三个都与 OKX 的
// max-size 接口逐位相同。
func TestMaxSizeAtLever(t *testing.T) {
	fx, snap, inst := leverSnapshot(t)
	tbl, err := refdata.TierTableFor(snap, inst, types.MgnIsolated)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.ByLever {
		eq(t, tbl.MaxSizeAt(dec(c.Lever)), c.MaxPos, c.Lever+" 倍杠杆下的最大持仓量")
	}
	// 高于第一档上限的杠杆本就设不上，此处返回零而不是最后一档
	eq(t, tbl.MaxSizeAt(dec("51")), "0", "超过最高杠杆时没有可用档位")
}

// TestTierCrossingAddIsRejected 锁定这个最容易想当然的极端情形。
//
// 一档顶格杠杆开满之后再加一张，仓位会落进二档——而二档的杠杆上限更低，容不下
// 当前的杠杆。实测 OKX **直接拒单**（51004），而不是放行后让仓位降档。
//
// 不校验的后果不是数字差一点：模拟器会走到一个真实账户上不可能存在的状态，
// 按二档的维持保证金率算出一套看着正常的风险指标，而实盘根本下不出这一单。
func TestTierCrossingAddIsRejected(t *testing.T) {
	_, snap, inst := leverSnapshot(t)
	s := leverSim(t, snap)

	// 一档 [0,1000] 杠杆≤50；二档 [1001,2500] 杠杆≤40
	if err := s.SetLeverage(inst.InstID, types.MgnIsolated, types.PosLong,
		dec("50")); err != nil {
		t.Fatal(err)
	}
	open := func(sz string) error {
		_, err := s.Fill(Fill{
			InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Buy,
			PosSide: types.PosLong, Sz: dec(sz), Px: dec("0.4"),
			ExecType: types.Taker, Ts: 1,
		})
		return err
	}

	if err := open("1000"); err != nil {
		t.Fatalf("开满一档 1000 张应当成功: %v", err)
	}
	p, _ := s.PositionOf(inst.InstID, types.PosLong)
	eq(t, p.Pos, "1000", "顶格持仓")

	err := open("1")
	if !okxerr.HasCode(err, okxerr.CodeExceedsMaxPosAtLever) {
		t.Fatalf("跨档加仓的错误 = %v，期望 51004", err)
	}
	p, _ = s.PositionOf(inst.InstID, types.PosLong)
	eq(t, p.Pos, "1000", "被拒之后持仓不应变化")

	// 降到 40 倍杠杆后，二档就容得下了
	if err := s.SetLeverage(inst.InstID, types.MgnIsolated, types.PosLong,
		dec("40")); err != nil {
		t.Fatalf("降杠杆到 40 应当成功: %v", err)
	}
	if err := open("1"); err != nil {
		t.Fatalf("40 倍杠杆下加到 1001 张应当成功: %v", err)
	}
	m, err := s.MetricsOf(inst.InstID, types.PosLong)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, m.MMRRate, "0.0075", "跨进二档后的维持保证金率")
}

// TestPosLimitCountsPendingOrders 判据里包含同方向的开仓挂单。
//
// 这一条由 OKX 的报文写明：「the sum of current order size, position quantity in the
// same direction, and pending orders in the same direction」。实测持仓 600 张、
// 同方向挂单 300 张时，再挂 200 张（合计 1100 > 1000）被拒，改挂 100 张通过。
func TestPosLimitCountsPendingOrders(t *testing.T) {
	_, snap, inst := leverSnapshot(t)
	s := leverSim(t, snap)
	if err := s.SetLeverage(inst.InstID, types.MgnIsolated, types.PosLong,
		dec("50")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fill(Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosLong, Sz: dec("600"), Px: dec("0.4"),
		ExecType: types.Taker, Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	place := func(id, sz string) error {
		_, err := s.PlaceOrder(Order{
			OrdID: id, InstID: inst.InstID, TdMode: types.TdIsolated,
			Side: types.Buy, PosSide: types.PosLong, OrdType: types.OrdLimit,
			Px: dec("0.2"), Sz: dec(sz), Ts: 2,
		})
		return err
	}
	if err := place("p1", "300"); err != nil {
		t.Fatalf("挂 300 张应当成功（合计 900）: %v", err)
	}
	if err := place("p2", "200"); !okxerr.HasCode(err, okxerr.CodeExceedsMaxPosAtLever) {
		t.Fatalf("合计 1100 应当被拒，实际 %v", err)
	}
	if err := place("p3", "100"); err != nil {
		t.Fatalf("合计恰好 1000 应当成功: %v", err)
	}

	// 平仓方向的挂单不占额度——它让持仓变小
	if err := func() error {
		_, err := s.PlaceOrder(Order{
			OrdID: "close", InstID: inst.InstID, TdMode: types.TdIsolated,
			Side: types.Sell, PosSide: types.PosLong, OrdType: types.OrdLimit,
			Px: dec("0.9"), Sz: dec("500"), ReduceOnly: true, Ts: 3,
		})
		return err
	}(); err != nil {
		t.Errorf("平仓方向的挂单不该被这条规则挡下: %v", err)
	}
}

// TestSetLeverageRefusedWhenPositionWouldExceed 提高杠杆会压低最大持仓量。
//
// 实测 OKX 返回 59247 并**拒绝改杠杆**，而不是把仓位强制减掉。
func TestSetLeverageRefusedWhenPositionWouldExceed(t *testing.T) {
	_, snap, inst := leverSnapshot(t)
	s := leverSim(t, snap)

	// 40 倍杠杆下开 1500 张（落在二档，二档上限 2500）
	if err := s.SetLeverage(inst.InstID, types.MgnIsolated, types.PosLong,
		dec("40")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fill(Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosLong, Sz: dec("1500"), Px: dec("0.4"),
		ExecType: types.Taker, Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// 提到 50 倍：那一档只容 1000 张，现有 1500 张超了
	err := s.SetLeverage(inst.InstID, types.MgnIsolated, types.PosLong, dec("50"))
	if !okxerr.HasCode(err, okxerr.CodeLeverExceedsPosLimit) {
		t.Fatalf("提杠杆致现有持仓超限的错误 = %v，期望 59247", err)
	}
	eq(t, s.Leverage(inst.InstID, types.MgnIsolated, types.PosLong), "40",
		"被拒之后杠杆不应变化")

	// 超过该品种最高杠杆是另一条规则，另一个码
	if err := s.SetLeverage(inst.InstID, types.MgnIsolated, types.PosLong,
		dec("51")); !okxerr.HasCode(err, okxerr.CodeLeverTooHigh) {
		t.Errorf("超过品种上限的错误 = %v，期望 59102", err)
	}
}

// TestMaxSizeCappedByLeverLimit 可开张数要同时受资金与杠杆两条约束。
//
// 实测 OKX 的 max-size 取两者之小：25 倍杠杆下按资金能开 33 万张，而杠杆那条线
// 只允许 6.6 万张，OKX 返回 6.6 万。只算资金会在高杠杆下高估——而高杠杆恰恰是
// 杠杆这条线最紧的时候。
func TestMaxSizeCappedByLeverLimit(t *testing.T) {
	_, snap, inst := leverSnapshot(t)
	s := leverSim(t, snap)
	for _, side := range []types.PosSide{types.PosLong, types.PosShort} {
		if err := s.SetLeverage(inst.InstID, types.MgnIsolated, side, dec("50")); err != nil {
			t.Fatal(err)
		}
	}

	m, err := s.MaxSize(inst.InstID, types.TdIsolated, dec("0.4"))
	if err != nil {
		t.Fatal(err)
	}
	// 一百万 USDT 按资金能开的远不止 1000 张，此处必须被杠杆那条线卡住
	eq(t, m.MaxBuy, "1000", "50 倍杠杆下的最大可买")
	eq(t, m.MaxSell, "1000", "50 倍杠杆下的最大可卖")

	// 已有持仓要从额度里扣掉
	if _, err := s.Fill(Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosLong, Sz: dec("600"), Px: dec("0.4"),
		ExecType: types.Taker, Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	m, err = s.MaxSize(inst.InstID, types.TdIsolated, dec("0.4"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, m.MaxBuy, "400", "持仓 600 张之后还能买 400 张")
	eq(t, m.MaxSell, "1000", "空头方向不受多头持仓影响")

	// 降杠杆之后额度放宽
	if err := s.SetLeverage(inst.InstID, types.MgnIsolated, types.PosLong,
		dec("25")); err != nil {
		t.Fatal(err)
	}
	m, err = s.MaxSize(inst.InstID, types.TdIsolated, dec("0.4"))
	if err != nil {
		t.Fatal(err)
	}
	if !m.MaxBuy.GreaterThan(dec("10000")) {
		t.Errorf("25 倍杠杆下的额度应当宽得多，实为 %s", m.MaxBuy)
	}
}

// TestPosLimitDoesNotBlockReducing 减仓与平仓不受这条规则影响。
//
// 规则卡的是「持仓变大」。已经超限的仓位（比如降杠杆之前建立的）必须还能平掉，
// 否则使用者会被锁死在一个出不来的仓位里。
func TestPosLimitDoesNotBlockReducing(t *testing.T) {
	_, snap, inst := leverSnapshot(t)
	s := leverSim(t, snap)
	if err := s.SetLeverage(inst.InstID, types.MgnIsolated, types.PosLong,
		dec("50")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fill(Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosLong, Sz: dec("1000"), Px: dec("0.4"),
		ExecType: types.Taker, Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, sz := range []string{"400", "600"} {
		if _, err := s.Fill(Fill{
			InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Sell,
			PosSide: types.PosLong, Sz: dec(sz), Px: dec("0.4"),
			ExecType: types.Taker, Ts: 2,
		}); err != nil {
			t.Fatalf("平掉 %s 张不该被挡: %v", sz, err)
		}
	}
	if _, ok := s.PositionOf(inst.InstID, types.PosLong); ok {
		t.Error("应当已全平")
	}
}

var _ = decimal.Zero

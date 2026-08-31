package okxsim

import (
	"encoding/json"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// richSim 造一个尽量复杂的状态：多个仓位（含累计手续费与资金费）、两类挂单、
// 一张已经棘轮过的移动止损、一张 OCO、三条行情、以及没有持仓的杠杆设置。
//
// 存档最容易漏的就是这些边角：漏了不报错，只会让续跑时凭空少几笔在途委托。
func richSim(t *testing.T) *Simulator {
	t.Helper()
	s, err := New(Config{
		PosMode: types.LongShortMode, RefData: refdata.MustEmbedded(),
		DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit("USDT", dec("200000")); err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit("BTC", dec("2")); err != nil {
		t.Fatal(err)
	}

	// 先设一条【没有持仓】的杠杆——漏掉它，恢复后的第一笔成交就会用错杠杆
	if err := s.SetLeverage("ETH-USDT-SWAP", types.MgnIsolated, types.PosShort,
		dec("7")); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		inst, sz, px, lever string
		mgn                 types.MgnMode
		side                types.PosSide
	}{
		{"BTC-USDT-SWAP", "4", "78000", "5", types.MgnIsolated, types.PosLong},
		{"ETH-USDT-SWAP", "10", "2400", "10", types.MgnCross, types.PosLong},
		{"BTC-USD-SWAP", "40", "78000", "20", types.MgnIsolated, types.PosLong},
	} {
		if err := s.SetLeverage(c.inst, c.mgn, c.side, dec(c.lever)); err != nil {
			t.Fatal(err)
		}
		td := types.TdIsolated
		if c.mgn == types.MgnCross {
			td = types.TdCross
		}
		if _, err := s.Fill(Fill{
			InstID: c.inst, TdMode: td, Side: types.Buy, PosSide: c.side,
			Sz: dec(c.sz), Px: dec(c.px), ExecType: types.Taker, Ts: 1,
		}); err != nil {
			t.Fatalf("%s 建仓失败: %v", c.inst, err)
		}
	}

	// 三条行情各推一条，且互不相同
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("77500")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastPx("BTC-USDT-SWAP", dec("77600")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIndexPx("BTC-USDT-SWAP", dec("77550")); err != nil {
		t.Fatal(err)
	}

	// 累计资金费——它记在仓位上，是最容易被手写搬运代码漏掉的一项。
	// 排在推完行情之后：结算要用标记价。
	if _, err := s.SettleFunding("BTC-USDT-SWAP", Funding{Rate: dec("0.0001")}, 2); err != nil {
		t.Fatal(err)
	}

	// 两笔挂单：一笔开仓方向（有冻结）、一笔平仓方向（无冻结）
	if _, err := s.PlaceOrder(Order{
		OrdID: "open-1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosLong, OrdType: types.OrdLimit,
		Px: dec("70000"), Sz: dec("2"), Ts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceOrder(Order{
		OrdID: "close-1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosLong, OrdType: types.OrdLimit,
		Px: dec("90000"), Sz: dec("1"), ReduceOnly: true, Ts: 4,
	}); err != nil {
		t.Fatal(err)
	}

	// 一张移动止损，并让它先棘轮一次——极值与当前触发价都必须存下来
	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "trail", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosLong, OrdType: types.AlgoMoveStop,
		Sz: dec("2"), ReduceOnly: true, CallbackRatio: dec("0.05"), Ts: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: dec("82000"),
		High: dec("82000"), Low: dec("81000"), Ts: 6,
	}); err != nil {
		t.Fatal(err)
	}

	// 一张 OCO：两条腿方向相反，恢复时若重新推断方向就会反过来
	if _, err := s.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "oco", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosLong, OrdType: types.AlgoOCO,
		Sz: dec("1"), ReduceOnly: true,
		TpTriggerPx: dec("95000"), SlTriggerPx: dec("70000"), Ts: 7,
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func restoredCopy(t *testing.T, src *Simulator) *Simulator {
	t.Helper()
	blob, err := json.Marshal(src.State())
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var st State
	if err := json.Unmarshal(blob, &st); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	dst, err := New(Config{
		PosMode: types.LongShortMode, RefData: refdata.MustEmbedded(),
		DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.Restore(st); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	return dst
}

// TestStateRoundTrip 存档经 JSON 往返之后必须逐字节相同。
func TestStateRoundTrip(t *testing.T) {
	src := richSim(t)
	dst := restoredCopy(t, src)

	a, _ := json.Marshal(src.State())
	b, _ := json.Marshal(dst.State())
	if string(a) != string(b) {
		t.Errorf("往返后状态不同：\n原  %s\n新  %s", a, b)
	}

	// 抽查几项最容易漏的
	sp, _ := src.PositionOf("BTC-USDT-SWAP", types.PosLong)
	dp, _ := dst.PositionOf("BTC-USDT-SWAP", types.PosLong)
	eq(t, dp.Fee, sp.Fee.String(), "累计手续费")
	eq(t, dp.Funding, sp.Funding.String(), "累计资金费")
	eq(t, dp.RealizedPnl, sp.RealizedPnl.String(), "累计已实现盈亏")

	if n := len(dst.PendingOrders("")); n != 2 {
		t.Errorf("挂单数 = %d，期望 2", n)
	}
	if n := len(dst.PendingAlgos("")); n != 2 {
		t.Errorf("算法委托数 = %d，期望 2", n)
	}
	eq(t, dst.Leverage("ETH-USDT-SWAP", types.MgnIsolated, types.PosShort), "7",
		"没有持仓的那条杠杆设置")
	eq(t, dst.IndexPx("BTC-USDT-SWAP"), "77550", "指数价")
	eq(t, dst.LastPx("BTC-USDT-SWAP"), "82000", "最新价")
}

// TestRestoredSimulatorBehavesIdentically 恢复出来的模拟器与原来的【不可区分】。
//
// 这是存档真正要保证的性质，比逐字段比对强得多：把两个模拟器喂同样的行情，
// 一路推下去，每一步的成交、触发、强平、余额都必须一样。任何我漏掉的未导出状态
// 都会在这里露出来——比如移动止损的极值若没存，两边的止损会在不同的价位触发。
func TestRestoredSimulatorBehavesIdentically(t *testing.T) {
	src := richSim(t)
	dst := restoredCopy(t, src)

	bars := []Bar{
		{Last: dec("83000"), High: dec("83500"), Low: dec("82000")},
		{Last: dec("79000"), High: dec("83000"), Low: dec("78000")},
		{Last: dec("71000"), High: dec("79000"), Low: dec("69500")},
		{Last: dec("96000"), High: dec("96500"), Low: dec("71000")},
		{Last: dec("60000"), High: dec("96000"), Low: dec("58000")},
	}
	for i, b := range bars {
		b.InstID = "BTC-USDT-SWAP"
		b.Ts = int64(100 + i)
		b.Funding = &Funding{Rate: dec("0.0001")}

		ra, ea := src.Advance(b)
		rb, eb := dst.Advance(b)
		if (ea == nil) != (eb == nil) {
			t.Fatalf("第 %d 步一边报错一边没有：%v / %v", i, ea, eb)
		}
		ja, _ := json.Marshal(ra)
		jb, _ := json.Marshal(rb)
		if string(ja) != string(jb) {
			t.Fatalf("第 %d 步的结果不同：\n原  %s\n新  %s", i, ja, jb)
		}
		sa, _ := json.Marshal(src.State())
		sb, _ := json.Marshal(dst.State())
		if string(sa) != string(sb) {
			t.Fatalf("第 %d 步之后状态分岔：\n原  %s\n新  %s", i, sa, sb)
		}
	}

	// 这段行情要真的发生过点什么，否则这条测试等于没测
	if len(src.Positions()) == len(richSim(t).Positions()) &&
		src.CashBal("USDT").Equal(dec("200000")) {
		t.Error("这段行情什么都没触发，测试无效")
	}
}

// TestRestoreRefusesMismatchedConfig 配置不匹配时宁可报错，不将就着跑。
func TestRestoreRefusesMismatchedConfig(t *testing.T) {
	src := richSim(t)
	st := src.State()

	t.Run("持仓方式不同", func(t *testing.T) {
		dst, err := New(Config{PosMode: types.NetMode, RefData: refdata.MustEmbedded()})
		if err != nil {
			t.Fatal(err)
		}
		if err := dst.Restore(st); err == nil {
			t.Error("买卖模式与开平仓模式对 posSide 的要求不同，应当拒绝")
		}
	})

	t.Run("规则数据版本不同", func(t *testing.T) {
		dst, err := New(Config{PosMode: types.LongShortMode, RefData: refdata.MustEmbedded()})
		if err != nil {
			t.Fatal(err)
		}
		bad := st
		bad.RefDataVersion = st.RefDataVersion + 1
		if err := dst.Restore(bad); err == nil {
			t.Error("档位表可能已变，同一仓位的维持保证金率就不一样，应当拒绝")
		}
		// 显式清零表示「我知道规则可能变了」
		bad.RefDataVersion = 0
		if err := dst.Restore(bad); err != nil {
			t.Errorf("清零后应当放行: %v", err)
		}
	})

	t.Run("合约不存在", func(t *testing.T) {
		dst, err := New(Config{PosMode: types.LongShortMode, RefData: refdata.MustEmbedded()})
		if err != nil {
			t.Fatal(err)
		}
		bad := st
		bad.Positions = append([]Position{}, st.Positions...)
		bad.Positions[0].InstID = "NOPE-USDT-SWAP"
		if err := dst.Restore(bad); err == nil {
			t.Error("存档里有快照中不存在的合约，应当拒绝")
		}
	})
}

// TestStateIsACopy 导出的状态不应随后续操作而变。
func TestStateIsACopy(t *testing.T) {
	s := richSim(t)
	before, _ := json.Marshal(s.State())
	st := s.State()

	if err := s.Deposit("USDT", dec("50000")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("1000")); err != nil {
		t.Fatal(err)
	}

	after, _ := json.Marshal(st)
	if string(before) != string(after) {
		t.Error("导出的状态被后续操作改动了——State 应当返回副本")
	}
}

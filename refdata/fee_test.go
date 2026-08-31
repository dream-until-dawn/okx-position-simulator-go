package refdata

import (
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func loadSwapFee(t *testing.T) TradeFee {
	t.Helper()
	fees, err := DecodeResponse[TradeFee](readTestdata(t, "trade-fee-SWAP.json"))
	if err != nil {
		t.Fatalf("解析费率失败: %v", err)
	}
	if len(fees) != 1 {
		t.Fatalf("期望 1 条费率，实际 %d 条", len(fees))
	}
	return fees[0]
}

// TestFeeRateSignConvention 费率沿用 OKX 的符号约定：负数表示收取。
// 保留符号是为了让手续费能直接参与余额加法，无需在调用处判断正负。
func TestFeeRateSignConvention(t *testing.T) {
	f := loadSwapFee(t)

	if f.InstType != types.InstSwap {
		t.Errorf("instType = %q", f.InstType)
	}
	if f.Level != types.Lv1 {
		t.Errorf("level = %q，期望 Lv1", f.Level)
	}
	eq(t, f.Base.Maker, "-0.0002", "maker")
	eq(t, f.Base.Taker, "-0.0005", "taker")
	eq(t, f.U.Maker, "-0.0002", "makerU")
	eq(t, f.U.Taker, "-0.0005", "takerU")

	if !f.Base.Taker.IsNegative() {
		t.Error("taker 费率应为负数（表示收取）")
	}
}

// TestFeeRateMatchesRealFill 用一笔真实成交反算费率，验证符号与数值。
//
// 样本取自 testdata/conformance：平仓名义价值 11974.9046800000000013792 USDT，
// OKX 收取 fee = -5.98745234。
func TestFeeRateMatchesRealFill(t *testing.T) {
	f := loadSwapFee(t)

	notional := dec(t, "11974.9046800000000013792")
	realFee := dec(t, "-5.98745234")

	got := notional.Mul(f.Base.Taker)
	if got.Sub(realFee).Abs().GreaterThan(dec(t, "1e-7")) {
		t.Errorf("按 taker 费率算出 %s，真实成交为 %s", got, realFee)
	}
	if !got.IsNegative() {
		t.Error("算出的手续费应为负数，才能直接与余额相加")
	}
}

func TestFeeRateOfExecType(t *testing.T) {
	f := loadSwapFee(t)

	if !f.Base.Of(types.Maker).Equal(f.Base.Maker) {
		t.Error("Of(Maker) 未返回挂单费率")
	}
	if !f.Base.Of(types.Taker).Equal(f.Base.Taker) {
		t.Error("Of(Taker) 未返回吃单费率")
	}
}

// TestFeeGroupsPreserved 费率组不参与 Rate 的解析，但必须完整保留，
// 因为现货各组费率差异显著，说明该维度确实生效。
func TestFeeGroupsPreserved(t *testing.T) {
	f := loadSwapFee(t)

	if len(f.Groups) == 0 {
		t.Fatal("费率组未被保留")
	}
	// 实测全部永续合约的 groupId 均为 4
	r, ok := f.Group("4")
	if !ok {
		t.Fatal("找不到 groupId=4 的费率组")
	}
	eq(t, r.Taker, "-0.0005", "group4.taker")

	if _, ok := f.Group("不存在的组"); ok {
		t.Error("不存在的费率组不应返回 ok")
	}
}

// TestSwapAllInstrumentsShareOneGroup 守卫 Rate 不解析 groupId 这一决定所依赖的前提。
//
// 若将来永续合约出现多个费率组、或各组费率不再一致，该前提失效，
// Rate 必须补上费率组维度的解析。
func TestSwapAllInstrumentsShareOneGroup(t *testing.T) {
	f := loadSwapFee(t)

	base := f.Groups[0].FeeRate
	for _, g := range f.Groups {
		if !g.Maker.Equal(base.Maker) || !g.Taker.Equal(base.Taker) {
			t.Errorf("永续费率组 %s 与组 %s 费率不同（maker %s vs %s，taker %s vs %s），"+
				"Rate 需要补上费率组维度的解析",
				g.GroupID, f.Groups[0].GroupID, g.Maker, base.Maker, g.Taker, base.Taker)
		}
	}
	// 各组费率还应与 maker/taker、makerU/takerU 一致，两条路径才能给出同一个数字
	if !base.Taker.Equal(f.Base.Taker) || !base.Taker.Equal(f.U.Taker) {
		t.Errorf("费率组 taker(%s) 与 taker(%s)、takerU(%s) 不一致",
			base.Taker, f.Base.Taker, f.U.Taker)
	}
}

func TestFeeScheduleRateBySettleCcy(t *testing.T) {
	s := NewFeeSchedule(loadSwapFee(t))

	linear := loadInstrument(t, "instruments-BTC-USDT-SWAP.json") // settleCcy = USDT
	r, err := s.Rate(linear)
	if err != nil {
		t.Fatalf("查询 USDT 本位费率失败: %v", err)
	}
	eq(t, r.Taker, "-0.0005", "USDT 本位 taker")

	inverse := loadInstrument(t, "instruments-BTC-USD-SWAP.json") // settleCcy = BTC
	r, err = s.Rate(inverse)
	if err != nil {
		t.Fatalf("查询币本位费率失败: %v", err)
	}
	eq(t, r.Taker, "-0.0005", "币本位 taker")
}

func TestFeeScheduleUnknownInstType(t *testing.T) {
	s := NewFeeSchedule(loadSwapFee(t))

	opt := Instrument{InstType: types.InstOption, InstID: "BTC-USD-260327-100000-C"}
	if _, err := s.Rate(opt); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("未知产品类型的错误 = %v，期望 51000", err)
	}
}

// TestFeeScheduleWithRateIsImmutable 覆盖费率必须返回副本，不能改动原表。
// 回测引擎可能并行持有同一份费率表做参数扫描，就地修改会互相污染。
func TestFeeScheduleWithRateIsImmutable(t *testing.T) {
	orig := NewFeeSchedule(loadSwapFee(t))
	inst := loadInstrument(t, "instruments-BTC-USDT-SWAP.json")

	custom := FeeRate{Maker: dec(t, "-0.00005"), Taker: dec(t, "-0.0002")}
	derived := orig.WithRate(types.InstSwap, custom)

	got, err := derived.Rate(inst)
	if err != nil {
		t.Fatalf("查询覆盖后的费率失败: %v", err)
	}
	eq(t, got.Taker, "-0.0002", "覆盖后的 taker")
	eq(t, got.Maker, "-0.00005", "覆盖后的 maker")

	origRate, err := orig.Rate(inst)
	if err != nil {
		t.Fatalf("查询原费率失败: %v", err)
	}
	eq(t, origRate.Taker, "-0.0005", "原表 taker 不应被改动")
}

func TestFeeScheduleWithLevelKeepsRates(t *testing.T) {
	orig := NewFeeSchedule(loadSwapFee(t))
	derived := orig.WithLevel(types.VIP3)

	if derived.Level() != types.VIP3 {
		t.Errorf("等级 = %q，期望 VIP3", derived.Level())
	}
	if orig.Level() != types.Lv1 {
		t.Errorf("原表等级被改动为 %q", orig.Level())
	}

	// 等级到费率的映射未公开，改等级不得凭空改动费率数值
	f, ok := derived.TradeFee(types.InstSwap)
	if !ok {
		t.Fatal("找不到 SWAP 费率")
	}
	eq(t, f.Base.Taker, "-0.0005", "改等级后 taker 不应变化")
}

func TestFeeScheduleEmpty(t *testing.T) {
	var s FeeSchedule
	if s.Level() != "" {
		t.Errorf("空费率表的等级 = %q，期望空", s.Level())
	}
	if _, ok := s.TradeFee(types.InstSwap); ok {
		t.Error("空费率表不应返回费率")
	}
	// 空表上调用 WithRate 应当可用，不得 panic
	got, err := s.WithRate(types.InstSwap, FeeRate{Taker: decimal.NewFromFloat(-0.0005)}).
		Rate(Instrument{InstType: types.InstSwap})
	if err != nil {
		t.Fatalf("在空表上覆盖费率后查询失败: %v", err)
	}
	eq(t, got.Taker, "-0.0005", "空表覆盖后的 taker")
}

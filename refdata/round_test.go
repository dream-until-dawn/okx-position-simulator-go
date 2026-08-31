package refdata

import (
	"errors"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func TestIsMultipleOf(t *testing.T) {
	cases := []struct {
		v, step string
		want    bool
	}{
		{"0.03", "0.01", true},
		{"1000", "0.01", true},
		{"0.015", "0.01", false},
		{"1000.005", "0.01", false},
		{"77657.4", "0.1", true},
		{"77657.45", "0.1", false},
		{"0", "0.01", true},
		{"5", "1", true},
		{"5.5", "1", false},
		// 步长为零时视为不设限制，避免除零
		{"1.2345", "0", true},
	}
	for _, c := range cases {
		got := IsMultipleOf(dec(t, c.v), dec(t, c.step))
		if got != c.want {
			t.Errorf("IsMultipleOf(%s, %s) = %v, 期望 %v", c.v, c.step, got, c.want)
		}
	}
}

func TestFloorAndCeilToStep(t *testing.T) {
	cases := []struct {
		v, step, floor, ceil string
	}{
		{"1000.005", "0.01", "1000", "1000.01"},
		{"0.015", "0.01", "0.01", "0.02"},
		{"0.02", "0.01", "0.02", "0.02"},
		{"77657.45", "0.1", "77657.4", "77657.5"},
		{"0", "0.01", "0", "0"},
		{"0.004", "0.01", "0", "0.01"},
	}
	for _, c := range cases {
		v, step := dec(t, c.v), dec(t, c.step)
		eq(t, FloorToStep(v, step), c.floor, "FloorToStep("+c.v+","+c.step+")")
		eq(t, CeilToStep(v, step), c.ceil, "CeilToStep("+c.v+","+c.step+")")
	}
}

func TestRoundSizeFloorsDown(t *testing.T) {
	i := loadInstrument(t, "instruments-BTC-USDT-SWAP.json") // lotSz = 0.01

	// 向下取整：宁可少下，不可多下
	eq(t, i.RoundSize(dec(t, "0.019")), "0.01", "RoundSize(0.019)")
	eq(t, i.RoundSize(dec(t, "1000.009")), "1000", "RoundSize(1000.009)")
	eq(t, i.RoundSize(dec(t, "3")), "3", "RoundSize(3)")
	eq(t, i.RoundSize(dec(t, "0.009")), "0", "RoundSize(0.009) 不足一个步长")
}

// TestRoundPriceDirection 买单向下、卖单向上取整，两侧都朝对下单方更不利的方向，
// 因此取整本身不会凭空制造出更优的成交价。
func TestRoundPriceDirection(t *testing.T) {
	i := loadInstrument(t, "instruments-BTC-USDT-SWAP.json") // tickSz = 0.1

	px := dec(t, "77657.45")
	eq(t, i.RoundPrice(px, types.Buy), "77657.4", "买单取整")
	eq(t, i.RoundPrice(px, types.Sell), "77657.5", "卖单取整")

	// 已是整数倍时两个方向都不变
	exact := dec(t, "77657.4")
	eq(t, i.RoundPrice(exact, types.Buy), "77657.4", "买单取整(已对齐)")
	eq(t, i.RoundPrice(exact, types.Sell), "77657.4", "卖单取整(已对齐)")
}

func TestValidateSize(t *testing.T) {
	i := loadInstrument(t, "instruments-BTC-USDT-SWAP.json") // lotSz = minSz = 0.01

	if err := i.ValidateSize(dec(t, "0.01")); err != nil {
		t.Errorf("最小合法数量被拒: %v", err)
	}
	if err := i.ValidateSize(dec(t, "12.34")); err != nil {
		t.Errorf("合法数量被拒: %v", err)
	}

	// 非 lotSz 整数倍 —— 实测 OKX 返回 51121
	err := i.ValidateSize(dec(t, "0.015"))
	if !okxerr.HasCode(err, okxerr.CodeNotLotSizeMultiple) {
		t.Errorf("非整数倍数量的错误 = %v，期望 51121", err)
	}

	// 低于最小下单量 —— 永续的 minSz 与 lotSz 相等，OKX 同样返回 51121
	err = i.ValidateSize(dec(t, "0.001"))
	if !okxerr.HasCode(err, okxerr.CodeNotLotSizeMultiple) {
		t.Errorf("低于最小量的错误 = %v，期望 51121", err)
	}

	// 零与负数属参数错误
	for _, bad := range []string{"0", "-1", "-0.01"} {
		err := i.ValidateSize(dec(t, bad))
		if !okxerr.HasCode(err, okxerr.CodeParamError) {
			t.Errorf("ValidateSize(%s) 的错误 = %v，期望 51000", bad, err)
		}
	}
}

// TestSwapMinSzEqualsLotSz 守卫一个被实测证实、且 ValidateSize 的错误码选择所依赖的前提：
// 全部 459 个永续合约的 minSz 与 lotSz 相等。若将来引入两者不等的品种，
// 低于最小下单量情形的真实错误码需要重新向 OKX 探测。
func TestSwapMinSzEqualsLotSz(t *testing.T) {
	for _, name := range []string{
		"instruments-BTC-USDT-SWAP.json",
		"instruments-BTC-USD-SWAP.json",
	} {
		i := loadInstrument(t, name)
		if !i.MinSz.Equal(i.LotSz) {
			t.Errorf("%s: minSz(%s) 与 lotSz(%s) 不等，需重新探测低于最小量的错误码",
				i.InstID, i.MinSz, i.LotSz)
		}
	}
}

func TestValidateOrderSizeMaxLimits(t *testing.T) {
	i := loadInstrument(t, "instruments-BTC-USDT-SWAP.json")

	// 限价与市价的单笔上限不同，取值应随委托类型切换
	if !i.MaxOrderSize(types.OrdLimit).Equal(i.MaxLmtSz) {
		t.Errorf("限价单上限 = %s，期望 maxLmtSz = %s", i.MaxOrderSize(types.OrdLimit), i.MaxLmtSz)
	}
	if !i.MaxOrderSize(types.OrdMarket).Equal(i.MaxMktSz) {
		t.Errorf("市价单上限 = %s，期望 maxMktSz = %s", i.MaxOrderSize(types.OrdMarket), i.MaxMktSz)
	}

	over := i.MaxMktSz.Add(decimal.NewFromInt(1))
	err := i.ValidateOrderSize(over, types.OrdMarket)
	if !okxerr.HasCode(err, okxerr.CodeExceedsMaxOrderAmt) {
		t.Errorf("超市价单上限的错误 = %v，期望 51005", err)
	}

	// 同样的数量对限价单是合法的（maxLmtSz 远大于 maxMktSz）
	if err := i.ValidateOrderSize(over, types.OrdLimit); err != nil {
		t.Errorf("限价单上限内的数量被拒: %v", err)
	}
}

// TestValidatePriceTickIsOptIn tickSz 的真实强制性尚未定论，
// 因此该校验必须是显式调用的，不能被 ValidateSize 隐式触发。
func TestValidatePriceTickIsOptIn(t *testing.T) {
	i := loadInstrument(t, "instruments-BTC-USDT-SWAP.json") // tickSz = 0.1

	// 显式调用时应当报出非整数倍
	err := i.ValidatePriceTick(dec(t, "10000.05"))
	if !errors.Is(err, ErrPriceNotTickMultiple) {
		t.Errorf("ValidatePriceTick(10000.05) = %v，期望 ErrPriceNotTickMultiple", err)
	}
	if err := i.ValidatePriceTick(dec(t, "10000.1")); err != nil {
		t.Errorf("对齐的价格被拒: %v", err)
	}

	// 数量校验不应牵连价格
	if err := i.ValidateSize(dec(t, "0.01")); err != nil {
		t.Errorf("ValidateSize 不应涉及价格，却报错: %v", err)
	}
}

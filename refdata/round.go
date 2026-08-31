package refdata

import (
	"errors"
	"fmt"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// ErrPriceNotTickMultiple 表示价格不是 tickSz 的整数倍。
//
// 它只用于 ValidatePriceTick 这个诊断用途，模拟器主路径不会产生该错误——
// 实测确认 OKX 从不拒绝超精度价格，而是按方向取整后接受（详见 RoundPrice）。
// 因此它没有对应的 OKX 错误码，也不该被当成「OKX 会这样报错」来使用。
var ErrPriceNotTickMultiple = errors.New("价格不是 tickSz 的整数倍")

// IsMultipleOf 报告 v 是否为 step 的整数倍；step 为零时恒为真。
//
// 用取余而非除法判断，避免 decimal 除法精度设置带来的误判。
func IsMultipleOf(v, step decimal.Decimal) bool {
	if step.IsZero() {
		return true
	}
	return v.Mod(step).IsZero()
}

// FloorToStep 把非负的 v 向下取整到 step 的整数倍。
func FloorToStep(v, step decimal.Decimal) decimal.Decimal {
	if step.IsZero() || v.IsNegative() {
		return v
	}
	return v.Sub(v.Mod(step))
}

// CeilToStep 把非负的 v 向上取整到 step 的整数倍。
func CeilToStep(v, step decimal.Decimal) decimal.Decimal {
	if step.IsZero() || v.IsNegative() {
		return v
	}
	rem := v.Mod(step)
	if rem.IsZero() {
		return v
	}
	return v.Sub(rem).Add(step)
}

// RoundSize 把下单数量向下取整到 lotSz 的整数倍。
//
// 取整方向为向下：宁可少下，不可多下——向上取整会让实际下单量超出调用方的意图。
func (i Instrument) RoundSize(sz decimal.Decimal) decimal.Decimal {
	return FloorToStep(sz, i.LotSz)
}

// RoundPrice 按买卖方向把价格取整到 tickSz 的整数倍：买单向下、卖单向上。
//
// 该行为经模拟盘实测确认与 OKX 一致。以 tickSz 为 0.1 的合约为例：
//
//	买单  2150.51 / 2150.55 / 2150.59  ->  2150.5   向下截断
//	卖单  2376.91 / 2376.95 / 2376.99  ->  2377     向上进位
//
// 两个方向都是朝远离市价的一侧移动，即让委托变得更不激进，
// 不会凭空提高成交概率或制造出更优的成交价。
//
// 这是模拟器处理价格精度的正道：OKX 从不因超精度而拒单，只做取整。
// 若改为强制校验并拒单，模拟器会拒掉真实 OKX 会接受的订单，
// 回测中凭空少掉成交——那与还原真实行为的目标正好相反。
func (i Instrument) RoundPrice(px decimal.Decimal, side types.Side) decimal.Decimal {
	if side == types.Buy {
		return FloorToStep(px, i.TickSz)
	}
	return CeilToStep(px, i.TickSz)
}

// ValidateSize 校验下单数量是否符合该合约的数量精度与最小下单量。
//
// 实测 459 个永续合约的 minSz 与 lotSz 全部相等，因此对永续而言这两项校验
// 等价，OKX 也统一返回 51121。若将来扩展到 minSz 与 lotSz 不等的品种
// （交割、现货），低于最小下单量情形的真实错误码需要重新探测。
func (i Instrument) ValidateSize(sz decimal.Decimal) error {
	if !sz.IsPositive() {
		return okxerr.New(okxerr.CodeParamError, "sz: 下单数量须为正数，实为 %s", sz)
	}
	if !IsMultipleOf(sz, i.LotSz) {
		return okxerr.New(okxerr.CodeNotLotSizeMultiple,
			"%s: 下单数量 %s 不是数量精度 %s 的整数倍", i.InstID, sz, i.LotSz)
	}
	if sz.LessThan(i.MinSz) {
		return okxerr.New(okxerr.CodeNotLotSizeMultiple,
			"%s: 下单数量 %s 小于最小下单量 %s", i.InstID, sz, i.MinSz)
	}
	return nil
}

// MaxOrderSize 返回该委托类型允许的单笔最大委托数量；未设上限时返回零值。
func (i Instrument) MaxOrderSize(ordType types.OrdType) decimal.Decimal {
	switch ordType {
	case types.OrdMarket, types.OrdOptimalLimitIOC:
		return i.MaxMktSz
	default:
		return i.MaxLmtSz
	}
}

// ValidateOrderSize 在 ValidateSize 之上追加该委托类型的单笔上限校验。
func (i Instrument) ValidateOrderSize(sz decimal.Decimal, ordType types.OrdType) error {
	if err := i.ValidateSize(sz); err != nil {
		return err
	}
	if max := i.MaxOrderSize(ordType); max.IsPositive() && sz.GreaterThan(max) {
		return okxerr.New(okxerr.CodeExceedsMaxOrderAmt,
			"%s: 下单数量 %s 超出 %s 委托的单笔上限 %s", i.InstID, sz, ordType, max)
	}
	return nil
}

// ValidatePriceTick 校验价格是否为 tickSz 的整数倍。
//
// 这是一个**诊断工具**，不是模拟真实行为的一环：OKX 不会因价格超精度而拒单，
// 因此模拟器主路径走的是 RoundPrice 而非本方法。
//
// 它的用途是让使用者主动自查——策略若在下超精度的价格，说明其价格计算与合约
// 精度不匹配，虽然 OKX 会替它取整，但策略自己算出的预期成交价会与实际有偏差。
// 需要时显式调用。
func (i Instrument) ValidatePriceTick(px decimal.Decimal) error {
	if !px.IsPositive() {
		return okxerr.New(okxerr.CodeParamError, "px: 价格须为正数，实为 %s", px)
	}
	if !IsMultipleOf(px, i.TickSz) {
		return fmt.Errorf("%s: 价格 %s 不是价格精度 %s 的整数倍: %w",
			i.InstID, px, i.TickSz, ErrPriceNotTickMultiple)
	}
	return nil
}

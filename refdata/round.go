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
// 该规则的真实行为尚未定论：对模拟盘的探测中，一笔远离市价、价格为 10000.05
// 而 tickSz 为 0.1 的限价买单被 OKX 接受，且 px 被原样存储而非取整；近市价的
// 复验因账户可用余额不足而被提前拦截，未能取得结论。
//
// 因此本包不默认强制该校验——ValidateSize 不会检查价格，需要严格模式的调用方
// 显式调用 ValidatePriceTick。待模拟盘账户有充足资金后于 v0.2.0 对拍时定论，
// 届时若确认 OKX 会拒绝，再为其补上真实错误码。
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
// 两个方向都朝着「对下单方更不利」的一侧取整，因而不会凭空制造出更优的成交价。
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
// 该校验不被 ValidateSize 或 ValidateOrderSize 调用，需要严格模式的调用方
// 显式使用。原因见 ErrPriceNotTickMultiple 的说明。
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

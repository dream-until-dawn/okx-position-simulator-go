package refdata

import (
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// DefaultFeeSchedule 返回 Lv1 等级的费率表。
//
// 费率与其他规则数据性质不同：它是账户相关的。取得真实费率要调用
// /api/v5/account/trade-fee，该接口需要鉴权且返回的是「当前账户」的费率，
// 无法作为公共规则拉取；等级到费率的映射表也未公开于任何免鉴权接口。
//
// 因此这里给出的是新账户的默认等级 Lv1，取值经模拟盘实测确认：
//
//	SWAP     maker -0.0002  taker -0.0005
//	FUTURES  maker -0.0002  taker -0.0005  交割 0.0001
//
// 沿用 OKX 的符号约定：负数表示收取。
//
// 实际费率若与此不同——VIP 等级、协议费率、活动折扣、OKB 抵扣后的净费率——
// 请用 FeeSchedule.WithRate 覆盖，或从 /account/trade-fee 拉取后自行装配。
// 这是刻意的设计：与其凭等级去推算一个无从验证的数字，不如让使用者给出真值。
func DefaultFeeSchedule() FeeSchedule {
	lv1 := FeeRate{
		Maker: decimal.RequireFromString("-0.0002"),
		Taker: decimal.RequireFromString("-0.0005"),
	}
	return NewFeeSchedule(
		TradeFee{
			InstType: types.InstSwap,
			Level:    types.Lv1,
			Base:     lv1,
			U:        lv1,
			USDC:     lv1,
		},
		TradeFee{
			InstType: types.InstFutures,
			Level:    types.Lv1,
			Base:     lv1,
			U:        lv1,
			USDC:     lv1,
			Delivery: decimal.RequireFromString("0.0001"),
		},
	)
}

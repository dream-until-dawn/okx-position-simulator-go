package types

// InstType 产品类型，取值与 OKX API v5 的 instType 字段完全一致。
type InstType string

const (
	InstSpot    InstType = "SPOT"
	InstMargin  InstType = "MARGIN"
	InstSwap    InstType = "SWAP"
	InstFutures InstType = "FUTURES"
	InstOption  InstType = "OPTION"
)

func (t InstType) String() string { return string(t) }

// Valid 报告 t 是否为 OKX 定义的合法产品类型。
func (t InstType) Valid() bool {
	switch t {
	case InstSpot, InstMargin, InstSwap, InstFutures, InstOption:
		return true
	}
	return false
}

// IsDerivative 报告 t 是否为衍生品（有仓位、保证金、档位的品种）。
func (t InstType) IsDerivative() bool {
	switch t {
	case InstSwap, InstFutures, InstOption:
		return true
	}
	return false
}

// CtType 合约类型，取值与 OKX 的 ctType 字段一致。
//
// linear  正向合约（USDT/USDC 本位）：面值以标的币计，保证金与盈亏以计价币结算。
// inverse 反向合约（币本位）：面值以计价币计，保证金与盈亏以标的币结算。
type CtType string

const (
	Linear  CtType = "linear"
	Inverse CtType = "inverse"
)

func (t CtType) String() string { return string(t) }

func (t CtType) Valid() bool { return t == Linear || t == Inverse }

// InstState 产品状态，取值与 OKX 的 state 字段一致。
type InstState string

const (
	StateLive    InstState = "live"
	StateSuspend InstState = "suspend"
	StatePreOpen InstState = "preopen"
	StateTest    InstState = "test"
	StateExpired InstState = "expired"
)

func (s InstState) String() string { return string(s) }

// Tradable 报告该状态下产品是否可交易。
func (s InstState) Tradable() bool { return s == StateLive }

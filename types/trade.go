package types

// TdMode 交易模式，下单时指定，取值与 OKX 的 tdMode 字段一致。
type TdMode string

const (
	TdIsolated     TdMode = "isolated"      // 逐仓
	TdCross        TdMode = "cross"         // 全仓
	TdCash         TdMode = "cash"          // 非保证金（现货）
	TdSpotIsolated TdMode = "spot_isolated" // 现货逐仓（仅带单场景）
)

func (m TdMode) String() string { return string(m) }

func (m TdMode) Valid() bool {
	switch m {
	case TdIsolated, TdCross, TdCash, TdSpotIsolated:
		return true
	}
	return false
}

// MgnMode 返回该交易模式对应的保证金模式；非保证金模式返回空值与 false。
func (m TdMode) MgnMode() (MgnMode, bool) {
	switch m {
	case TdIsolated:
		return MgnIsolated, true
	case TdCross:
		return MgnCross, true
	}
	return "", false
}

// MgnMode 保证金模式，仓位上的属性，取值与 OKX 的 mgnMode 字段一致。
//
// 与 TdMode 的区别：TdMode 是下单参数（含 cash/spot_isolated），
// MgnMode 是仓位属性（只有 isolated/cross）。二者取值有重叠但语义不同，故分开定义。
type MgnMode string

const (
	MgnIsolated MgnMode = "isolated"
	MgnCross    MgnMode = "cross"
)

func (m MgnMode) String() string { return string(m) }

func (m MgnMode) Valid() bool { return m == MgnIsolated || m == MgnCross }

// TdMode 返回该保证金模式对应的交易模式。
func (m MgnMode) TdMode() TdMode {
	if m == MgnIsolated {
		return TdIsolated
	}
	return TdCross
}

// PosSide 持仓方向，取值与 OKX 的 posSide 字段一致。
//
// 买卖模式(net_mode)下只有 net；开平仓模式(long_short_mode)下为 long/short。
type PosSide string

const (
	PosLong  PosSide = "long"
	PosShort PosSide = "short"
	PosNet   PosSide = "net"
)

func (s PosSide) String() string { return string(s) }

func (s PosSide) Valid() bool {
	switch s {
	case PosLong, PosShort, PosNet:
		return true
	}
	return false
}

// Opposite 返回相反方向；net 无相反方向，原样返回。
func (s PosSide) Opposite() PosSide {
	switch s {
	case PosLong:
		return PosShort
	case PosShort:
		return PosLong
	}
	return s
}

// Side 订单方向，取值与 OKX 的 side 字段一致。
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

func (s Side) String() string { return string(s) }

func (s Side) Valid() bool { return s == Buy || s == Sell }

func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// OrdType 订单类型，取值与 OKX 的 ordType 字段一致。
type OrdType string

const (
	OrdMarket          OrdType = "market"
	OrdLimit           OrdType = "limit"
	OrdPostOnly        OrdType = "post_only"
	OrdFOK             OrdType = "fok"
	OrdIOC             OrdType = "ioc"
	OrdOptimalLimitIOC OrdType = "optimal_limit_ioc"
)

func (t OrdType) String() string { return string(t) }

func (t OrdType) Valid() bool {
	switch t {
	case OrdMarket, OrdLimit, OrdPostOnly, OrdFOK, OrdIOC, OrdOptimalLimitIOC:
		return true
	}
	return false
}

// ExecType 成交角色（挂单方/吃单方），取值与 OKX 成交明细的 execType 字段一致。
// 它决定按 maker 费率还是 taker 费率计费。
type ExecType string

const (
	Maker ExecType = "M"
	Taker ExecType = "T"
)

func (e ExecType) String() string { return string(e) }

func (e ExecType) Valid() bool { return e == Maker || e == Taker }

package types

// AcctLv 账户模式，取值与 OKX /api/v5/account/config 的 acctLv 字段一致（字符串 "1".."4"）。
type AcctLv string

const (
	AcctSpot            AcctLv = "1" // 现货模式
	AcctSpotAndFutures  AcctLv = "2" // 现货合约模式（原单币种保证金模式）
	AcctMultiCcyMargin  AcctLv = "3" // 跨币种保证金模式
	AcctPortfolioMargin AcctLv = "4" // 组合保证金模式
)

func (a AcctLv) String() string { return string(a) }

func (a AcctLv) Valid() bool {
	switch a {
	case AcctSpot, AcctSpotAndFutures, AcctMultiCcyMargin, AcctPortfolioMargin:
		return true
	}
	return false
}

// SupportsDerivatives 报告该账户模式是否支持衍生品交易。
func (a AcctLv) SupportsDerivatives() bool { return a != AcctSpot }

// PosMode 持仓方式，取值与 OKX 的 posMode 字段一致。
type PosMode string

const (
	NetMode       PosMode = "net_mode"        // 买卖模式：同一合约只能有一个方向的仓位
	LongShortMode PosMode = "long_short_mode" // 开平仓模式：可同时持有多空双向仓位
)

func (m PosMode) String() string { return string(m) }

func (m PosMode) Valid() bool { return m == NetMode || m == LongShortMode }

// PosSides 返回该持仓方式下合法的持仓方向集合。
func (m PosMode) PosSides() []PosSide {
	if m == NetMode {
		return []PosSide{PosNet}
	}
	return []PosSide{PosLong, PosShort}
}

// FeeLevel 手续费等级，取值与 OKX /api/v5/account/config 的 level 字段一致。
//
// 该等级由用户近 30 日成交量与资产量决定，会随时间变动，因此在模拟器中
// 作为可调参数由使用者设置，而不是从规则数据中推导。
type FeeLevel string

const (
	Lv1 FeeLevel = "Lv1"
	Lv2 FeeLevel = "Lv2"
	Lv3 FeeLevel = "Lv3"
	Lv4 FeeLevel = "Lv4"
	Lv5 FeeLevel = "Lv5"

	VIP1 FeeLevel = "VIP1"
	VIP2 FeeLevel = "VIP2"
	VIP3 FeeLevel = "VIP3"
	VIP4 FeeLevel = "VIP4"
	VIP5 FeeLevel = "VIP5"
	VIP6 FeeLevel = "VIP6"
	VIP7 FeeLevel = "VIP7"
	VIP8 FeeLevel = "VIP8"
)

func (l FeeLevel) String() string { return string(l) }

func (l FeeLevel) Valid() bool {
	switch l {
	case Lv1, Lv2, Lv3, Lv4, Lv5,
		VIP1, VIP2, VIP3, VIP4, VIP5, VIP6, VIP7, VIP8:
		return true
	}
	return false
}

package refdata

import (
	"encoding/json"

	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// Instrument 是一个合约的规格，字段与 OKX /api/v5/public/instruments 的响应对应。
//
// 只收录仓位与保证金计算、以及下单校验真正会用到的字段；OKX 响应里的其余字段
// （营销位、冰山单上限等）不予保留，以免模型被无关信息淹没。
type Instrument struct {
	InstType   types.InstType  // 产品类型
	InstID     string          // 产品 ID，如 BTC-USDT-SWAP
	InstFamily string          // 交易品种，如 BTC-USDT —— 档位表以此为聚合键
	Uly        string          // 标的指数，如 BTC-USDT
	BaseCcy    string          // 交易货币（仅币币/币币杠杆有值）
	QuoteCcy   string          // 计价货币（仅币币/币币杠杆有值）
	SettleCcy  string          // 结算与保证金币种，如 BTC / USDT
	CtVal      decimal.Decimal // 合约面值
	CtMult     decimal.Decimal // 合约乘数
	CtValCcy   string          // 合约面值计价币种
	CtType     types.CtType    // linear（正向）/ inverse（反向）
	GroupID    string          // 手续费费率组，对应费率表 feeGroup 里的 groupId
	Lever      decimal.Decimal // 该合约支持的最大杠杆
	TickSz     decimal.Decimal // 下单价格精度
	LotSz      decimal.Decimal // 下单数量精度
	MinSz      decimal.Decimal // 最小下单数量
	MaxLmtSz   decimal.Decimal // 限价单最大委托数量
	MaxMktSz   decimal.Decimal // 市价单最大委托数量
	ListTime   int64           // 上线时间（毫秒）
	ExpTime    int64           // 到期时间（毫秒），永续为 0
	State      types.InstState // 产品状态
}

// rawInstrument 是 OKX 的线格式：所有字段都是字符串。
type rawInstrument struct {
	InstType   string `json:"instType"`
	InstID     string `json:"instId"`
	InstFamily string `json:"instFamily"`
	Uly        string `json:"uly"`
	BaseCcy    string `json:"baseCcy"`
	QuoteCcy   string `json:"quoteCcy"`
	SettleCcy  string `json:"settleCcy"`
	CtVal      string `json:"ctVal"`
	CtMult     string `json:"ctMult"`
	CtValCcy   string `json:"ctValCcy"`
	CtType     string `json:"ctType"`
	GroupID    string `json:"groupId"`
	Lever      string `json:"lever"`
	TickSz     string `json:"tickSz"`
	LotSz      string `json:"lotSz"`
	MinSz      string `json:"minSz"`
	MaxLmtSz   string `json:"maxLmtSz"`
	MaxMktSz   string `json:"maxMktSz"`
	ListTime   string `json:"listTime"`
	ExpTime    string `json:"expTime"`
	State      string `json:"state"`
}

// UnmarshalJSON 按 OKX 的线格式解析。
func (i *Instrument) UnmarshalJSON(b []byte) error {
	var r rawInstrument
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}

	var err error
	i.InstType = types.InstType(r.InstType)
	i.InstID = r.InstID
	i.InstFamily = r.InstFamily
	i.Uly = r.Uly
	i.BaseCcy = r.BaseCcy
	i.QuoteCcy = r.QuoteCcy
	i.SettleCcy = r.SettleCcy
	i.CtValCcy = r.CtValCcy
	i.CtType = types.CtType(r.CtType)
	i.GroupID = r.GroupID
	i.State = types.InstState(r.State)

	if i.CtVal, err = parseDec("ctVal", r.CtVal); err != nil {
		return err
	}
	if i.CtMult, err = parseDec("ctMult", r.CtMult); err != nil {
		return err
	}
	if i.Lever, err = parseDec("lever", r.Lever); err != nil {
		return err
	}
	if i.TickSz, err = parseDec("tickSz", r.TickSz); err != nil {
		return err
	}
	if i.LotSz, err = parseDec("lotSz", r.LotSz); err != nil {
		return err
	}
	if i.MinSz, err = parseDec("minSz", r.MinSz); err != nil {
		return err
	}
	if i.MaxLmtSz, err = parseDec("maxLmtSz", r.MaxLmtSz); err != nil {
		return err
	}
	if i.MaxMktSz, err = parseDec("maxMktSz", r.MaxMktSz); err != nil {
		return err
	}
	if i.ListTime, err = parseMillis("listTime", r.ListTime); err != nil {
		return err
	}
	if i.ExpTime, err = parseMillis("expTime", r.ExpTime); err != nil {
		return err
	}
	return nil
}

// MarshalJSON 按 OKX 的线格式输出，使快照文件可与真实 API 响应直接比对。
func (i Instrument) MarshalJSON() ([]byte, error) {
	return json.Marshal(rawInstrument{
		InstType:   i.InstType.String(),
		InstID:     i.InstID,
		InstFamily: i.InstFamily,
		Uly:        i.Uly,
		BaseCcy:    i.BaseCcy,
		QuoteCcy:   i.QuoteCcy,
		SettleCcy:  i.SettleCcy,
		CtVal:      formatDec(i.CtVal),
		CtMult:     formatDec(i.CtMult),
		CtValCcy:   i.CtValCcy,
		CtType:     i.CtType.String(),
		GroupID:    i.GroupID,
		Lever:      formatDec(i.Lever),
		TickSz:     formatDec(i.TickSz),
		LotSz:      formatDec(i.LotSz),
		MinSz:      formatDec(i.MinSz),
		MaxLmtSz:   formatDec(i.MaxLmtSz),
		MaxMktSz:   formatDec(i.MaxMktSz),
		ListTime:   formatMillis(i.ListTime),
		ExpTime:    formatMillis(i.ExpTime),
		State:      i.State.String(),
	})
}

// IsInverse 报告该合约是否为反向（币本位）合约。
func (i Instrument) IsInverse() bool { return i.CtType == types.Inverse }

// IsPerpetual 报告该合约是否为永续合约（无到期时间）。
func (i Instrument) IsPerpetual() bool { return i.InstType == types.InstSwap }

// ContractQty 返回 sz 张合约的名义数量 Q = ctVal * sz * ctMult。
//
// 正向合约 Q 的单位是标的币（如 BTC），乘以价格得到计价币价值；
// 反向合约 Q 的单位是计价币（如 USD），除以价格得到标的币价值。
func (i Instrument) ContractQty(sz decimal.Decimal) decimal.Decimal {
	return i.CtVal.Mul(sz).Mul(i.CtMult)
}

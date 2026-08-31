package refdata

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// FeeRate 是一对挂单/吃单费率。
//
// 沿用 OKX 的符号约定：负数表示收取（从账户扣除），正数表示返佣。
// 实测 Lv1 永续为 maker=-0.0002、taker=-0.0005，成交明细里的 fee 字段同样带负号。
// 保留这个符号而不取绝对值，是为了让手续费能直接参与账户余额的加法运算。
type FeeRate struct {
	Maker decimal.Decimal
	Taker decimal.Decimal
}

// Of 返回该成交角色对应的费率。
func (r FeeRate) Of(e types.ExecType) decimal.Decimal {
	if e == types.Maker {
		return r.Maker
	}
	return r.Taker
}

// IsZero 报告挂单与吃单费率是否均为零。
func (r FeeRate) IsZero() bool { return r.Maker.IsZero() && r.Taker.IsZero() }

// FeeGroup 是一个费率组。合约规格里的 groupId 指向这里。
type FeeGroup struct {
	GroupID string
	FeeRate
}

// TradeFee 对应 GET /api/v5/account/trade-fee 的一条响应，按产品类型给出费率。
//
// 该接口需要鉴权且返回的是「当前账户」的费率——费率随用户近 30 日成交量与资产量
// 浮动，因此它属于会变的参数，既要能定期重新拉取，也要允许使用者直接指定。
type TradeFee struct {
	InstType types.InstType
	Level    types.FeeLevel
	Base     FeeRate         // maker/taker：默认费率，币本位合约用这一组
	U        FeeRate         // makerU/takerU：USDT 保证金合约
	USDC     FeeRate         // makerUSDC/takerUSDC：USDC 保证金合约
	Delivery decimal.Decimal // 交割手续费率，仅交割合约有值
	Exercise decimal.Decimal // 行权手续费率，仅期权有值
	Groups   []FeeGroup
	Ts       int64
}

type rawFeeGroup struct {
	GroupID string `json:"groupId"`
	Maker   string `json:"maker"`
	Taker   string `json:"taker"`
}

type rawTradeFee struct {
	InstType  string        `json:"instType"`
	Level     string        `json:"level"`
	Maker     string        `json:"maker"`
	Taker     string        `json:"taker"`
	MakerU    string        `json:"makerU"`
	TakerU    string        `json:"takerU"`
	MakerUSDC string        `json:"makerUSDC"`
	TakerUSDC string        `json:"takerUSDC"`
	Delivery  string        `json:"delivery"`
	Exercise  string        `json:"exercise"`
	FeeGroup  []rawFeeGroup `json:"feeGroup"`
	Ts        string        `json:"ts"`
}

func parseFeeRate(prefix, maker, taker string) (FeeRate, error) {
	var r FeeRate
	var err error
	if r.Maker, err = parseDec(prefix+"maker", maker); err != nil {
		return r, err
	}
	if r.Taker, err = parseDec(prefix+"taker", taker); err != nil {
		return r, err
	}
	return r, nil
}

// UnmarshalJSON 按 OKX 的线格式解析。
func (f *TradeFee) UnmarshalJSON(b []byte) error {
	var r rawTradeFee
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	f.InstType = types.InstType(r.InstType)
	f.Level = types.FeeLevel(r.Level)

	var err error
	if f.Base, err = parseFeeRate("", r.Maker, r.Taker); err != nil {
		return err
	}
	if f.U, err = parseFeeRate("U:", r.MakerU, r.TakerU); err != nil {
		return err
	}
	if f.USDC, err = parseFeeRate("USDC:", r.MakerUSDC, r.TakerUSDC); err != nil {
		return err
	}
	if f.Delivery, err = parseDec("delivery", r.Delivery); err != nil {
		return err
	}
	if f.Exercise, err = parseDec("exercise", r.Exercise); err != nil {
		return err
	}
	if f.Ts, err = parseMillis("ts", r.Ts); err != nil {
		return err
	}

	f.Groups = make([]FeeGroup, 0, len(r.FeeGroup))
	for _, g := range r.FeeGroup {
		rate, err := parseFeeRate("group "+g.GroupID+":", g.Maker, g.Taker)
		if err != nil {
			return err
		}
		f.Groups = append(f.Groups, FeeGroup{GroupID: g.GroupID, FeeRate: rate})
	}
	return nil
}

// MarshalJSON 按 OKX 的线格式输出，使快照中的费率可与真实响应直接比对。
func (f TradeFee) MarshalJSON() ([]byte, error) {
	groups := make([]rawFeeGroup, 0, len(f.Groups))
	for _, g := range f.Groups {
		groups = append(groups, rawFeeGroup{
			GroupID: g.GroupID,
			Maker:   formatDec(g.Maker),
			Taker:   formatDec(g.Taker),
		})
	}
	return json.Marshal(rawTradeFee{
		InstType:  f.InstType.String(),
		Level:     f.Level.String(),
		Maker:     formatDec(f.Base.Maker),
		Taker:     formatDec(f.Base.Taker),
		MakerU:    formatDec(f.U.Maker),
		TakerU:    formatDec(f.U.Taker),
		MakerUSDC: formatDec(f.USDC.Maker),
		TakerUSDC: formatDec(f.USDC.Taker),
		Delivery:  formatDec(f.Delivery),
		Exercise:  formatDec(f.Exercise),
		FeeGroup:  groups,
		Ts:        formatMillis(f.Ts),
	})
}

// Group 返回指定费率组的费率。
func (f TradeFee) Group(groupID string) (FeeRate, bool) {
	for _, g := range f.Groups {
		if g.GroupID == groupID {
			return g.FeeRate, true
		}
	}
	return FeeRate{}, false
}

// Rate 返回某个合约适用的挂单/吃单费率。
//
// 解析顺序为**费率组优先**：
//
//  1. 合约规格里的 groupId 能在费率表的 feeGroup 中匹配到，就用该组的费率
//  2. 否则按结算币种选择：USDT 保证金走 U 组，USDC 保证金走 USDC 组，
//     币本位合约走 Base 组
//  3. 对应组为零值时回落到 Base
//
// 这个优先级无法从数据中判定——实测全部 459 个永续合约的 groupId 均为 4，
// 且 SWAP 费率表 1..7 组与 maker/taker/makerU/takerU 的取值完全一致，
// 两条路径给出同一个数字。因此它是使用者拍板的决定，而非实测结论。
//
// 支持这个选择的依据：现货各费率组差异显著（组 3 taker 0.22%、组 8 是 0.4%、
// 组 11 免费），说明费率组是 OKX 用来做精细区分的维度；而结算币种变体更像是
// 面向整类合约的默认值。精细者优先是合理的读法。
func (f TradeFee) Rate(inst Instrument) FeeRate {
	if r, ok := f.Group(inst.GroupID); ok {
		return r
	}
	switch inst.SettleCcy {
	case "USDT":
		if !f.U.IsZero() {
			return f.U
		}
	case "USDC":
		if !f.USDC.IsZero() {
			return f.USDC
		}
	}
	return f.Base
}

// FeeSchedule 是按产品类型索引的费率表，可整体替换或按产品类型覆盖。
//
// 零值不可用，须经 NewFeeSchedule 构造。
type FeeSchedule struct {
	fees map[types.InstType]TradeFee
}

// NewFeeSchedule 由若干产品类型的费率构造费率表。
func NewFeeSchedule(fees ...TradeFee) FeeSchedule {
	m := make(map[types.InstType]TradeFee, len(fees))
	for _, f := range fees {
		m[f.InstType] = f
	}
	return FeeSchedule{fees: m}
}

// TradeFee 返回指定产品类型的费率明细。
func (s FeeSchedule) TradeFee(instType types.InstType) (TradeFee, bool) {
	f, ok := s.fees[instType]
	return f, ok
}

// Level 返回费率表所属的等级；各产品类型等级不一致或表为空时返回空值。
func (s FeeSchedule) Level() types.FeeLevel {
	var lv types.FeeLevel
	for _, f := range s.fees {
		if lv == "" {
			lv = f.Level
			continue
		}
		if f.Level != lv {
			return ""
		}
	}
	return lv
}

// Rate 返回某个合约适用的费率。
func (s FeeSchedule) Rate(inst Instrument) (FeeRate, error) {
	f, ok := s.fees[inst.InstType]
	if !ok {
		return FeeRate{}, okxerr.New(okxerr.CodeParamError,
			"费率表中没有产品类型 %s 的费率", inst.InstType)
	}
	return f.Rate(inst), nil
}

// WithRate 返回一份副本，其中指定产品类型的费率被整体替换。
//
// 供使用者指定自己的实际费率——协议费率、活动折扣、OKB 抵扣后的净费率都无法
// 从公开数据推导，只能由使用者给出。传入的 FeeRate 沿用 OKX 的符号约定：
// 负数表示收取。
//
// 覆盖是彻底的：Base、U、USDC 与全部费率组都会被替换成 r，因此无论 Rate 走
// 哪条解析路径，结果都是 r。仅替换前三者是不够的——Rate 以费率组优先，
// 漏掉费率组会让使用者显式设置的费率被静默忽略而用回 OKX 的默认值，
// 那是会在回测里直接算错钱的。费率组的编号予以保留，仅数值被替换。
func (s FeeSchedule) WithRate(instType types.InstType, r FeeRate) FeeSchedule {
	m := make(map[types.InstType]TradeFee, len(s.fees)+1)
	for k, v := range s.fees {
		m[k] = v
	}
	f := m[instType]
	f.InstType = instType
	f.Base, f.U, f.USDC = r, r, r

	groups := make([]FeeGroup, len(f.Groups))
	for i, g := range f.Groups {
		groups[i] = FeeGroup{GroupID: g.GroupID, FeeRate: r}
	}
	f.Groups = groups

	m[instType] = f
	return FeeSchedule{fees: m}
}

// WithLevel 返回一份副本，其中所有产品类型的等级标记被改写。
//
// 仅改写标记本身，不改动费率数值——等级到费率的映射表并未公开于任何免鉴权接口，
// 凭等级推算费率会引入无从验证的假设。要改费率请用 WithRate，
// 或重新拉取 /account/trade-fee。
func (s FeeSchedule) WithLevel(lv types.FeeLevel) FeeSchedule {
	m := make(map[types.InstType]TradeFee, len(s.fees))
	for k, v := range s.fees {
		v.Level = lv
		m[k] = v
	}
	return FeeSchedule{fees: m}
}

// instTypesSorted 返回费率表中的产品类型，按固定顺序排列，
// 使快照的序列化结果逐字节稳定、可做有意义的 diff。
func (s FeeSchedule) instTypesSorted() []types.InstType {
	out := make([]types.InstType, 0, len(s.fees))
	for it := range s.fees {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// String 便于排查问题时查看费率表内容。
func (s FeeSchedule) String() string {
	if len(s.fees) == 0 {
		return "FeeSchedule{空}"
	}
	out := fmt.Sprintf("FeeSchedule{等级=%s", s.Level())
	for _, it := range []types.InstType{
		types.InstSpot, types.InstSwap, types.InstFutures, types.InstOption,
	} {
		if f, ok := s.fees[it]; ok {
			out += fmt.Sprintf(" %s(maker=%s,taker=%s)", it, f.Base.Maker, f.Base.Taker)
		}
	}
	return out + "}"
}

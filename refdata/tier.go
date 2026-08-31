package refdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// ErrSizeExceedsMaxTier 表示持仓量超出了该品种档位表覆盖的最大范围。
//
// 真实 OKX 在这种情况下会拒绝开仓，其具体错误码待 v0.2.0 对拍时确认后补上。
var ErrSizeExceedsMaxTier = errors.New("持仓量超出档位表覆盖范围")

// ErrEmptyTierTable 表示档位表为空。
var ErrEmptyTierTable = errors.New("档位表为空")

// TierKey 是档位表的聚合键。
//
// 衍生品的档位按 instFamily 聚合，而不是按 instId —— 全仓模式下同一 instFamily
// 的多个合约（永续与各期交割）持仓必须合并后再查档位。这是 OKX 规则中最容易被
// 第三方实现搞错的一点，因此在类型层面就把 instFamily 定为聚合键，避免误用 instId。
//
// 逐仓与全仓是两张独立的档位表，故 MgnMode 也是键的一部分。
type TierKey struct {
	InstType types.InstType
	MgnMode  types.MgnMode
	Family   string // 衍生品为 instFamily
}

func (k TierKey) String() string {
	return fmt.Sprintf("%s:%s:%s", k.InstType, k.MgnMode, k.Family)
}

// ParseTierKey 解析 TierKey.String 产生的文本，用于快照文件的键还原。
func ParseTierKey(s string) (TierKey, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return TierKey{}, fmt.Errorf("非法的档位表键 %q：应为 产品类型:保证金模式:品种", s)
	}
	k := TierKey{
		InstType: types.InstType(parts[0]),
		MgnMode:  types.MgnMode(parts[1]),
		Family:   parts[2],
	}
	if !k.InstType.Valid() {
		return TierKey{}, fmt.Errorf("非法的档位表键 %q：产品类型 %q 无效", s, parts[0])
	}
	if !k.MgnMode.Valid() {
		return TierKey{}, fmt.Errorf("非法的档位表键 %q：保证金模式 %q 无效", s, parts[1])
	}
	if k.Family == "" {
		return TierKey{}, fmt.Errorf("非法的档位表键 %q：品种为空", s)
	}
	return k, nil
}

// PositionTier 是一个档位，字段与 OKX /api/v5/public/position-tiers 的响应对应。
type PositionTier struct {
	Tier     int             // 档位编号，从 1 开始
	MinSz    decimal.Decimal // 该档位最小持仓量（张）
	MaxSz    decimal.Decimal // 该档位最大持仓量（张）
	MMR      decimal.Decimal // 维持保证金率
	IMR      decimal.Decimal // 最低初始保证金率
	MaxLever decimal.Decimal // 该档位最大可用杠杆
}

type rawPositionTier struct {
	Tier     string `json:"tier"`
	MinSz    string `json:"minSz"`
	MaxSz    string `json:"maxSz"`
	MMR      string `json:"mmr"`
	IMR      string `json:"imr"`
	MaxLever string `json:"maxLever"`
}

// UnmarshalJSON 按 OKX 的线格式解析。
func (t *PositionTier) UnmarshalJSON(b []byte) error {
	var r rawPositionTier
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	tier, err := strconv.Atoi(r.Tier)
	if err != nil {
		return fmt.Errorf("字段 tier: 无法解析档位编号 %q: %w", r.Tier, err)
	}
	t.Tier = tier
	if t.MinSz, err = parseDec("minSz", r.MinSz); err != nil {
		return err
	}
	if t.MaxSz, err = parseDec("maxSz", r.MaxSz); err != nil {
		return err
	}
	if t.MMR, err = parseDec("mmr", r.MMR); err != nil {
		return err
	}
	if t.IMR, err = parseDec("imr", r.IMR); err != nil {
		return err
	}
	if t.MaxLever, err = parseDec("maxLever", r.MaxLever); err != nil {
		return err
	}
	return nil
}

// MarshalJSON 按 OKX 的线格式输出。
func (t PositionTier) MarshalJSON() ([]byte, error) {
	return json.Marshal(rawPositionTier{
		Tier:     strconv.Itoa(t.Tier),
		MinSz:    formatDec(t.MinSz),
		MaxSz:    formatDec(t.MaxSz),
		MMR:      formatDec(t.MMR),
		IMR:      formatDec(t.IMR),
		MaxLever: formatDec(t.MaxLever),
	})
}

// TierTable 是某个 (instType, mgnMode, instFamily) 下的完整档位表，按档位升序。
type TierTable struct {
	Key   TierKey
	Tiers []PositionTier
}

// NewTierTable 构造档位表，按 Tier 升序排序并校验基本一致性。
//
// 不校验档位之间「无缝衔接」——OKX 的真实数据里相邻档位存在刻意的间隙
// （如 tier1 上界 1000，tier2 下界 1000.01），这是为了配合 lotSz 的步长，
// 并非数据错误。查档因此以「首个满足 sz <= maxSz 的档位」为准，天然容忍间隙。
func NewTierTable(key TierKey, tiers []PositionTier) (*TierTable, error) {
	if len(tiers) == 0 {
		return nil, fmt.Errorf("%s: %w", key, ErrEmptyTierTable)
	}
	cp := make([]PositionTier, len(tiers))
	copy(cp, tiers)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Tier < cp[j].Tier })

	for i, t := range cp {
		if t.MaxSz.LessThan(t.MinSz) {
			return nil, fmt.Errorf("%s 档位 %d: maxSz(%s) 小于 minSz(%s)",
				key, t.Tier, t.MaxSz, t.MinSz)
		}
		if i > 0 && !t.MinSz.GreaterThan(cp[i-1].MaxSz) {
			return nil, fmt.Errorf("%s 档位 %d: minSz(%s) 未大于上一档的 maxSz(%s)",
				key, t.Tier, t.MinSz, cp[i-1].MaxSz)
		}
		if i > 0 && t.MMR.LessThan(cp[i-1].MMR) {
			return nil, fmt.Errorf("%s 档位 %d: mmr(%s) 小于上一档的 mmr(%s)，维持保证金率应随档位单调不减",
				key, t.Tier, t.MMR, cp[i-1].MMR)
		}
	}
	return &TierTable{Key: key, Tiers: cp}, nil
}

// Lookup 返回持仓量 sz（张）所处的档位，空头的负张数按绝对值处理。
//
// sz 应当是同一 instFamily 下所有相关仓位合并后的总张数，而非单个合约的张数。
//
// 维持保证金按「单档整体适用」计算，不是分层累进：落在第 N 档就整体乘以第 N 档的
// mmr，而不是逐档累加。这一点已与 OKX 官方文档核对。
//
// 判据是「首个满足 sz <= maxSz 的档位」，因此落在相邻档位间隙里的取值会归入
// 更高的那一档，即偏保守（更高的维持保证金率）。间隙值在实际中不可达——
// sz 必然是 lotSz 的整数倍，而间隙宽度正好小于 lotSz。
func (t *TierTable) Lookup(sz decimal.Decimal) (PositionTier, error) {
	if len(t.Tiers) == 0 {
		return PositionTier{}, fmt.Errorf("%s: %w", t.Key, ErrEmptyTierTable)
	}
	abs := sz.Abs()
	for _, tier := range t.Tiers {
		if abs.LessThanOrEqual(tier.MaxSz) {
			return tier, nil
		}
	}
	return PositionTier{}, fmt.Errorf("%s: 持仓量 %s 超出最大档位上限 %s: %w",
		t.Key, abs, t.MaxSize(), ErrSizeExceedsMaxTier)
}

// MaxSize 返回该品种允许的最大持仓量（最高档位的 maxSz）。
func (t *TierTable) MaxSize() decimal.Decimal {
	if len(t.Tiers) == 0 {
		return decimal.Zero
	}
	return t.Tiers[len(t.Tiers)-1].MaxSz
}

// MaxLeverage 返回该品种的最高可用杠杆（第一档的 maxLever）。
func (t *TierTable) MaxLeverage() decimal.Decimal {
	if len(t.Tiers) == 0 {
		return decimal.Zero
	}
	return t.Tiers[0].MaxLever
}

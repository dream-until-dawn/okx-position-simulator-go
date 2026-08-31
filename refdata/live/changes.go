package live

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
)

// ChangeKind 是一项变更的类型。
type ChangeKind string

const (
	Added    ChangeKind = "新增"
	Removed  ChangeKind = "移除"
	Modified ChangeKind = "变更"
)

// InstrumentChange 是一个合约规格的变更。
type InstrumentChange struct {
	Kind   ChangeKind
	InstID string
	Fields []string // 发生变化的字段，仅 Modified 时有值
	Before refdata.Instrument
	After  refdata.Instrument
}

func (c InstrumentChange) String() string {
	if c.Kind == Modified {
		return fmt.Sprintf("合约 %s %s: %s", c.InstID, c.Kind, strings.Join(c.Fields, ", "))
	}
	return fmt.Sprintf("合约 %s %s", c.InstID, c.Kind)
}

// TierTableChange 是一张档位表的变更。
type TierTableChange struct {
	Kind   ChangeKind
	Key    refdata.TierKey
	Detail string // 人可读的变化说明，仅 Modified 时有值
}

func (c TierTableChange) String() string {
	if c.Kind == Modified {
		return fmt.Sprintf("档位表 %s %s: %s", c.Key, c.Kind, c.Detail)
	}
	return fmt.Sprintf("档位表 %s %s", c.Key, c.Kind)
}

// Changes 描述两份规则数据之间的差异。
//
// 定期拉取若只是默默换掉数据，使用者就无从知晓 OKX 改了什么——
// 而档位表或合约面值的变动会直接改变仓位的风险指标，属于必须让人知道的事。
type Changes struct {
	FromVersion int64
	ToVersion   int64
	Instruments []InstrumentChange
	TierTables  []TierTableChange
}

// IsEmpty 报告是否没有任何变更。
func (c Changes) IsEmpty() bool {
	return len(c.Instruments) == 0 && len(c.TierTables) == 0
}

// Count 返回变更项总数。
func (c Changes) Count() int { return len(c.Instruments) + len(c.TierTables) }

func (c Changes) String() string {
	if c.IsEmpty() {
		return fmt.Sprintf("规则数据无变化（版本 %d → %d）", c.FromVersion, c.ToVersion)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "规则数据有 %d 项变化（版本 %d → %d）:", c.Count(), c.FromVersion, c.ToVersion)
	for _, ic := range c.Instruments {
		b.WriteString("\n  " + ic.String())
	}
	for _, tc := range c.TierTables {
		b.WriteString("\n  " + tc.String())
	}
	return b.String()
}

// Diff 比较两份快照，返回从 before 到 after 的变更。
//
// 只比较真正影响仓位与保证金计算的字段。上线时间、状态描述之类的字段变动
// 不会改变任何计算结果，报出来只会淹没真正重要的变更。
func Diff(before, after *refdata.Snapshot) Changes {
	c := Changes{}
	if before != nil {
		c.FromVersion = before.Version()
	}
	if after != nil {
		c.ToVersion = after.Version()
	}
	if before == nil || after == nil {
		return c
	}

	c.Instruments = diffInstruments(before, after)
	c.TierTables = diffTierTables(before, after)
	return c
}

func diffInstruments(before, after *refdata.Snapshot) []InstrumentChange {
	var out []InstrumentChange

	beforeIDs := before.InstrumentIDs()
	afterSet := make(map[string]bool, len(after.InstrumentIDs()))
	for _, id := range after.InstrumentIDs() {
		afterSet[id] = true
	}

	for _, id := range beforeIDs {
		b, _ := before.Instrument(id)
		a, err := after.Instrument(id)
		if err != nil {
			out = append(out, InstrumentChange{Kind: Removed, InstID: id, Before: b})
			continue
		}
		if fields := changedInstrumentFields(b, a); len(fields) > 0 {
			out = append(out, InstrumentChange{
				Kind: Modified, InstID: id, Fields: fields, Before: b, After: a,
			})
		}
	}

	beforeSet := make(map[string]bool, len(beforeIDs))
	for _, id := range beforeIDs {
		beforeSet[id] = true
	}
	for _, id := range after.InstrumentIDs() {
		if !beforeSet[id] {
			a, _ := after.Instrument(id)
			out = append(out, InstrumentChange{Kind: Added, InstID: id, After: a})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].InstID < out[j].InstID })
	return out
}

// changedInstrumentFields 返回两份合约规格之间发生变化的字段名。
func changedInstrumentFields(b, a refdata.Instrument) []string {
	var f []string
	add := func(name string, changed bool) {
		if changed {
			f = append(f, name)
		}
	}
	add("instFamily", b.InstFamily != a.InstFamily)
	add("settleCcy", b.SettleCcy != a.SettleCcy)
	add("ctType", b.CtType != a.CtType)
	add("ctVal", !b.CtVal.Equal(a.CtVal))
	add("ctMult", !b.CtMult.Equal(a.CtMult))
	add("ctValCcy", b.CtValCcy != a.CtValCcy)
	add("lotSz", !b.LotSz.Equal(a.LotSz))
	add("minSz", !b.MinSz.Equal(a.MinSz))
	add("tickSz", !b.TickSz.Equal(a.TickSz))
	add("lever", !b.Lever.Equal(a.Lever))
	add("maxLmtSz", !b.MaxLmtSz.Equal(a.MaxLmtSz))
	add("maxMktSz", !b.MaxMktSz.Equal(a.MaxMktSz))
	add("groupId", b.GroupID != a.GroupID)
	add("expTime", b.ExpTime != a.ExpTime)
	add("state", b.State != a.State)
	return f
}

func diffTierTables(before, after *refdata.Snapshot) []TierTableChange {
	var out []TierTableChange

	beforeKeys := before.TierKeys()
	beforeSet := make(map[refdata.TierKey]bool, len(beforeKeys))
	for _, k := range beforeKeys {
		beforeSet[k] = true
	}

	for _, k := range beforeKeys {
		b, _ := before.TierTable(k)
		a, err := after.TierTable(k)
		if err != nil {
			out = append(out, TierTableChange{Kind: Removed, Key: k})
			continue
		}
		if detail := tierTableDetail(b, a); detail != "" {
			out = append(out, TierTableChange{Kind: Modified, Key: k, Detail: detail})
		}
	}
	for _, k := range after.TierKeys() {
		if !beforeSet[k] {
			out = append(out, TierTableChange{Kind: Added, Key: k})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out
}

// tierTableDetail 描述两张档位表的差异；无差异时返回空串。
func tierTableDetail(b, a *refdata.TierTable) string {
	if len(b.Tiers) != len(a.Tiers) {
		return fmt.Sprintf("档位数 %d → %d", len(b.Tiers), len(a.Tiers))
	}
	var diffs []string
	for i := range b.Tiers {
		bt, at := b.Tiers[i], a.Tiers[i]
		var parts []string
		if !bt.MinSz.Equal(at.MinSz) || !bt.MaxSz.Equal(at.MaxSz) {
			parts = append(parts, fmt.Sprintf("区间 [%s,%s] → [%s,%s]",
				bt.MinSz, bt.MaxSz, at.MinSz, at.MaxSz))
		}
		if !bt.MMR.Equal(at.MMR) {
			parts = append(parts, fmt.Sprintf("mmr %s → %s", bt.MMR, at.MMR))
		}
		if !bt.IMR.Equal(at.IMR) {
			parts = append(parts, fmt.Sprintf("imr %s → %s", bt.IMR, at.IMR))
		}
		if !bt.MaxLever.Equal(at.MaxLever) {
			parts = append(parts, fmt.Sprintf("maxLever %s → %s", bt.MaxLever, at.MaxLever))
		}
		if len(parts) > 0 {
			diffs = append(diffs, fmt.Sprintf("第 %d 档 %s", bt.Tier, strings.Join(parts, "、")))
		}
	}
	// 档位数可达 99，全部列出会淹没信息，超出部分折叠
	const maxShown = 3
	if len(diffs) > maxShown {
		return fmt.Sprintf("%s（另有 %d 档变化）",
			strings.Join(diffs[:maxShown], "；"), len(diffs)-maxShown)
	}
	return strings.Join(diffs, "；")
}

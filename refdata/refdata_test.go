package refdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// testdata 里存放的是 OKX 公共接口的真实响应原文。
// 用真实数据而不是手工构造的期望值做测试，是为了避免把「我对规则的理解」
// 连同其中可能的错误一起固化下来。

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 testdata 失败: %v", err)
	}
	return b
}

func loadInstrument(t *testing.T, name string) Instrument {
	t.Helper()
	insts, err := DecodeResponse[Instrument](readTestdata(t, name))
	if err != nil {
		t.Fatalf("解析合约规格失败: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("期望 1 个合约，实际 %d 个", len(insts))
	}
	return insts[0]
}

func loadTierTable(t *testing.T, name string, key TierKey) *TierTable {
	t.Helper()
	tiers, err := DecodeResponse[PositionTier](readTestdata(t, name))
	if err != nil {
		t.Fatalf("解析档位表失败: %v", err)
	}
	tbl, err := NewTierTable(key, tiers)
	if err != nil {
		t.Fatalf("构造档位表失败: %v", err)
	}
	return tbl
}

func dec(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("非法测试数值 %q: %v", s, err)
	}
	return d
}

func eq(t *testing.T, got decimal.Decimal, want, field string) {
	t.Helper()
	if !got.Equal(dec(t, want)) {
		t.Errorf("%s = %s, 期望 %s", field, got, want)
	}
}

func TestInstrumentLinear(t *testing.T) {
	i := loadInstrument(t, "instruments-BTC-USDT-SWAP.json")

	if i.InstID != "BTC-USDT-SWAP" {
		t.Errorf("instId = %q", i.InstID)
	}
	if i.InstFamily != "BTC-USDT" {
		t.Errorf("instFamily = %q，档位表以此为聚合键，不能为空", i.InstFamily)
	}
	if i.InstType != types.InstSwap {
		t.Errorf("instType = %q", i.InstType)
	}
	if i.CtType != types.Linear {
		t.Errorf("ctType = %q，期望 linear", i.CtType)
	}
	if i.SettleCcy != "USDT" {
		t.Errorf("settleCcy = %q，正向合约应以计价币结算", i.SettleCcy)
	}
	if i.CtValCcy != "BTC" {
		t.Errorf("ctValCcy = %q，正向合约面值应以标的币计", i.CtValCcy)
	}
	eq(t, i.CtVal, "0.01", "ctVal")
	eq(t, i.CtMult, "1", "ctMult")
	eq(t, i.LotSz, "0.01", "lotSz")
	eq(t, i.MinSz, "0.01", "minSz")
	eq(t, i.TickSz, "0.1", "tickSz")
	eq(t, i.Lever, "100", "lever")

	if !i.IsPerpetual() {
		t.Error("IsPerpetual() = false，SWAP 应为永续")
	}
	if i.IsInverse() {
		t.Error("IsInverse() = true，期望正向合约")
	}
	if i.ExpTime != 0 {
		t.Errorf("expTime = %d，永续合约应为 0", i.ExpTime)
	}
	if !i.State.Tradable() {
		t.Errorf("state = %q，期望可交易", i.State)
	}

	// Q = ctVal * sz * ctMult：10 张 = 0.01 * 10 * 1 = 0.1 BTC
	eq(t, i.ContractQty(dec(t, "10")), "0.1", "ContractQty(10)")
}

func TestInstrumentInverse(t *testing.T) {
	i := loadInstrument(t, "instruments-BTC-USD-SWAP.json")

	if i.CtType != types.Inverse {
		t.Errorf("ctType = %q，期望 inverse", i.CtType)
	}
	if i.SettleCcy != "BTC" {
		t.Errorf("settleCcy = %q，反向合约应以标的币结算", i.SettleCcy)
	}
	if i.CtValCcy != "USD" {
		t.Errorf("ctValCcy = %q，反向合约面值应以计价币计", i.CtValCcy)
	}
	eq(t, i.CtVal, "100", "ctVal")
	eq(t, i.LotSz, "0.1", "lotSz")

	if !i.IsInverse() {
		t.Error("IsInverse() = false")
	}
	// Q = 100 * 3 * 1 = 300 USD
	eq(t, i.ContractQty(dec(t, "3")), "300", "ContractQty(3)")
}

func TestInstrumentJSONRoundTrip(t *testing.T) {
	orig := loadInstrument(t, "instruments-BTC-USDT-SWAP.json")

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var back Instrument
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if back.InstID != orig.InstID || back.CtType != orig.CtType ||
		!back.CtVal.Equal(orig.CtVal) || !back.LotSz.Equal(orig.LotSz) ||
		back.ExpTime != orig.ExpTime {
		t.Errorf("往返后不一致:\n原始 %+v\n往返 %+v", orig, back)
	}
}

func btcTable(t *testing.T) *TierTable {
	t.Helper()
	return loadTierTable(t, "position-tiers-SWAP-cross-BTC-USDT.json",
		TierKey{InstType: types.InstSwap, MgnMode: types.MgnCross, Family: "BTC-USDT"})
}

func TestTierTableParse(t *testing.T) {
	tbl := btcTable(t)

	if len(tbl.Tiers) != 99 {
		t.Errorf("档位数 = %d，期望 99", len(tbl.Tiers))
	}
	if tbl.Tiers[0].Tier != 1 {
		t.Errorf("首档编号 = %d，期望 1", tbl.Tiers[0].Tier)
	}
	eq(t, tbl.MaxLeverage(), "100", "MaxLeverage")

	// 与真实数据核对前三档
	want := []struct{ minSz, maxSz, mmr, imr, lever string }{
		{"0", "1000", "0.004", "0.01", "100"},
		{"1000.01", "5000", "0.005", "0.015", "66.66"},
		{"5000.01", "20000", "0.0075", "0.02", "50"},
	}
	for i, w := range want {
		tr := tbl.Tiers[i]
		eq(t, tr.MinSz, w.minSz, fmt.Sprintf("tier%d.minSz", i+1))
		eq(t, tr.MaxSz, w.maxSz, fmt.Sprintf("tier%d.maxSz", i+1))
		eq(t, tr.MMR, w.mmr, fmt.Sprintf("tier%d.mmr", i+1))
		eq(t, tr.IMR, w.imr, fmt.Sprintf("tier%d.imr", i+1))
		eq(t, tr.MaxLever, w.lever, fmt.Sprintf("tier%d.maxLever", i+1))
	}
}

// TestTierLookupBoundaries 是本包最关键的测试。
//
// OKX 相邻档位之间存在刻意的间隙（tier1 上界 1000，tier2 下界 1000.01），
// 间隙宽度跟随 lotSz。查档以「首个满足 sz <= maxSz 的档位」为准，
// 因此落在间隙里的取值会归入更高的那一档——这是保守方向（更高的维持保证金率），
// 宁可高估风险也不低估。
//
// 实际上间隙值不可达：sz 必然是 lotSz 的整数倍，而间隙宽度正好小于 lotSz。
// 这里仍然固定该行为，是为了让边界语义有明确定义而非依赖巧合。
func TestTierLookupBoundaries(t *testing.T) {
	tbl := btcTable(t)

	cases := []struct {
		sz   string
		tier int
		why  string
	}{
		{"0", 1, "空仓落在首档"},
		{"0.01", 1, "最小下单量"},
		{"999.99", 1, "首档上界之内"},
		{"1000", 1, "恰好等于首档上界，含上界"},
		{"1000.005", 2, "落在档位间隙内，保守归入更高档"},
		{"1000.01", 2, "恰好等于次档下界"},
		{"5000", 2, "次档上界"},
		{"5000.01", 3, "第三档下界"},
		{"20000", 3, "第三档上界"},
	}
	for _, c := range cases {
		got, err := tbl.Lookup(dec(t, c.sz))
		if err != nil {
			t.Errorf("Lookup(%s) 报错: %v", c.sz, err)
			continue
		}
		if got.Tier != c.tier {
			t.Errorf("Lookup(%s) = 档位 %d，期望 %d（%s）", c.sz, got.Tier, c.tier, c.why)
		}
	}
}

// TestTierLookupShortPosition 空头持仓的张数在模型中为负，查档应按绝对值。
func TestTierLookupShortPosition(t *testing.T) {
	tbl := btcTable(t)

	long, err := tbl.Lookup(dec(t, "3000"))
	if err != nil {
		t.Fatalf("多头查档失败: %v", err)
	}
	short, err := tbl.Lookup(dec(t, "-3000"))
	if err != nil {
		t.Fatalf("空头查档失败: %v", err)
	}
	if long.Tier != short.Tier {
		t.Errorf("多头档位 %d 与空头档位 %d 不一致，查档应按绝对值",
			long.Tier, short.Tier)
	}
}

func TestTierLookupExceedsMax(t *testing.T) {
	tbl := btcTable(t)

	over := tbl.MaxSize().Add(decimal.NewFromInt(1))
	if _, err := tbl.Lookup(over); !errors.Is(err, ErrSizeExceedsMaxTier) {
		t.Errorf("Lookup(超上限) 的错误 = %v，期望 ErrSizeExceedsMaxTier", err)
	}
}

// TestTierMMRMonotonic 维持保证金率必须随档位单调不减，杠杆上限单调不增。
func TestTierMMRMonotonic(t *testing.T) {
	tbl := btcTable(t)

	for i := 1; i < len(tbl.Tiers); i++ {
		prev, cur := tbl.Tiers[i-1], tbl.Tiers[i]
		if cur.MMR.LessThan(prev.MMR) {
			t.Errorf("档位 %d 的 mmr(%s) 小于档位 %d 的 mmr(%s)",
				cur.Tier, cur.MMR, prev.Tier, prev.MMR)
		}
		if cur.MaxLever.GreaterThan(prev.MaxLever) {
			t.Errorf("档位 %d 的 maxLever(%s) 大于档位 %d 的 maxLever(%s)",
				cur.Tier, cur.MaxLever, prev.Tier, prev.MaxLever)
		}
	}
}

// TestTierTablesArePerFamily 验证不同 instFamily 的档位表确实互不相同。
//
// 这条测试是对一个设计假设的守卫：曾设想过按「阶梯形状」去重以压缩内置快照，
// 实测证明 BTC 与 DOGE 连首档的 mmr 与杠杆都不同，去重不成立。
// 若将来有人再次尝试共享档位表，这条测试会失败。
func TestTierTablesArePerFamily(t *testing.T) {
	btc := btcTable(t)
	doge := loadTierTable(t, "position-tiers-SWAP-cross-DOGE-USDT.json",
		TierKey{InstType: types.InstSwap, MgnMode: types.MgnCross, Family: "DOGE-USDT"})

	if btc.Tiers[0].MMR.Equal(doge.Tiers[0].MMR) &&
		btc.Tiers[0].MaxLever.Equal(doge.Tiers[0].MaxLever) {
		t.Error("BTC 与 DOGE 的首档 mmr 与 maxLever 相同，与实测不符")
	}
	eq(t, doge.Tiers[0].MMR, "0.01", "DOGE 首档 mmr")
	eq(t, doge.MaxLeverage(), "50", "DOGE 最高杠杆")
}

func TestTierTableRejectsOverlap(t *testing.T) {
	key := TierKey{InstType: types.InstSwap, MgnMode: types.MgnCross, Family: "TEST"}
	tiers := []PositionTier{
		{Tier: 1, MinSz: decimal.Zero, MaxSz: decimal.NewFromInt(1000), MMR: dec(t, "0.004")},
		{Tier: 2, MinSz: decimal.NewFromInt(500), MaxSz: decimal.NewFromInt(5000), MMR: dec(t, "0.005")},
	}
	if _, err := NewTierTable(key, tiers); err == nil {
		t.Error("期望拒绝区间重叠的档位表")
	}
}

func TestTierTableRejectsNonMonotonicMMR(t *testing.T) {
	key := TierKey{InstType: types.InstSwap, MgnMode: types.MgnCross, Family: "TEST"}
	tiers := []PositionTier{
		{Tier: 1, MinSz: decimal.Zero, MaxSz: decimal.NewFromInt(1000), MMR: dec(t, "0.01")},
		{Tier: 2, MinSz: dec(t, "1000.01"), MaxSz: decimal.NewFromInt(5000), MMR: dec(t, "0.005")},
	}
	if _, err := NewTierTable(key, tiers); err == nil {
		t.Error("期望拒绝维持保证金率随档位下降的档位表")
	}
}

func TestTierKeyString(t *testing.T) {
	k := TierKey{InstType: types.InstSwap, MgnMode: types.MgnCross, Family: "BTC-USDT"}
	if got := k.String(); got != "SWAP:cross:BTC-USDT" {
		t.Errorf("TierKey.String() = %q", got)
	}
}

func TestDecodeResponseError(t *testing.T) {
	_, err := DecodeResponse[Instrument]([]byte(
		`{"code":"51001","data":[],"msg":"Instrument ID doesn't exist."}`))
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !okxerr.HasCode(err, okxerr.CodeInstNotExist) {
		t.Errorf("错误 = %v，期望携带错误码 51001", err)
	}
}

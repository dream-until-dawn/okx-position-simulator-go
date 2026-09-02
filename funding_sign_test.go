package okxsim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
)

// shortFundingFixture 是 testdata/conformance/funding-short-negative.json 的结构。
type shortFundingFixture struct {
	Instrument json.RawMessage `json:"instrument"`
	Tiers      json.RawMessage `json:"tiers"`
	Cases      []struct {
		UTC8   string `json:"utc8"`
		Sz     string `json:"sz"`
		Px     string `json:"px"`
		Rate   string `json:"rate"`
		Amount string `json:"amount"`
	} `json:"cases"`
}

// TestFundingSignShortNegativeRate 用**真实账单**钉住资金费的符号约定，
// 并补上四个象限里空着的那一个。
//
// 符号约定此前已有 TestFundingSign 覆盖，但那是合成数据，且只有三个象限：
//
//	费率 > 0，多头  ->  付    已有（合成）
//	费率 > 0，空头  ->  收    已有（合成）
//	费率 < 0，多头  ->  收    已有（合成）
//	费率 < 0，空头  ->  付    **空着**
//
// 第四格是两个负号叠在一起的结果，最容易写反，而写反之后金额的绝对值仍然
// 正确、只有方向反了——回测里不会报错，只会让盈亏悄悄偏移。变异
// `pos.IsLong() || f.Rate.IsNegative()` 只被本测试抓住，TestFundingSign 全绿。
//
// 另一半价值在「真实」二字上：合成测试只能确认代码与**我对约定的判断**
// 自洽，不能确认那个判断本身对。此前 24 个真实样本（funding-bills 20 条 +
// inverse-funding 4 条）全是多头 + 正费率，空头那侧从未被真实数据检验过。
//
// 数据取自 2026-09-01~09-02 的三次连续结算（ETH-USDT-SWAP 全仓空头
// 137.89 张），一次收两次付，其中一次费率 -0.00256 已接近下限。费率独立
// 取自 funding-rate-history，不是从金额反推的。
func TestFundingSignShortNegativeRate(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "funding-short-negative.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx shortFundingFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}

	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatal(err)
	}
	if inst.IsInverse() {
		t.Fatalf("%s 是反向合约，这份夹具要的是正向", inst.InstID)
	}
	var tiers []refdata.PositionTier
	if err := json.Unmarshal(fx.Tiers, &tiers); err != nil {
		t.Fatal(err)
	}
	tbl, err := refdata.NewTierTable(refdata.TierKey{
		InstType: types.InstSwap, MgnMode: types.MgnCross, Family: inst.InstFamily}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	// 夹具本身要守住：两个费率符号都在，否则这个测试就退化成了它要取代的那种
	var sawPos, sawNeg bool
	for _, c := range fx.Cases {
		if dec(c.Rate).IsNegative() {
			sawNeg = true
		} else {
			sawPos = true
		}
	}
	if !sawPos || !sawNeg {
		t.Fatalf("夹具须同时含正负费率，否则测不出符号约定：正=%v 负=%v", sawPos, sawNeg)
	}

	for _, c := range fx.Cases {
		t.Run(c.UTC8, func(t *testing.T) {
			snap := refdata.NewSnapshotBuilder(1).AddInstruments(inst).
				AddTierTable(tbl).SetFeeSchedule(refdata.DefaultFeeSchedule()).Build()
			s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Deposit(inst.SettleCcy, dec("100000")); err != nil {
				t.Fatal(err)
			}
			if err := s.SetPosition(Position{
				InstID: inst.InstID, MgnMode: types.MgnCross, PosSide: types.PosShort,
				Pos: dec(c.Sz), AvgPx: dec(c.Px), Lever: dec("10"),
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetMarkPx(inst.InstID, dec(c.Px)); err != nil {
				t.Fatal(err)
			}
			cashBefore := s.CashBal(inst.SettleCcy)

			rs, err := s.SettleFunding(inst.InstID, Funding{Rate: dec(c.Rate)}, 1)
			if err != nil {
				t.Fatalf("结算资金费失败: %v", err)
			}
			if len(rs) != 1 {
				t.Fatalf("应当结算一笔，实为 %d 笔", len(rs))
			}

			// 金额：正向代入独立取回的费率，与真实账单比
			near(t, rs[0].Amount, dec(c.Amount), "1e-16",
				"正向资金费 = ctVal x sz x 结算价 x 费率")

			// 方向：这才是这个测试存在的理由
			rateNeg := dec(c.Rate).IsNegative()
			if rateNeg && !rs[0].Amount.IsNegative() {
				t.Errorf("负费率下空头应当【支付】，金额须为负，实为 %s", rs[0].Amount)
			}
			if !rateNeg && !rs[0].Amount.IsPositive() {
				t.Errorf("正费率下空头应当【收取】，金额须为正，实为 %s", rs[0].Amount)
			}

			// 落点：全仓扣现金
			near(t, s.CashBal(inst.SettleCcy).Sub(cashBefore), rs[0].Amount, "1e-16",
				"全仓的资金费应从现金进出")
			after, _ := s.PositionOf(inst.InstID, types.PosShort)
			near(t, after.Funding, rs[0].Amount, "1e-16", "累计资金费记在仓位上")
		})
	}
}

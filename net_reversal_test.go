package okxsim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
)

// netReversalFixture 是 testdata/conformance/net-reversal.json 的结构。
type netReversalFixture struct {
	Instrument json.RawMessage `json:"instrument"`
	Tiers      json.RawMessage `json:"tiers"`
	Lever      string          `json:"lever"`
	Before     struct {
		Pos    string `json:"pos"`
		AvgPx  string `json:"avgPx"`
		Margin string `json:"margin"`
		CTime  string `json:"cTime"`
	} `json:"before"`
	Flip struct {
		Side      string `json:"side"`
		Sz        string `json:"sz"`
		FillPx    string `json:"fillPx"`
		AccFillSz string `json:"accFillSz"`
	} `json:"flip"`
	After struct {
		Pos    string `json:"pos"`
		AvgPx  string `json:"avgPx"`
		Margin string `json:"margin"`
		CTime  string `json:"cTime"`
	} `json:"after"`
}

// TestNetReversalAgainstRealFill 用真实成交锁定买卖模式的反手。
//
// 这是本仓补上的最后一块空白：在此之前，买卖模式**整体**没有对过真实 OKX
// ——全部夹具 113 个 posSide 取值里一个 net 都没有，反手只有合成单元测试。
//
// 数据取自 2026-09-02 的模拟盘，账户临时切到 net_mode，逐仓 10x：
//
//	反手前  pos=4    avgPx=2417.5    margin=96.7
//	一笔卖出 10 张，成交价 2415.42
//	反手后  pos=-6   avgPx=2415.42   margin=144.9252
//
// 一次确证了四条此前只有文档依据的断言：
//
//  1. **一笔委托可以直接反手**——OKX 既没拒绝也没把 sz 裁到 4 张。
//     这与开平仓模式下的「超量平仓被裁剪」（okx-rules.md §8）行为不同，
//     两种持仓方式在这里必须分开建模
//  2. 拆分是「平掉 4、反向开出 6」，不是整笔当平仓也不是整笔当开仓
//  3. 新仓位均价重置为本笔成交价
//  4. 建仓时刻重置
//
// 仍未实测：账单是一条还是两条。金额上「整笔计手续费」与「拆成平仓+开仓
// 各计」同值，只有条数与 subType 能分辨，而取数当时模拟盘的账单管线落后
// 数小时。见 fidelity.md §4。
func TestNetReversalAgainstRealFill(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "net-reversal.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx netReversalFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatal(err)
	}

	var tiers []refdata.PositionTier
	if err := json.Unmarshal(fx.Tiers, &tiers); err != nil {
		t.Fatal(err)
	}
	tbl, err := refdata.NewTierTable(refdata.TierKey{
		InstType: types.InstSwap, MgnMode: types.MgnIsolated,
		Family: inst.InstFamily}, tiers)
	if err != nil {
		t.Fatal(err)
	}
	snap := refdata.NewSnapshotBuilder(1).AddInstruments(inst).AddTierTable(tbl).
		SetFeeSchedule(refdata.DefaultFeeSchedule()).Build()
	s, err := New(Config{PosMode: types.NetMode, RefData: snap})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Deposit(inst.SettleCcy, dec("5000")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLeverage(inst.InstID, types.MgnIsolated, types.PosNet,
		dec(fx.Lever)); err != nil {
		t.Fatal(err)
	}

	// 建仓：买 4 张 @ 2417.5
	if _, err := s.Fill(Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Buy,
		Sz: dec(fx.Before.Pos), Px: dec(fx.Before.AvgPx), Ts: 1000,
	}); err != nil {
		t.Fatalf("建仓失败: %v", err)
	}
	before, _ := s.PositionOf(inst.InstID, types.PosNet)
	eq(t, before.Pos, fx.Before.Pos, "建仓后的净持仓")
	near(t, before.Margin, dec(fx.Before.Margin), "1e-12",
		"建仓后的逐仓保证金应与 OKX 一致")

	// 反手：一笔卖出 10 张 @ 2415.42
	res, err := s.Fill(Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Sell,
		Sz: dec(fx.Flip.Sz), Px: dec(fx.Flip.FillPx), Ts: 2000,
	})
	if err != nil {
		t.Fatalf("反手失败——若 OKX 也拒绝，本仓的反手路径就该删掉；"+
			"但实测 OKX 直接受理了: %v", err)
	}

	if !res.Reversed {
		t.Error("应当标记为反手")
	}
	eq(t, res.ClosedSz, fx.Before.Pos, "平掉的是原有的 4 张")
	eq(t, res.OpenedSz, "6", "反向开出 6 张")

	after, _ := s.PositionOf(inst.InstID, types.PosNet)
	eq(t, after.Pos, fx.After.Pos, "反手后为 6 张空头（净持仓记负）")
	eq(t, after.AvgPx, fx.After.AvgPx, "新仓位均价重置为本笔成交价")
	near(t, after.Margin, dec(fx.After.Margin), "1e-12",
		"保证金按新仓位重算：0.1 x 6 x 2415.42 / 10")

	// 建仓时刻重置——真实数据里 cTime 从 1788330742610 跳到 1788330755784
	if fx.Before.CTime == fx.After.CTime {
		t.Fatal("夹具里 cTime 没变，这条断言无从验起")
	}
	if after.CTime != 2000 {
		t.Errorf("建仓时刻应重置为本笔成交时刻 2000，实为 %d", after.CTime)
	}
}

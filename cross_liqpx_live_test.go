package okxsim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
)

// liveLiqPxFixture 是 testdata/conformance/cross-liqpx-live.json 的结构。
type liveLiqPxFixture struct {
	ExactCash string `json:"exactCash"`
	Position  struct {
		InstID  string `json:"instId"`
		PosSide string `json:"posSide"`
		Pos     string `json:"pos"`
		AvgPx   string `json:"avgPx"`
		MarkPx  string `json:"markPx"`
		LiqPx   string `json:"liqPx"`
		MMR     string `json:"mmr"`
		Lever   string `json:"lever"`
	} `json:"position"`
	OtherCcyEquity []struct {
		Ccy string `json:"ccy"`
		Eq  string `json:"eq"`
		IMR string `json:"imr"`
		MMR string `json:"mmr"`
	} `json:"otherCcyEquity"`
	Instrument json.RawMessage `json:"instrument"`
	Tiers      json.RawMessage `json:"tiers"`
}

// TestCrossLiqPxAgainstLivePosition 拿一个**活的**全仓仓位校验强平价，
// 并钉住「谁在兜底」。
//
// 快照取自 2026-09-02 的模拟盘（acctLv=2 现货合约模式），ETH-USDT-SWAP
// 全仓空头 137.89 张，OKX 当时给的 liqPx = 2480.7088557412476。
//
// 代入 OKX 报的 cashBal = 286.8952266692395，本仓算出
// 2480.708855741247893233，与 OKX 的 liqPx 相对差 1.18e-16——即 OKX 打印
// 精度的最后一位。
//
// 另有一重交叉验证：由三笔资金费独立推算出的现金是 286.8952266692395027
// （见 funding-short-negative.json），与 OKX 报的 cashBal 吻合到同样的位数。
// 两条互不相干的公式各自落在同一个现金上，互为对方的验证。
//
// # 只有结算币在兜底
//
// 快照当时账户还持有 0.964273 BTC（74,478 USD）与 100 OKB（10,889 USD），
// 两者的 imr/mmr 都是 0，对这个仓位一分不兜底——acctLv=2 下全仓保证金
// **按结算币逐币结算**，不跨币种汇总。
//
// 这一条错了不会报错，而且错的方向是**偏安全**的：把总权益 86,472 拿去算，
// 强平价会远到永远不触发，回测里一路绿灯，只是从不爆仓。所以这里显式存了
// 一笔 BTC 与 OKB，要求 LiqPx 一个数位都不许动。
func TestCrossLiqPxAgainstLivePosition(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "cross-liqpx-live.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx liveLiqPxFixture
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
		InstType: types.InstSwap, MgnMode: types.MgnCross, Family: inst.InstFamily}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	// build 造出与快照同构的账户；withOther 决定要不要把 BTC/OKB 也存进去
	build := func(t *testing.T, withOther bool) *Simulator {
		t.Helper()
		snap := refdata.NewSnapshotBuilder(1).AddInstruments(inst).
			AddTierTable(tbl).SetFeeSchedule(refdata.DefaultFeeSchedule()).Build()
		s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Deposit(inst.SettleCcy, dec(fx.ExactCash)); err != nil {
			t.Fatal(err)
		}
		if withOther {
			for _, o := range fx.OtherCcyEquity {
				if o.IMR != "0" || o.MMR != "0" {
					t.Fatalf("%s 的 imr/mmr 非零（%s/%s），夹具的前提变了",
						o.Ccy, o.IMR, o.MMR)
				}
				if err := s.Deposit(o.Ccy, dec(o.Eq)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := s.SetPosition(Position{
			InstID: inst.InstID, MgnMode: types.MgnCross, PosSide: types.PosShort,
			Pos: dec(fx.Position.Pos), AvgPx: dec(fx.Position.AvgPx),
			Lever: dec(fx.Position.Lever),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMarkPx(inst.InstID, dec(fx.Position.MarkPx)); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// 一、强平价对上 OKX 给的那个数
	s := build(t, false)
	cm, err := s.CrossMetricsOf(inst.SettleCcy)
	if err != nil {
		t.Fatal(err)
	}
	// 容差 1e-12 是量出来的，不是拍的：本仓算出 2480.708855741247893428，
	// OKX 打印 2480.7088557412476，差 2.93e-13（相对 1.18e-16）——那是 OKX 把
	// 输出截在 17 位有效数字，不是算法分歧。收到 1e-15 会红，说明这条容差
	// 卡在真实精度的边上，而不是宽到测不出东西。
	near(t, cm.LiqPx, dec(fx.Position.LiqPx), "1e-12",
		"全仓强平价 = (结算币现金 + k x avgPx) / (k x (1 + mmr率 + taker率))")

	// 顺带核一下维持保证金，确认档位取对了
	near(t, cm.MMR, dec(fx.Position.MMR), "1e-6", "维持保证金应与 OKX 一致")

	// 二、别的币种不许影响它
	s2 := build(t, true)
	// 先确认那两笔存款真的落地了——否则下面的守卫是空转的，
	// 而空转的守卫比没有守卫更坏：它看起来在守。
	for _, o := range fx.OtherCcyEquity {
		if got := s2.CashBal(o.Ccy); !got.Equal(dec(o.Eq)) {
			t.Fatalf("%s 的存款没落地：余额 %s，应为 %s——"+
				"下面那条守卫会因此空转", o.Ccy, got, o.Eq)
		}
	}
	cm2, err := s2.CrossMetricsOf(inst.SettleCcy)
	if err != nil {
		t.Fatal(err)
	}
	if !cm2.LiqPx.Equal(cm.LiqPx) {
		t.Errorf("存入 BTC 与 OKB 之后强平价变了：%s -> %s\n"+
			"acctLv=2 的全仓保证金按结算币逐币结算，别的币种不参与兜底；"+
			"实测快照里它们的 imr/mmr 都是 0", cm.LiqPx, cm2.LiqPx)
	}
	if !cm2.Equity.Equal(cm.Equity) {
		t.Errorf("存入 BTC 与 OKB 之后 %s 的权益变了：%s -> %s",
			inst.SettleCcy, cm.Equity, cm2.Equity)
	}
}

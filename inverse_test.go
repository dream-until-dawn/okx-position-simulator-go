package okxsim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// inverseFixture 是 testdata/conformance/inverse-position-metrics.json 的结构。
type inverseFixture struct {
	Instrument json.RawMessage            `json:"instrument"`
	Tiers      map[string]json.RawMessage `json:"tiers"`
	FeeRate    struct{ Maker, Taker string }
	Samples    []struct {
		Label     string           `json:"label"`
		BTC       map[string]any   `json:"btc"`
		Positions []map[string]any `json:"positions"`
	} `json:"samples"`
}

func loadInverseFixture(t *testing.T) (inverseFixture, *refdata.Snapshot) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "inverse-position-metrics.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx inverseFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatalf("解析合约规格失败: %v", err)
	}
	if !inst.IsInverse() {
		t.Fatalf("%s 不是反向合约，夹具用错了", inst.InstID)
	}
	sb := refdata.NewSnapshotBuilder(1).
		AddInstruments(inst).
		SetFeeSchedule(refdata.DefaultFeeSchedule().WithRate(types.InstSwap, refdata.FeeRate{
			Maker: dec(fx.FeeRate.Maker), Taker: dec(fx.FeeRate.Taker),
		}))
	for key, raw := range fx.Tiers {
		k, err := refdata.ParseTierKey(key)
		if err != nil {
			t.Fatalf("解析档位表键 %q 失败: %v", key, err)
		}
		var tiers []refdata.PositionTier
		if err := json.Unmarshal(raw, &tiers); err != nil {
			t.Fatalf("解析档位表 %q 失败: %v", key, err)
		}
		tbl, err := refdata.NewTierTable(k, tiers)
		if err != nil {
			t.Fatalf("构造档位表 %q 失败: %v", key, err)
		}
		sb.AddTierTable(tbl)
	}
	return fx, sb.Build()
}

// TestInverseAgainstRealAccount 用模拟盘上真实的币本位仓位逐项校验。
//
// 币本位与正向合约的差别只在名义价值的算法：Q 以计价币计，除以价格才得到标的币
// 金额，于是所有随名义变化的量都变成价格的反比。账户层完全不变，只是计价单位从
// USDT 换成 BTC。
//
// 覆盖逐仓多空、全仓多头，共 7 个仓位样本。期望值全部来自 OKX 的实际返回。
func TestInverseAgainstRealAccount(t *testing.T) {
	fx, snap := loadInverseFixture(t)

	for _, sm := range fx.Samples {
		if len(sm.Positions) == 0 {
			continue
		}
		t.Run(sm.Label, func(t *testing.T) {
			s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
			if err != nil {
				t.Fatalf("新建模拟器失败: %v", err)
			}
			if err := s.Deposit("BTC", dec(fieldOf(t, sm.BTC, "cashBal"))); err != nil {
				t.Fatalf("入金失败: %v", err)
			}
			for _, p := range sm.Positions {
				mgnMode := types.MgnMode(fieldOf(t, p, "mgnMode"))
				pos := Position{
					InstID:  fieldOf(t, p, "instId"),
					MgnMode: mgnMode,
					PosSide: types.PosSide(fieldOf(t, p, "posSide")),
					Pos:     dec(fieldOf(t, p, "pos")),
					AvgPx:   dec(fieldOf(t, p, "avgPx")),
					Lever:   dec(fieldOf(t, p, "lever")),
				}
				if m := fieldOf(t, p, "margin"); m != "" {
					pos.Margin = dec(m)
				}
				if err := s.SetPosition(pos); err != nil {
					t.Fatalf("置入仓位失败: %v", err)
				}
				s.SetMarkPx(pos.InstID, dec(fieldOf(t, p, "markPx")))
			}

			for _, p := range sm.Positions {
				side := types.PosSide(fieldOf(t, p, "posSide"))
				m, err := s.MetricsOf(fieldOf(t, p, "instId"), side)
				if err != nil {
					t.Fatalf("查询风险指标失败: %v", err)
				}
				// 币本位的量级很小（BTC 计价），用相对容差才有意义
				relNear(t, m.UPL, dec(fieldOf(t, p, "upl")), "未实现盈亏")
				relNear(t, m.UPLRatio, dec(fieldOf(t, p, "uplRatio")), "收益率")
				relNear(t, m.MMR, dec(fieldOf(t, p, "mmr")), "维持保证金")
				relNear(t, m.BePx, dec(fieldOf(t, p, "bePx")), "盈亏平衡价")
				if v := fieldOf(t, p, "imr"); v != "" {
					relNear(t, m.IMR, dec(v), "初始保证金")
				}
				if v := fieldOf(t, p, "mgnRatio"); v != "" {
					relNear(t, m.MgnRatio, dec(v), "保证金率")
				}
				if v := fieldOf(t, p, "liqPx"); v != "" {
					relNear(t, m.LiqPx, dec(v), "强平价")
				} else if !m.LiqPx.IsZero() {
					t.Errorf("OKX 未给出强平价，本库不应凭空算一个 %s", m.LiqPx)
				}
			}

			b, err := s.BalanceOf("BTC")
			if err != nil {
				t.Fatalf("查询余额失败: %v", err)
			}
			// 币种级只用余额响应内部的字段互验，避开与持仓响应的取数时点差异
			okx := func(k string) decimal.Decimal {
				v := fieldOf(t, sm.BTC, k)
				if v == "" {
					return decimal.Zero
				}
				return dec(v)
			}
			cash, imr := okx("cashBal"), okx("imr")
			crossUpl := okx("upl").Sub(okx("isoUpl"))
			relNear(t, okx("availBal"), cash.Add(crossUpl).Sub(imr),
				"OKX 的 availBal = 现金 + 全仓浮盈 − imr")
			relNear(t, okx("eq"), cash.Add(okx("isoEq")).Add(crossUpl),
				"OKX 的 eq = 现金 + 逐仓权益 + 全仓浮盈")
			relNear(t, okx("frozenBal"), okx("isoEq").Add(imr),
				"OKX 的 frozenBal = 逐仓权益 + imr")

			// 本库的实现要满足同一组公式
			eq(t, b.AvailBal, b.CashBal.Add(b.CrossUpl).Sub(b.IMR).String(), "本库的 AvailBal")
			eq(t, b.Eq, b.CashBal.Add(b.IsoEq).Add(b.CrossUpl).String(), "本库的 Eq")
			eq(t, b.FrozenBal, b.IsoEq.Add(b.IMR).String(), "本库的 FrozenBal")
		})
	}
}

// relNear 按相对容差比较。币本位的金额以标的币计，绝对值可以小到 1e-7，
// 绝对容差在这里没有意义。
func relNear(t *testing.T, got, want decimal.Decimal, field string) {
	t.Helper()
	const tol = "0.000001"
	if want.IsZero() {
		if !got.Abs().LessThan(dec("1e-12")) {
			t.Errorf("%s = %s，期望 0", field, got)
		}
		return
	}
	if diff := got.Sub(want).Abs().Div(want.Abs()); diff.GreaterThan(dec(tol)) {
		t.Errorf("%s = %s，OKX 为 %s，相对差 %s 超出 %s", field, got, want, diff, tol)
	}
}

// TestInverseShortLossIsBounded 锁定币本位空头亏损有上界这一特性。
//
// 反向空头的未实现亏损是 Q(1/markPx − 1/avgPx)，价格涨到无穷时收敛于 −Q/avgPx。
// 保证金若已覆盖这个上界，价格再怎么涨也爆不掉——正向合约的空头没有这个性质，
// 它的亏损随价格线性发散。
//
// 照搬正向的公式会在这里给出一个负的或者荒谬的强平价，因此单独锁住。
func TestInverseShortLossIsBounded(t *testing.T) {
	_, snap := loadInverseFixture(t)
	inst, err := snap.Instrument("BTC-USD-SWAP")
	if err != nil {
		t.Fatal(err)
	}
	// Q = 100 × 10 = 1000 USD，开仓均价 80000 -> 最大亏损 1000/80000 = 0.0125 BTC
	qty := inst.ContractQty(dec("10"))
	avgPx := dec("80000")
	maxLoss := div(qty, avgPx)
	eq(t, maxLoss, "0.0125", "反向空头的最大亏损")

	newSim := func(t *testing.T, margin decimal.Decimal) Metrics {
		t.Helper()
		s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Deposit("BTC", dec("10")); err != nil {
			t.Fatal(err)
		}
		if err := s.SetPosition(Position{
			InstID: "BTC-USD-SWAP", MgnMode: types.MgnIsolated, PosSide: types.PosShort,
			Pos: dec("10"), AvgPx: avgPx, Lever: dec("10"), Margin: margin,
		}); err != nil {
			t.Fatal(err)
		}
		s.SetMarkPx("BTC-USD-SWAP", avgPx)
		m, err := s.MetricsOf("BTC-USD-SWAP", types.PosShort)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	// 保证金远小于上界：有强平价，且高于开仓均价
	m := newSim(t, dec("0.00125"))
	if !m.LiqPx.IsPositive() {
		t.Fatalf("保证金不足以覆盖最大亏损时应当有强平价，实为 %s", m.LiqPx)
	}
	if m.LiqPx.LessThanOrEqual(avgPx) {
		t.Errorf("空头的强平价 %s 应高于开仓均价 %s", m.LiqPx, avgPx)
	}

	// 保证金超过上界：价格涨到无穷也爆不掉，不应给出强平价
	if m := newSim(t, maxLoss.Mul(dec("1.5"))); !m.LiqPx.IsZero() {
		t.Errorf("保证金已覆盖最大亏损，不应有强平价，实为 %s", m.LiqPx)
	}
	// 恰好等于上界也一样
	if m := newSim(t, maxLoss); !m.LiqPx.IsZero() {
		t.Errorf("保证金恰好等于最大亏损时不应有强平价，实为 %s", m.LiqPx)
	}
}

// TestCrossFundingChargesCash 锁定全仓的资金费落在现金余额上。
//
// 逐仓的资金费从仓位保证金扣、不动现金（真实账单里 balChg 恒为 0）。全仓的保证金
// 从未离开现金，仓位上的 Margin 恒为零——照逐仓的写法扣 Margin，资金费会被夹零
// 逻辑静默吞掉，持仓再久也不花一分钱。
func TestCrossFundingChargesCash(t *testing.T) {
	fx := loadCrossFixture(t)
	s, err := New(Config{PosMode: types.LongShortMode, RefData: crossSnapshot(t, fx)})
	if err != nil {
		t.Fatalf("新建模拟器失败: %v", err)
	}
	if err := s.Deposit("USDT", dec("10000")); err != nil {
		t.Fatalf("入金失败: %v", err)
	}
	if err := s.SetPosition(Position{
		InstID: "ETH-USDT-SWAP", MgnMode: types.MgnCross, PosSide: types.PosLong,
		Pos: dec("10"), AvgPx: dec("2400"), Lever: dec("10"),
	}); err != nil {
		t.Fatalf("置入仓位失败: %v", err)
	}
	s.SetMarkPx("ETH-USDT-SWAP", dec("2400"))

	before := s.CashBal("USDT")
	rs, err := s.SettleFunding("ETH-USDT-SWAP", Funding{Rate: dec("0.0001")}, 1)
	if err != nil {
		t.Fatalf("结算资金费失败: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("应当结算一笔资金费，实为 %d 笔", len(rs))
	}
	// 名义 0.1×10×2400 = 2400，费率 0.0001 -> 多头支付 0.24
	eq(t, rs[0].Amount, "-0.24", "多头支付的资金费")
	eq(t, s.CashBal("USDT"), before.Sub(dec("0.24")).String(), "全仓的资金费应从现金扣")

	p, _ := s.PositionOf("ETH-USDT-SWAP", types.PosLong)
	eq(t, p.Margin, "0", "全仓仓位的保证金始终为零")
	eq(t, p.Funding, "-0.24", "累计资金费仍记在仓位上")
}

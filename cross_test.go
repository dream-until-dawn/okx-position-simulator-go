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

// crossFixture 是 testdata/conformance/cross-position-metrics.json 的结构。
type crossFixture struct {
	Instruments map[string]json.RawMessage `json:"instruments"`
	Tiers       map[string]json.RawMessage `json:"tiers"`
	FeeRate     struct {
		Maker string `json:"maker"`
		Taker string `json:"taker"`
	} `json:"feeRate"`
	Scenarios []crossScenario `json:"scenarios"`
}

type crossScenario struct {
	Name    string `json:"name"`
	Samples []struct {
		Label     string           `json:"label"`
		USDT      map[string]any   `json:"usdt"`
		Positions []map[string]any `json:"positions"`
	} `json:"samples"`
	PendingOrder *struct {
		AppliesToSampleIndex int    `json:"appliesToSampleIndex"`
		InstID               string `json:"instId"`
		Side                 string `json:"side"`
		PosSide              string `json:"posSide"`
		Px                   string `json:"px"`
		Sz                   string `json:"sz"`
		Lever                string `json:"lever"`
	} `json:"pendingOrder"`
}

func loadCrossFixture(t *testing.T) crossFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "cross-position-metrics.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var f crossFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	return f
}

// crossSnapshot 由夹具装配一份快照。
//
// 必须用夹具自带的档位表，不能用内置快照：内置的取自生产环境，而这批数据采自
// 模拟盘，两者的档位区间实测有成倍差异，混用会让全部期望值失去意义。
func crossSnapshot(t *testing.T, fx crossFixture) *refdata.Snapshot {
	t.Helper()
	b := refdata.NewSnapshotBuilder(1)
	for _, raw := range fx.Instruments {
		var inst refdata.Instrument
		if err := json.Unmarshal(raw, &inst); err != nil {
			t.Fatalf("解析合约规格失败: %v", err)
		}
		b.AddInstruments(inst)
	}
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
		b.AddTierTable(tbl)
	}
	b.SetFeeSchedule(refdata.DefaultFeeSchedule().WithRate(types.InstSwap, refdata.FeeRate{
		Maker: dec(fx.FeeRate.Maker), Taker: dec(fx.FeeRate.Taker),
	}).WithRate(types.InstFutures, refdata.FeeRate{
		Maker: dec(fx.FeeRate.Maker), Taker: dec(fx.FeeRate.Taker),
	}))
	return b.Build()
}

// TestCrossAgainstRealAccount 用模拟盘上的真实全仓状态校验账务与风险公式。
//
// 13 个快照横跨三个场景：单个全仓仓位、两个交割期合并查档、同一合约多空并存
// 且带一笔挂单。期望值全部来自 OKX 的实际返回。
//
// 比对分三层，是被数据本身的性质逼出来的：余额响应与持仓响应并非同一瞬时，
// 两者之间有 1e-4 到 1e-2 量级的行情漂移。直接拿持仓算出来的数去对余额响应，
// 漂移会盖过真正要验的东西——全仓浮盈本身就只有 0.07 的量级，和漂移同量级。
// 因此：
//
//	仓位级   用持仓响应自身的 markPx 算，对持仓响应自身的字段，同一瞬时，可以严格
//	币种级   只用余额响应内部的字段互验，同一次响应内部必然自洽，可以严格
//	实现自洽 断言本库的 Balance 各字段之间满足同一组公式，精确相等
//
// 三层合起来既锁住了规则，也锁住了实现，而漂移一次都没进来。
func TestCrossAgainstRealAccount(t *testing.T) {
	fx := loadCrossFixture(t)
	snap := crossSnapshot(t, fx)
	const taker = "0.0005"

	for _, sc := range fx.Scenarios {
		// 夹具驱动的断言必须先确认样本非空：JSON 字段改名会让它反序列化成 nil，
		// 于是整个循环零次迭代、测试一路绿灯，而一条真实数据都没核对。
		if len(sc.Samples) == 0 {
			t.Fatalf("场景 %q 没有快照——夹具结构可能变了", sc.Name)
		}
		for i, sample := range sc.Samples {
			name := sc.Name
			if sample.Label != "" {
				name += "/" + sample.Label
			}
			t.Run(name, func(t *testing.T) {
				s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
				if err != nil {
					t.Fatalf("新建模拟器失败: %v", err)
				}
				if err := s.Deposit("USDT", dec(fieldOf(t, sample.USDT, "cashBal"))); err != nil {
					t.Fatalf("入金失败: %v", err)
				}
				for _, p := range sample.Positions {
					instID := fieldOf(t, p, "instId")
					if err := s.SetPosition(Position{
						InstID:  instID,
						MgnMode: types.MgnCross,
						PosSide: types.PosSide(fieldOf(t, p, "posSide")),
						Pos:     dec(fieldOf(t, p, "pos")),
						AvgPx:   dec(fieldOf(t, p, "avgPx")),
						Lever:   dec(fieldOf(t, p, "lever")),
					}); err != nil {
						t.Fatalf("置入仓位失败: %v", err)
					}
					s.SetMarkPx(instID, dec(fieldOf(t, p, "markPx")))
				}

				ordFee := decimal.Zero
				if po := sc.PendingOrder; po != nil && po.AppliesToSampleIndex == i {
					if err := s.SetLeverage(po.InstID, types.MgnCross,
						types.PosSide(po.PosSide), dec(po.Lever)); err != nil {
						t.Fatalf("设置杠杆失败: %v", err)
					}
					r, err := s.PlaceOrder(Order{
						OrdID: "pending", InstID: po.InstID, TdMode: types.TdCross,
						Side: types.Side(po.Side), PosSide: types.PosSide(po.PosSide),
						OrdType: types.OrdLimit, Px: dec(po.Px), Sz: dec(po.Sz),
					})
					if err != nil {
						t.Fatalf("挂单失败: %v", err)
					}
					ordFee = r.Cost.Fee
					// 全仓挂单的冻结按【委托价】算，与 OKX 的 ordFrozen 逐位相同
					eq(t, r.Cost.Margin, fieldOf(t, sample.USDT, "ordFrozen"), "挂单冻结的保证金")
					eq(t, r.Cost.Fee, "0.62625", "挂单预冻结的吃单手续费")
				}

				// ---- 第一层：仓位级，与持仓响应同一瞬时 ----
				for _, p := range sample.Positions {
					m, err := s.MetricsOf(fieldOf(t, p, "instId"),
						types.PosSide(fieldOf(t, p, "posSide")))
					if err != nil {
						t.Fatalf("查询风险指标失败: %v", err)
					}
					near(t, m.UPL, dec(fieldOf(t, p, "upl")), "0.0000001", "仓位未实现盈亏")
					near(t, m.IMR, dec(fieldOf(t, p, "imr")), "0.0000001", "仓位初始保证金")
					// 维持保证金隐含着查档结果：合并查档若算错，这一项会差 50%
					near(t, m.MMR, dec(fieldOf(t, p, "mmr")), "0.0000001", "仓位维持保证金")
					near(t, m.BePx, dec(fieldOf(t, p, "bePx")), "0.0000001", "盈亏平衡价")

					// 全仓强平价：OKX 给空串的情形本库返回零，给了值就必须对上。
					// 空串出现在两种情况下——同币种有多个合约（强平价无定义），
					// 或解为负（现金太厚，价格到不了）。
					if want := fieldOf(t, p, "liqPx"); want == "" {
						if !m.LiqPx.IsZero() {
							t.Errorf("OKX 未给出强平价，本库不应凭空算一个 %s", m.LiqPx)
						}
					} else {
						near(t, m.LiqPx, dec(want), "0.0000001", "全仓强平价")
					}
					if got := fieldOf(t, p, "margin"); got != "" {
						t.Errorf("全仓仓位的 margin 字段应为空串，实为 %q", got)
					}
				}

				// ---- 第二层：币种级公式，只用余额响应内部的字段互验 ----
				// 锁的是规则本身。这些等式若哪天不再成立，说明 OKX 改了账务模型，
				// 那时该改的是实现而不是测试。
				okx := func(k string) decimal.Decimal { return dec(fieldOf(t, sample.USDT, k)) }
				cash, imr, mmr := okx("cashBal"), okx("imr"), okx("mmr")
				isoEq, crossUpl := okx("isoEq"), okx("upl").Sub(okx("isoUpl"))

				near(t, okx("availBal"), cash.Add(crossUpl).Sub(imr).Sub(ordFee),
					"0.00000001", "OKX 的 availBal = 现金 + 全仓浮盈 − imr − 挂单手续费")
				near(t, okx("availEq"), okx("availBal"), "0.00000001", "OKX 的 availEq = availBal")
				near(t, okx("eq"), cash.Add(isoEq).Add(crossUpl),
					"0.00000001", "OKX 的 eq = 现金 + 逐仓权益 + 全仓浮盈")
				near(t, okx("frozenBal"), isoEq.Add(imr).Add(ordFee),
					"0.00000001", "OKX 的 frozenBal = 逐仓权益 + imr + 挂单手续费")

				// 保证金率的分母含各仓位的平仓手续费。挂单也进 mmr，但它没有可平的
				// 仓位，故不产生平仓手续费——这一项的取舍在末个快照上有 2e-4 的
				// 相对残差，与该快照上余额与持仓不同瞬时的表现一致，见 §12。
				closeFee := mmr.Div(dec(sampleMMRRate(t, sample.Positions, fx))).Mul(dec(taker))
				if den := mmr.Add(closeFee); !den.IsZero() {
					want := cash.Add(crossUpl).Div(den)
					if diff := want.Sub(okx("mgnRatio")).Abs(); diff.
						GreaterThan(okx("mgnRatio").Mul(dec("0.0003"))) {
						t.Errorf("OKX 的 mgnRatio %s 与 (现金+全仓浮盈)/(mmr+平仓费)=%s 相差 %s",
							okx("mgnRatio"), want, diff)
					}
				}

				// ---- 第三层：本库的实现是否满足同一组公式 ----
				b, err := s.BalanceOf("USDT")
				if err != nil {
					t.Fatalf("查询余额失败: %v", err)
				}
				eq(t, b.AvailBal, b.CashBal.Add(b.CrossUpl).Sub(b.IMR).Sub(ordFee).String(),
					"本库的 AvailBal")
				eq(t, b.AvailEq, b.AvailBal.String(), "本库的 AvailEq")
				eq(t, b.Eq, b.CashBal.Add(b.IsoEq).Add(b.CrossUpl).String(), "本库的 Eq")
				eq(t, b.FrozenBal, b.IsoEq.Add(b.IMR).Add(ordFee).String(), "本库的 FrozenBal")
				eq(t, b.OrdFrozen, sumOrderMargin(s).String(), "本库的 OrdFrozen")

				// 币种级的 imr/mmr 应当就是各仓位与挂单之和
				var sumIMR, sumMMR decimal.Decimal
				for _, p := range sample.Positions {
					m, _ := s.MetricsOf(fieldOf(t, p, "instId"),
						types.PosSide(fieldOf(t, p, "posSide")))
					sumIMR, sumMMR = sumIMR.Add(m.IMR), sumMMR.Add(m.MMR)
				}
				for _, o := range s.PendingOrders("") {
					sumIMR = sumIMR.Add(o.Cost.Margin)
				}
				near(t, b.IMR, sumIMR, "0.0000001", "本库的 IMR = 各仓位与挂单之和")
				if b.MMR.LessThan(sumMMR) {
					t.Errorf("本库的 MMR %s 不应小于各仓位之和 %s", b.MMR, sumMMR)
				}

				// 全仓仓位报出的保证金率就是币种级那个值
				for _, p := range sample.Positions {
					m, _ := s.MetricsOf(fieldOf(t, p, "instId"),
						types.PosSide(fieldOf(t, p, "posSide")))
					if !m.MgnRatio.Equal(b.MgnRatio) {
						t.Errorf("全仓仓位的保证金率 = %s，应与币种级的 %s 相同",
							m.MgnRatio, b.MgnRatio)
					}
				}
			})
		}
	}
}

// sampleMMRRate 由样本中任一仓位反推该快照所处档位的维持保证金率。
func sampleMMRRate(t *testing.T, positions []map[string]any, fx crossFixture) string {
	t.Helper()
	if len(positions) == 0 {
		t.Fatal("样本没有仓位")
	}
	p := positions[0]
	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instruments[fieldOf(t, p, "instId")], &inst); err != nil {
		t.Fatalf("解析合约规格失败: %v", err)
	}
	nom := notional(inst, dec(fieldOf(t, p, "pos")), dec(fieldOf(t, p, "markPx")))
	return div(dec(fieldOf(t, p, "mmr")), nom).String()
}

func sumOrderMargin(s *Simulator) decimal.Decimal {
	var out decimal.Decimal
	for _, o := range s.PendingOrders("") {
		out = out.Add(o.Cost.Margin)
	}
	return out
}

// TestCrossTierMergesByFamily 锁定全仓的合并查档。
//
// 这是全仓与逐仓最容易被第三方实现搞错的一点：同一 instFamily 下的全仓持仓要
// 合并后再查档，而且按张数【绝对值】相加，多空不相抵。
//
// 实测：两个交割期各 7000 张，先开的那条腿一张未动，仅因另一期开了仓，其维持
// 保证金率就从 0.01 跳到 0.015。
func TestCrossTierMergesByFamily(t *testing.T) {
	fx := loadCrossFixture(t)
	snap := crossSnapshot(t, fx)

	newS := func(t *testing.T) *Simulator {
		t.Helper()
		s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
		if err != nil {
			t.Fatalf("新建模拟器失败: %v", err)
		}
		if err := s.Deposit("USDT", dec("100000")); err != nil {
			t.Fatalf("入金失败: %v", err)
		}
		return s
	}
	put := func(t *testing.T, s *Simulator, instID string, side types.PosSide, sz string) {
		t.Helper()
		if err := s.SetPosition(Position{
			InstID: instID, MgnMode: types.MgnCross, PosSide: side,
			Pos: dec(sz), AvgPx: dec("0.359"), Lever: dec("20"),
		}); err != nil {
			t.Fatalf("置入仓位失败: %v", err)
		}
		s.SetMarkPx(instID, dec("0.359"))
	}
	mmrRate := func(t *testing.T, s *Simulator, instID string, side types.PosSide) decimal.Decimal {
		t.Helper()
		m, err := s.MetricsOf(instID, side)
		if err != nil {
			t.Fatalf("查询风险指标失败: %v", err)
		}
		return m.MMRRate
	}

	// 一档上限 12500 张，二档 mmr 0.015
	t.Run("单腿在一档", func(t *testing.T) {
		s := newS(t)
		put(t, s, "GRASS-USDT-260911", types.PosLong, "7000")
		eq(t, mmrRate(t, s, "GRASS-USDT-260911", types.PosLong), "0.01", "单腿 7000 张的维持保证金率")
	})

	t.Run("跨交割期合并后进二档", func(t *testing.T) {
		s := newS(t)
		put(t, s, "GRASS-USDT-260911", types.PosLong, "7000")
		put(t, s, "GRASS-USDT-260925", types.PosLong, "7000")
		eq(t, mmrRate(t, s, "GRASS-USDT-260911", types.PosLong), "0.015",
			"先开的那条腿一张未动，也应随合计张数进二档")
		eq(t, mmrRate(t, s, "GRASS-USDT-260925", types.PosLong), "0.015", "后开的那条腿")
	})

	t.Run("多空按绝对值相加而非净额", func(t *testing.T) {
		s := newS(t)
		put(t, s, "GRASS-USDT-260911", types.PosLong, "7000")
		put(t, s, "GRASS-USDT-260911", types.PosShort, "7000")
		eq(t, mmrRate(t, s, "GRASS-USDT-260911", types.PosLong), "0.015",
			"净额为零但绝对值合计 14000，应进二档")
	})

	t.Run("逐仓每个仓位单独查档", func(t *testing.T) {
		s := newS(t)
		for _, side := range []types.PosSide{types.PosLong, types.PosShort} {
			if err := s.SetPosition(Position{
				InstID: "GRASS-USDT-260911", MgnMode: types.MgnIsolated, PosSide: side,
				Pos: dec("7000"), AvgPx: dec("0.359"), Lever: dec("20"), Margin: dec("125.65"),
			}); err != nil {
				t.Fatalf("置入仓位失败: %v", err)
			}
		}
		s.SetMarkPx("GRASS-USDT-260911", dec("0.359"))
		// 逐仓与全仓是两张不同的档位表，此处只断言「没有被合并」——
		// 合并的话 14000 张会落到比单独查档更高的档位上。
		single := mmrRate(t, s, "GRASS-USDT-260911", types.PosLong)
		s2 := newS(t)
		put(t, s2, "GRASS-USDT-260911", types.PosLong, "7000")
		put(t, s2, "GRASS-USDT-260911", types.PosShort, "7000")
		if merged := mmrRate(t, s2, "GRASS-USDT-260911", types.PosLong); single.GreaterThanOrEqual(merged) {
			t.Errorf("逐仓维持保证金率 %s 不应达到全仓合并后的 %s——逐仓是每个仓位单独查档的",
				single, merged)
		}
	})
}

// TestCrossFillLeavesMarginInCash 锁定全仓开仓只扣手续费。
//
// 实测：全仓开仓后现金余额只减少了该单的手续费，保证金分文未动；OKX 在全仓
// 仓位上把 margin 字段返回为空串，正是因为根本没有划过这笔钱。
func TestCrossFillLeavesMarginInCash(t *testing.T) {
	fx := loadCrossFixture(t)
	s, err := New(Config{PosMode: types.LongShortMode, RefData: crossSnapshot(t, fx)})
	if err != nil {
		t.Fatalf("新建模拟器失败: %v", err)
	}
	if err := s.Deposit("USDT", dec("10000")); err != nil {
		t.Fatalf("入金失败: %v", err)
	}
	if err := s.SetLeverage("ETH-USDT-SWAP", types.MgnCross, types.PosLong, dec("10")); err != nil {
		t.Fatalf("设置杠杆失败: %v", err)
	}

	r, err := s.Fill(Fill{
		InstID: "ETH-USDT-SWAP", TdMode: types.TdCross, Side: types.Buy,
		PosSide: types.PosLong, Sz: dec("2"), Px: dec("2445.6"), ExecType: types.Taker,
	})
	if err != nil {
		t.Fatalf("开仓失败: %v", err)
	}
	// 名义价值 0.1×2×2445.6 = 489.12，吃单费率 0.0005 -> 手续费 0.24456
	eq(t, r.Fee, "-0.24456", "开仓手续费")
	eq(t, r.After.Margin, "0", "全仓仓位不划入保证金")
	eq(t, s.CashBal("USDT"), "9999.75544", "全仓开仓后现金只少了手续费")

	b, err := s.BalanceOf("USDT")
	if err != nil {
		t.Fatalf("查询余额失败: %v", err)
	}
	eq(t, b.IsoEq, "0", "全仓不产生逐仓权益")
	eq(t, b.IMR, "48.912", "全仓初始保证金占用 = 名义/杠杆")
	// 可用余额 = 现金 + 浮盈 − imr。未推送行情时以开仓均价为标记价，浮盈为零。
	eq(t, b.AvailBal, "9950.84344", "可用余额扣掉了被占用的初始保证金")

	// 全平：现金收回盈亏与手续费，占用归零
	if _, err := s.Fill(Fill{
		InstID: "ETH-USDT-SWAP", TdMode: types.TdCross, Side: types.Sell,
		PosSide: types.PosLong, Sz: dec("2"), Px: dec("2445.6"), ExecType: types.Taker,
	}); err != nil {
		t.Fatalf("平仓失败: %v", err)
	}
	if _, ok := s.PositionOf("ETH-USDT-SWAP", types.PosLong); ok {
		t.Error("全平后仓位应被移除")
	}
	b, _ = s.BalanceOf("USDT")
	eq(t, b.IMR, "0", "全平后不再占用初始保证金")
	eq(t, s.CashBal("USDT"), "9999.51088", "两笔手续费之外现金不应有其他变化")
}

// TestBreakEvenPx 锁定盈亏平衡价。
//
// 实测样本：avgPx 2445.6、吃单费率 0.0005，OKX 的 bePx 为 2448.0468234117056。
// 平仓那笔手续费按平仓价收取，故未知数在等式两侧都出现——写成 avgPx×(1+2×taker)
// 的近似式会给出 2448.0456，差 1.2 个价位。
func TestBreakEvenPx(t *testing.T) {
	long := breakEvenPx(true, dec("2445.6"), dec("0.0005"))
	// OKX 截断到 17 位有效数字，本库保留更多位，故按容差比
	near(t, long, dec("2448.0468234117056"), "0.0000000001", "多头盈亏平衡价")

	short := breakEvenPx(false, dec("2445.6"), dec("0.0005"))
	if !short.LessThan(dec("2445.6")) {
		t.Errorf("空头盈亏平衡价 %s 应低于开仓均价", short)
	}
	// 多空两侧相对开仓均价的偏移应当对称
	up := long.Sub(dec("2445.6"))
	down := dec("2445.6").Sub(short)
	if diff := up.Sub(down).Abs(); diff.GreaterThan(dec("0.01")) {
		t.Errorf("多空两侧偏移 %s 与 %s 不对称，差 %s", up, down, diff)
	}
}

// TestCrossLiquidation 驱动一次全仓强平，验证触发点与结算。
//
// 触发判据（币种全仓保证金率 ≤ 1）由 13 个真实快照核对过；触发之后的结算方式
// 是从已实测的逐仓强平平移过来的，尚未实测，见 checkCrossLiquidation 的说明。
func TestCrossLiquidation(t *testing.T) {
	fx := loadCrossFixture(t)
	s, err := New(Config{PosMode: types.LongShortMode, RefData: crossSnapshot(t, fx)})
	if err != nil {
		t.Fatalf("新建模拟器失败: %v", err)
	}
	if err := s.Deposit("USDT", dec("200")); err != nil {
		t.Fatalf("入金失败: %v", err)
	}
	if err := s.SetLeverage("ETH-USDT-SWAP", types.MgnCross, types.PosLong, dec("50")); err != nil {
		t.Fatalf("设置杠杆失败: %v", err)
	}
	// 名义 0.1×40×2400 = 9600，50 倍杠杆占用 192，几乎吃满 200 的现金
	if _, err := s.Fill(Fill{
		InstID: "ETH-USDT-SWAP", TdMode: types.TdCross, Side: types.Buy,
		PosSide: types.PosLong, Sz: dec("40"), Px: dec("2400"), ExecType: types.Taker,
	}); err != nil {
		t.Fatalf("开仓失败: %v", err)
	}

	cm, err := s.CrossMetricsOf("USDT")
	if err != nil {
		t.Fatalf("查询全仓指标失败: %v", err)
	}
	if !cm.LiqPx.IsPositive() {
		t.Fatalf("单个全仓仓位应当算得出强平价，实为 %s", cm.LiqPx)
	}
	if cm.LiqPx.GreaterThanOrEqual(dec("2400")) {
		t.Errorf("多头的强平价 %s 应当低于开仓价", cm.LiqPx)
	}
	if cm.IsLiquidatable() {
		t.Fatal("刚开仓就被判定为可强平，触发判据有误")
	}

	// 推到强平价之上一点，不应触发
	step, err := s.Advance(Bar{
		InstID: "ETH-USDT-SWAP", Last: cm.LiqPx.Mul(dec("1.01")), Ts: 1,
	})
	if err != nil {
		t.Fatalf("推进行情失败: %v", err)
	}
	if len(step.Liquidations) != 0 {
		t.Errorf("强平价之上不应触发强平，实际发生 %d 次", len(step.Liquidations))
	}

	// 跌破强平价
	step, err = s.Advance(Bar{
		InstID: "ETH-USDT-SWAP", Last: cm.LiqPx.Mul(dec("0.99")), Ts: 2,
	})
	if err != nil {
		t.Fatalf("推进行情失败: %v", err)
	}
	if len(step.Liquidations) != 1 {
		t.Fatalf("跌破强平价应当触发一次强平，实际 %d 次", len(step.Liquidations))
	}
	l := step.Liquidations[0]
	if l.MgnMode != types.MgnCross {
		t.Errorf("强平的保证金模式 = %s，期望 cross", l.MgnMode)
	}
	if l.Kind != LiqFull {
		t.Errorf("全仓强平当前一律全平，实为 %s", l.Kind)
	}
	eq(t, l.Sz, "40", "被平掉的张数")
	if !l.Penalty.IsNegative() {
		t.Errorf("爆仓罚金应为负数（表示收取），实为 %s", l.Penalty)
	}
	if _, ok := s.PositionOf("ETH-USDT-SWAP", types.PosLong); ok {
		t.Error("全仓强平后仓位应被移除")
	}
	if s.CashBal("USDT").IsNegative() {
		t.Errorf("强平后现金余额不应为负，实为 %s", s.CashBal("USDT"))
	}

	// 损失来自现金而非仓位保证金——这是全仓与逐仓最实质的差别
	if !l.Loss.IsPositive() {
		t.Errorf("全仓强平应当有实际损失，实为 %s", l.Loss)
	}
	if l.Loss.GreaterThan(dec("200")) {
		t.Errorf("损失 %s 不应超过入金的 200", l.Loss)
	}
}

// TestCrossLiquidationClosesWholeCurrency 验证全仓强平以结算币种为单位。
//
// 同币种下的全仓仓位共担一份权益，因此一旦触发，该币种下的全仓仓位一并了结——
// 哪怕本步推进的行情只属于其中一个合约。
func TestCrossLiquidationClosesWholeCurrency(t *testing.T) {
	fx := loadCrossFixture(t)
	s, err := New(Config{PosMode: types.LongShortMode, RefData: crossSnapshot(t, fx)})
	if err != nil {
		t.Fatalf("新建模拟器失败: %v", err)
	}
	if err := s.Deposit("USDT", dec("300")); err != nil {
		t.Fatalf("入金失败: %v", err)
	}
	for _, inst := range []string{"GRASS-USDT-260911", "GRASS-USDT-260925"} {
		if err := s.SetLeverage(inst, types.MgnCross, types.PosLong, dec("50")); err != nil {
			t.Fatalf("设置杠杆失败: %v", err)
		}
		if err := s.SetPosition(Position{
			InstID: inst, MgnMode: types.MgnCross, PosSide: types.PosLong,
			Pos: dec("6000"), AvgPx: dec("0.359"), Lever: dec("50"),
		}); err != nil {
			t.Fatalf("置入仓位失败: %v", err)
		}
		s.SetMarkPx(inst, dec("0.359"))
	}
	// 合计 12000 张，两个合约同属 GRASS-USDT 家族，合并后仍在一档（上限 12500）
	if cm, _ := s.CrossMetricsOf("USDT"); cm.IsLiquidatable() {
		t.Fatal("初始状态不应已可强平")
	}
	if cm, _ := s.CrossMetricsOf("USDT"); !cm.LiqPx.IsZero() {
		t.Errorf("同币种下有两个合约时不应给出强平价，实为 %s", cm.LiqPx)
	}

	// 只推其中一个合约的行情，另一个的标记价不动
	step, err := s.Advance(Bar{InstID: "GRASS-USDT-260911", Last: dec("0.31"), Ts: 1})
	if err != nil {
		t.Fatalf("推进行情失败: %v", err)
	}
	if len(step.Liquidations) != 2 {
		t.Fatalf("该币种下两个全仓仓位应一并了结，实际强平 %d 个", len(step.Liquidations))
	}
	seen := map[string]bool{}
	for _, l := range step.Liquidations {
		seen[l.InstID] = true
		if l.MgnMode != types.MgnCross {
			t.Errorf("%s 的保证金模式 = %s，期望 cross", l.InstID, l.MgnMode)
		}
	}
	if !seen["GRASS-USDT-260911"] || !seen["GRASS-USDT-260925"] {
		t.Errorf("两个合约都应被平掉，实际只有 %v", seen)
	}
	if len(s.Positions()) != 0 {
		t.Errorf("强平后不应还有持仓，实际 %d 个", len(s.Positions()))
	}
}

// TestMaxSizeAgainstRealAccount 用 OKX 的 max-size 实测值锁定两种模式的取价规则。
//
// 两种模式在这里分道扬镳，且差距是数量级的：同一账户、同一杠杆，以 1712 挂卖单
// （标记价 2446），逐仓能开 122.49 张，全仓只能开 30.74 张。差的正是「开仓瞬间的
// 盯市浮亏」——全仓的浮亏直接扣可用余额，逐仓的落在仓位保证金上。
//
// 容差取 1%：标记价与 max-size 是两次调用，其间有 0.1~0.9 的漂移，而用错公式时
// 差的是 300%，1% 足以区分。
func TestMaxSizeAgainstRealAccount(t *testing.T) {
	for _, name := range []string{"max-size.json", "max-size-inverse.json"} {
		t.Run(name, func(t *testing.T) { maxSizeConformance(t, name) })
	}
}

func maxSizeConformance(t *testing.T, name string) {
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", name))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var fx struct {
		Instrument json.RawMessage            `json:"instrument"`
		Tiers      map[string]json.RawMessage `json:"tiers"`
		Lever      string                     `json:"lever"`
		FeeRate    struct{ Maker, Taker string }
		Samples    []struct {
			TdMode   string `json:"tdMode"`
			Px       string `json:"px"`
			MarkPx   string `json:"markPx"`
			AvailBal string `json:"availBal"`
			MaxBuy   string `json:"maxBuy"`
			MaxSell  string `json:"maxSell"`
		} `json:"samples"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatalf("解析合约规格失败: %v", err)
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
	snap := sb.Build()

	if len(fx.Samples) < 10 {
		t.Fatalf("只有 %d 个样本，夹具应当有 14 个——反序列化可能出问题了", len(fx.Samples))
	}
	for _, sm := range fx.Samples {
		t.Run(sm.TdMode+"/"+sm.Px, func(t *testing.T) {
			s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
			if err != nil {
				t.Fatalf("新建模拟器失败: %v", err)
			}
			// 币本位的结算币是标的币，入金币种要照合约走
			if err := s.Deposit(inst.SettleCcy, dec(sm.AvailBal)); err != nil {
				t.Fatalf("入金失败: %v", err)
			}
			mgnMode := types.MgnIsolated
			if sm.TdMode == "cross" {
				mgnMode = types.MgnCross
			}
			for _, side := range []types.PosSide{types.PosLong, types.PosShort} {
				if err := s.SetLeverage(inst.InstID, mgnMode, side, dec(fx.Lever)); err != nil {
					t.Fatalf("设置杠杆失败: %v", err)
				}
			}
			s.SetMarkPx(inst.InstID, dec(sm.MarkPx))

			m, err := s.MaxSize(inst.InstID, types.TdMode(sm.TdMode), dec(sm.Px))
			if err != nil {
				t.Fatalf("查询最大可开失败: %v", err)
			}
			rel := func(got, want decimal.Decimal, field string) {
				t.Helper()
				if want.IsZero() {
					return
				}
				if diff := got.Sub(want).Abs(); diff.GreaterThan(want.Mul(dec("0.01"))) {
					t.Errorf("%s = %s，OKX 为 %s，相差 %s（超出 1%%）", field, got, want, diff)
				}
			}
			rel(m.MaxBuy, dec(sm.MaxBuy), "最大可买")
			rel(m.MaxSell, dec(sm.MaxSell), "最大可卖")
		})
	}
}

// TestAvailBalanceAgreesWithBalance 锁定两条可用余额的算法永远一致。
//
// Fill 的资金校验走 availBalance 这条快路径，它省掉了查档与强平价；Balance 走完整
// 那条。两者若哪天分岔，症状会极其难查：一笔成交在校验时算得起、落账后余额却是负的。
func TestAvailBalanceAgreesWithBalance(t *testing.T) {
	fx := loadCrossFixture(t)
	snap := crossSnapshot(t, fx)

	cases := []struct {
		name string
		set  func(t *testing.T, s *Simulator)
	}{
		{"空账户", func(t *testing.T, s *Simulator) {}},
		{"单个逐仓仓位", func(t *testing.T, s *Simulator) {
			putPos(t, s, "ETH-USDT-SWAP", types.MgnIsolated, types.PosLong, "2", "2445", "10", "489")
		}},
		{"单个全仓仓位", func(t *testing.T, s *Simulator) {
			putPos(t, s, "ETH-USDT-SWAP", types.MgnCross, types.PosLong, "2", "2445", "10", "")
		}},
		{"全仓多空并存", func(t *testing.T, s *Simulator) {
			putPos(t, s, "GRASS-USDT-260911", types.MgnCross, types.PosLong, "7000", "0.359", "20", "")
			putPos(t, s, "GRASS-USDT-260911", types.MgnCross, types.PosShort, "7000", "0.359", "20", "")
		}},
		{"逐仓全仓混合", func(t *testing.T, s *Simulator) {
			putPos(t, s, "ETH-USDT-SWAP", types.MgnIsolated, types.PosLong, "2", "2445", "10", "489")
			putPos(t, s, "GRASS-USDT-260911", types.MgnCross, types.PosLong, "7000", "0.359", "20", "")
			putPos(t, s, "GRASS-USDT-260925", types.MgnCross, types.PosShort, "5000", "0.359", "20", "")
		}},
		{"带全仓挂单", func(t *testing.T, s *Simulator) {
			putPos(t, s, "GRASS-USDT-260911", types.MgnCross, types.PosLong, "7000", "0.359", "20", "")
			if _, err := s.PlaceOrder(Order{
				OrdID: "p1", InstID: "GRASS-USDT-260911", TdMode: types.TdCross,
				Side: types.Buy, PosSide: types.PosLong, OrdType: types.OrdLimit,
				Px: dec("0.25"), Sz: dec("5000"),
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"带逐仓挂单", func(t *testing.T, s *Simulator) {
			if _, err := s.PlaceOrder(Order{
				OrdID: "p2", InstID: "ETH-USDT-SWAP", TdMode: types.TdIsolated,
				Side: types.Buy, PosSide: types.PosLong, OrdType: types.OrdLimit,
				Px: dec("2000"), Sz: dec("2"),
			}); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Deposit("USDT", dec("100000")); err != nil {
				t.Fatal(err)
			}
			c.set(t, s)

			fast, err := s.availBalance("USDT")
			if err != nil {
				t.Fatalf("快路径失败: %v", err)
			}
			b, err := s.BalanceOf("USDT")
			if err != nil {
				t.Fatalf("完整路径失败: %v", err)
			}
			if !fast.Equal(b.AvailBal) {
				t.Errorf("availBalance = %s，Balance.AvailBal = %s，两条路径必须一致",
					fast, b.AvailBal)
			}
		})
	}
}

func putPos(t *testing.T, s *Simulator, instID string, mgn types.MgnMode,
	side types.PosSide, sz, avgPx, lever, margin string) {

	t.Helper()
	p := Position{
		InstID: instID, MgnMode: mgn, PosSide: side,
		Pos: dec(sz), AvgPx: dec(avgPx), Lever: dec(lever),
	}
	if margin != "" {
		p.Margin = dec(margin)
	}
	if err := s.SetPosition(p); err != nil {
		t.Fatalf("置入仓位失败: %v", err)
	}
	s.SetMarkPx(instID, dec(avgPx))
}

// TestCrossMgnRatioHasStructuralFloor 锁定一条推得的、但影响很大的性质：
//
//	全仓保证金率的下限 = 1 / (该档最高杠杆 × (维持保证金率 + 吃单费率))
//
// 开仓时权益最少也要等于初始保证金 `名义/最高杠杆`，而保证金率的分母是
// `名义 × 维持保证金率 + 平仓手续费`。两者都正比于名义价值，**比值由档位表锁死**
// ——放多少钱、加多少仓、烧多少手续费都改不了它。
//
// 这条性质有两个实际后果：
//
//	一  全仓账户不可能在没有不利波动的情况下被强平。ETH-USD 二档算出 2.728，
//	    实测起点 2.73~2.76 与之相符
//	二  触发所需的不利波动幅度 = 1/最高杠杆 − (维持保证金率 + 吃单费率)。
//	    一档 100x 只需 0.55%，二档 66.66x 需 0.95% —— 用满杠杆反而离强平更近
//
// 若哪天本库算出低于该下限的保证金率，那一定是分子分母口径错位（例如分母漏了
// 平仓手续费，或初始保证金按标记价而非开仓价算），而不是真的更危险。
func TestCrossMgnRatioHasStructuralFloor(t *testing.T) {
	fx := loadCrossFixture(t)
	snap := crossSnapshot(t, fx)
	const instID = "ETH-USDT-SWAP"
	inst, err := snap.Instrument(instID)
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := refdata.TierTableFor(snap, inst, types.MgnCross)
	if err != nil {
		t.Fatal(err)
	}
	taker := dec(fx.FeeRate.Taker).Abs()

	// 逐档验证：每档用它自己的最高杠杆开满，看保证金率是否恰好落在该档的下限
	for _, tier := range tbl.Tiers {
		name := "第" + decimal.NewFromInt(int64(tier.Tier)).String() + "档"
		t.Run(name, func(t *testing.T) {
			s, err := New(Config{PosMode: types.LongShortMode, RefData: snap})
			if err != nil {
				t.Fatal(err)
			}
			// 张数取该档区间的中点，避免贴着边界受取整影响
			sz := tier.MinSz.Add(tier.MaxSz).Div(decimal.NewFromInt(2))
			sz = inst.RoundSize(sz)
			px := dec("2500")
			nom := notional(inst, sz, px)
			// 恰好按该档最高杠杆备足初始保证金
			imr := div(nom, tier.MaxLever)
			if err := s.Deposit("USDT", imr); err != nil {
				t.Fatal(err)
			}
			if err := s.SetLeverage(instID, types.MgnCross, types.PosLong,
				tier.MaxLever); err != nil {
				t.Fatal(err)
			}
			if err := s.SetPosition(Position{
				InstID: instID, MgnMode: types.MgnCross, PosSide: types.PosLong,
				Pos: sz, AvgPx: px, Lever: tier.MaxLever,
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetMarkPx(instID, px); err != nil {
				t.Fatal(err)
			}

			cm, err := s.CrossMetricsOf("USDT")
			if err != nil {
				t.Fatal(err)
			}
			want := div(decimal.NewFromInt(1), tier.MaxLever.Mul(tier.MMR.Add(taker)))
			near(t, cm.MgnRatio, want, "1e-12",
				name+" 用满杠杆时的保证金率应恰好等于结构性下限")
			if cm.MgnRatio.LessThan(decimal.NewFromInt(1)) {
				t.Errorf("下限 %s 低于 1，意味着开仓即可被强平——档位表或口径有问题",
					cm.MgnRatio)
			}

			// 触发所需的不利波动 = 1/最高杠杆 − (mmr率 + 吃单费率)
			wantMove := div(decimal.NewFromInt(1), tier.MaxLever).Sub(tier.MMR.Add(taker))
			gotMove := div(cm.Equity.Sub(cm.MMR.Add(cm.CloseFee)), nom)
			near(t, gotMove, wantMove, "1e-12", name+" 触发所需的不利波动幅度")
		})
	}
}

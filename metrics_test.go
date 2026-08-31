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

// metricsFixture 是 testdata/conformance/isolated-position-metrics.json 的结构。
type metricsFixture struct {
	Samples []struct {
		InstID     string          `json:"instId"`
		PosSide    string          `json:"posSide"`
		Instrument json.RawMessage `json:"instrument"`
		Position   map[string]any  `json:"position"`
	} `json:"samples"`
}

func loadMetricsFixture(t *testing.T) metricsFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "isolated-position-metrics.json"))
	if err != nil {
		t.Fatalf("读取夹具失败: %v", err)
	}
	var f metricsFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	return f
}

func fieldOf(t *testing.T, m map[string]any, k string) string {
	t.Helper()
	v, ok := m[k].(string)
	if !ok {
		t.Fatalf("夹具缺少字段 %s", k)
	}
	return v
}

// buildFromReal 由真实仓位数据重建 Position 与 Instrument。
func buildFromReal(t *testing.T, rawInst json.RawMessage, p map[string]any) (Position, refdata.Instrument, refdata.PositionTier) {
	t.Helper()

	var inst refdata.Instrument
	if err := json.Unmarshal(rawInst, &inst); err != nil {
		t.Fatalf("解析合约规格失败: %v", err)
	}

	posSide := types.PosSide(fieldOf(t, p, "posSide"))
	pos := Position{
		InstID:  inst.InstID,
		MgnMode: types.MgnIsolated,
		PosSide: posSide,
		Pos:     dec(fieldOf(t, p, "pos")),
		AvgPx:   dec(fieldOf(t, p, "avgPx")),
		Lever:   dec(fieldOf(t, p, "lever")),
		Margin:  dec(fieldOf(t, p, "margin")),
	}

	// 由 mmr 金额反推维持保证金率——夹具里没有档位表，而这正是 OKX 用的那个值。
	markPx := dec(fieldOf(t, p, "markPx"))
	nom := notional(inst, pos.AbsPos(), markPx)
	tier := refdata.PositionTier{Tier: 1}
	if !nom.IsZero() {
		tier.MMR = div(dec(fieldOf(t, p, "mmr")), nom)
	}
	return pos, inst, tier
}

// TestMetricsAgainstRealPositions 用模拟盘上四个真实逐仓仓位逐项校验风险指标。
//
// 四个样本横跨 tickSz 为 0.01 / 0.1 / 1e-7 的三个量级，含多空两个方向，
// 期望值全部来自 OKX 的实际返回而非我的推导。
func TestMetricsAgainstRealPositions(t *testing.T) {
	fx := loadMetricsFixture(t)
	if len(fx.Samples) == 0 {
		t.Fatal("夹具没有样本")
	}

	for _, s := range fx.Samples {
		t.Run(s.InstID+"/"+s.PosSide, func(t *testing.T) {
			pos, inst, tier := buildFromReal(t, s.Instrument, s.Position)
			markPx := dec(fieldOf(t, s.Position, "markPx"))

			m := ComputeMetrics(pos, inst, tier, markPx, dec(takerRate))

			// 相对容差：OKX 与本实现的除法舍入策略未必逐位相同。
			relNear := func(got, want decimal.Decimal, field string) {
				t.Helper()
				scale := want.Abs()
				if scale.LessThan(decimal.NewFromInt(1)) {
					scale = decimal.NewFromInt(1)
				}
				tol := scale.Mul(dec("1e-9"))
				if got.Sub(want).Abs().GreaterThan(tol) {
					t.Errorf("%s = %s, 期望 %s, 差 %s", field, got, want, got.Sub(want))
				}
			}

			relNear(m.UPL, dec(fieldOf(t, s.Position, "upl")), "未实现盈亏")
			relNear(m.UPLRatio, dec(fieldOf(t, s.Position, "uplRatio")), "收益率")
			relNear(m.MMR, dec(fieldOf(t, s.Position, "mmr")), "维持保证金金额")
			relNear(m.MgnRatio, dec(fieldOf(t, s.Position, "mgnRatio")), "保证金率")
			relNear(m.LiqPx, dec(fieldOf(t, s.Position, "liqPx")), "强平价")

			t.Logf("upl=%s mgnRatio=%s liqPx=%s（OKX: %s / %s / %s）",
				m.UPL, m.MgnRatio, m.LiqPx,
				fieldOf(t, s.Position, "upl"),
				fieldOf(t, s.Position, "mgnRatio"),
				fieldOf(t, s.Position, "liqPx"))
		})
	}
}

// TestLiqPxTickBuffer 守卫强平价里那一个 tickSz 的安全缓冲。
//
// 该缓冲在任何文档里都没有写，是在真实仓位上标定出来的。少了它，强平价会
// 系统性地偏乐观一个 tick——数值虽小，却会在对拍时表现为一个甩不掉的偏差。
// 这条测试直接比较「含缓冲」与「不含缓冲」两种算法与真实值的距离。
func TestLiqPxTickBuffer(t *testing.T) {
	fx := loadMetricsFixture(t)

	for _, s := range fx.Samples {
		pos, inst, tier := buildFromReal(t, s.Instrument, s.Position)
		markPx := dec(fieldOf(t, s.Position, "markPx"))
		real := dec(fieldOf(t, s.Position, "liqPx"))

		withBuf := ComputeMetrics(pos, inst, tier, markPx, dec(takerRate)).LiqPx

		// 去掉缓冲：多头减回一个 tick、空头加回一个 tick
		noBuf := withBuf.Sub(inst.TickSz)
		if pos.IsShort() {
			noBuf = withBuf.Add(inst.TickSz)
		}

		dWith := withBuf.Sub(real).Abs()
		dNo := noBuf.Sub(real).Abs()
		if !dWith.LessThan(dNo) {
			t.Errorf("%s %s: 含缓冲的强平价距真实值 %s，不含缓冲距 %s —— 缓冲未起作用",
				s.InstID, s.PosSide, dWith, dNo)
		}
		ratio := real.Sub(noBuf)
		if !inst.TickSz.IsZero() {
			t.Logf("%s %s tickSz=%s 偏移/tickSz=%s",
				s.InstID, s.PosSide, inst.TickSz, div(ratio, inst.TickSz).Round(6))
		}
	}
}

// TestUPLRatioUsesOpenInitialMargin 收益率的分母是开仓时的初始保证金，
// 不是当前 margin。
//
// 两者在开仓瞬间相等，逐仓的资金费从保证金里扣，持仓久了才分离——
// 因此只有那个持有七周的样本能区分这两种算法。
func TestUPLRatioUsesOpenInitialMargin(t *testing.T) {
	fx := loadFixture(t, "close-long-isolated-linear.json")

	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatalf("解析合约规格失败: %v", err)
	}
	p := fx.PositionBefore
	str := func(k string) string { return fieldOf(t, p, k) }

	pos := Position{
		InstID: inst.InstID, MgnMode: types.MgnIsolated, PosSide: types.PosLong,
		Pos: dec(str("pos")), AvgPx: dec(str("avgPx")),
		Lever: dec(str("lever")), Margin: dec(str("margin")),
	}
	markPx := dec(str("markPx"))
	nom := notional(inst, pos.AbsPos(), markPx)
	tier := refdata.PositionTier{Tier: 1, MMR: div(dec(str("mmr")), nom)}

	m := ComputeMetrics(pos, inst, tier, markPx, dec(takerRate))
	real := dec(str("uplRatio"))

	near(t, m.UPLRatio, real, "1e-9", "收益率")

	// 若误用当前 margin 作分母，会得到一个明显不同的数
	wrong := div(m.UPL, pos.Margin)
	if wrong.Sub(real).Abs().LessThan(dec("1e-6")) {
		t.Fatal("该样本无法区分两种算法，守卫失效")
	}
	t.Logf("用开仓初始保证金 %s（OKX %s）；误用当前 margin 会得到 %s",
		m.UPLRatio, real, wrong)
}

// TestMMRIsAmountNotRate 守卫字段语义陷阱：position.mmr 是维持保证金的金额，
// 不是比率。名字极具误导性，混淆会让保证金率差出几个数量级。
func TestMMRIsAmountNotRate(t *testing.T) {
	fx := loadMetricsFixture(t)
	s := fx.Samples[0]

	pos, inst, tier := buildFromReal(t, s.Instrument, s.Position)
	markPx := dec(fieldOf(t, s.Position, "markPx"))
	m := ComputeMetrics(pos, inst, tier, markPx, dec(takerRate))

	if m.MMR.LessThan(decimal.NewFromInt(1)) {
		t.Errorf("维持保证金金额 = %s，看起来像比率而非金额", m.MMR)
	}
	if m.MMRRate.GreaterThan(decimal.NewFromInt(1)) {
		t.Errorf("维持保证金率 = %s，看起来像金额而非比率", m.MMRRate)
	}
	if !m.MMR.Equal(m.Notional.Mul(m.MMRRate)) {
		t.Errorf("维持保证金金额 %s 不等于 名义价值 %s × 比率 %s",
			m.MMR, m.Notional, m.MMRRate)
	}
}

func TestMetricsEmptyPosition(t *testing.T) {
	m := ComputeMetrics(emptyNetPos(), btcSwap(t), refdata.PositionTier{Tier: 1},
		dec("70000"), dec(takerRate))

	if !m.UPL.IsZero() || !m.MgnRatio.IsZero() || !m.LiqPx.IsZero() {
		t.Errorf("空仓的指标应为零值: %+v", m)
	}
	if m.IsLiquidatable() {
		t.Error("空仓不应被判定为可强平")
	}
}

// TestIsLiquidatable 保证金率以倍数表示，1 即 100%，≤ 1 触发强平。
func TestIsLiquidatable(t *testing.T) {
	cases := []struct {
		ratio string
		want  bool
	}{
		{"89.02609934784486", false}, // 实测的健康仓位
		{"1.5", false},
		{"1.0000001", false},
		{"1", true},
		{"0.8", true},
		{"0", true}, // 权益已被耗尽，最该强平
	}
	for _, c := range cases {
		m := Metrics{MgnRatio: dec(c.ratio), HasPosition: true}
		if got := m.IsLiquidatable(); got != c.want {
			t.Errorf("保证金率 %s 判定为可强平=%v，期望 %v", c.ratio, got, c.want)
		}
	}
}

// TestIsLiquidatableNeedsPosition 保证金率为零有两种截然不同的含义，
// 必须靠 HasPosition 区分，不能只看数值。
//
// 「没有仓位」与「权益已被亏损或资金费耗尽」的保证金率都是零，而后者恰恰最该
// 强平。早先只用 MgnRatio 非零来排除空仓，结果是被资金费耗穿的仓位反而爆不掉。
func TestIsLiquidatableNeedsPosition(t *testing.T) {
	// 无仓位：保证金率为零也不该判为可强平
	if (Metrics{}).IsLiquidatable() {
		t.Error("空仓不应被判定为可强平")
	}
	if (Metrics{MgnRatio: dec("0")}).IsLiquidatable() {
		t.Error("无仓位时保证金率为零不应被判定为可强平")
	}
	// 有仓位且权益耗尽：必须判为可强平
	if !(Metrics{HasPosition: true, MgnRatio: dec("0")}).IsLiquidatable() {
		t.Error("有仓位且权益耗尽时必须判定为可强平")
	}
}

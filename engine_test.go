package okxsim

import (
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// 本文件里的四条来自一个真实使用者——一个网格回测引擎在接入本库时提出的需求。
// 放在一起是因为它们都是「对外形态」的问题，而不是某一条规则算得对不对。

// TestFillResultCarriesFill 锁定成交结果带着成交本身。
//
// 没有它，引擎从 StepResult.Fills 里认不出是【哪一笔挂单】在【什么价】成交的，
// 只能在每次 Advance 前后各拍一次 PendingOrders 快照做差集——网格这类常驻几十笔
// 挂单的策略，那是热路径上一笔白花的 O(n)。
//
// 带出来的必须是**规范化之后**的成交：ExecType 已补默认值、PosSide 已由空值
// 解析成实际方向。给原样的入参就等于把规范化的活又推回给调用方。
func TestFillResultCarriesFill(t *testing.T) {
	s := newSim(t, types.LongShortMode)
	if err := s.SetLeverage("BTC-USDT-SWAP", types.MgnIsolated, types.PosLong,
		dec("10")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlaceOrder(Order{
		OrdID: "grid-7", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosLong, OrdType: types.OrdLimit,
		Px: dec("77000"), Sz: dec("1"), Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: dec("77500"), High: dec("78000"),
		Low: dec("76900"), MarkPx: dec("77500"), Ts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fills) != 1 {
		t.Fatalf("应当成交一笔，实为 %d 笔", len(res.Fills))
	}
	f := res.Fills[0].Fill
	if f.OrdID != "grid-7" {
		t.Errorf("委托 ID = %q，期望 grid-7——认不出是哪一格成交就补挂不了", f.OrdID)
	}
	eq(t, f.Px, "77000", "成交价即委托价")
	eq(t, f.Sz, "1", "成交张数")
	if f.Side != types.Buy || f.PosSide != types.PosLong {
		t.Errorf("方向 = %s/%s，期望 buy/long", f.Side, f.PosSide)
	}
	if f.ExecType != types.Maker {
		t.Errorf("成交角色 = %q，挂单被触及应为 maker", f.ExecType)
	}
	if f.Ts != 2 {
		t.Errorf("成交时刻 = %d，期望 2", f.Ts)
	}

	// 买卖模式下 PosSide 入参为空，结果里必须已经解析成 net
	s2 := newSim(t, types.NetMode)
	fr, err := s2.Fill(Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Buy,
		Sz: dec("1"), Px: dec("78000"), Ts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fr.Fill.PosSide != types.PosNet {
		t.Errorf("PosSide = %q，买卖模式下应已解析为 net", fr.Fill.PosSide)
	}
	if fr.Fill.ExecType != types.Taker {
		t.Errorf("ExecType = %q，未指定时应已补上默认值", fr.Fill.ExecType)
	}
}

// TestLeanLiquidationCheckAgreesWithMetrics 这次性能改动唯一真正的风险点。
//
// 强平判据改走精简路径后，它与 Metrics.IsLiquidatable 是两段代码。两者一旦分岔，
// 表现是「保证金率显示没事却被强平了」或反过来——而且不会有任何报错，回测照跑。
//
// 所以扫过强平边界两侧逐点比对：从远高于强平价一路扫到远低于，任何一点不一致
// 都说明两段算法已经分家。
func TestLeanLiquidationCheckAgreesWithMetrics(t *testing.T) {
	for _, tc := range []struct {
		name string
		side types.Side
	}{{"多头", types.Buy}, {"空头", types.Sell}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t, types.NetMode)
			mustFill(t, s, netFill(tc.side, "2", "78000"))
			pos, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)

			// 由强平价出发，向两侧各扫一段，务必跨过边界
			m0, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
			if err != nil {
				t.Fatal(err)
			}
			if !m0.LiqPx.IsPositive() {
				t.Fatal("拿不到强平价，测不了")
			}
			var checked, flipped int
			for i := -60; i <= 60; i++ {
				px := m0.LiqPx.Add(m0.LiqPx.Mul(dec("0.0005")).
					Mul(decimal.NewFromInt(int64(i))))
				if !px.IsPositive() {
					continue
				}
				if err := s.SetMarkPx("BTC-USDT-SWAP", px); err != nil {
					t.Fatal(err)
				}
				lean, err := s.isolatedIsLiquidatable(pos, "BTC-USDT-SWAP")
				if err != nil {
					t.Fatal(err)
				}
				m, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
				if err != nil {
					t.Fatal(err)
				}
				if lean != m.IsLiquidatable() {
					t.Fatalf("标记价 %s：精简判据=%v，完整指标=%v（保证金率 %s）——"+
						"两段算法已经分家", px, lean, m.IsLiquidatable(), m.MgnRatio)
				}
				if lean {
					flipped++
				}
				checked++
			}
			if checked < 100 {
				t.Fatalf("只扫了 %d 个点，覆盖不够", checked)
			}
			if flipped == 0 || flipped == checked {
				t.Fatalf("扫过的 %d 个点全部同一结论（触发 %d 个），"+
					"根本没跨过强平边界，这条测试等于没测", checked, flipped)
			}
		})
	}
}

// TestRequireMarkPxRefusesToDegrade 开启后，缺标记价即报错而不是悄悄降级。
//
// 退化的后果很具体：强平判据本该看标记价，用最新成交价会让插针扫掉本不该爆的
// 仓位。对尾部风险就是强平的策略，这是**假阴性**——参数组合被淘汰，而扫描结果里
// 不留任何痕迹。
func TestRequireMarkPxRefusesToDegrade(t *testing.T) {
	mk := func(require bool) *Simulator {
		t.Helper()
		s, err := New(Config{
			PosMode: types.NetMode, RefData: refdata.MustEmbedded(),
			DefaultLever: decimal.NewFromInt(5), RequireMarkPx: require,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Deposit("USDT", dec("10000")); err != nil {
			t.Fatal(err)
		}
		return s
	}
	noMark := Bar{InstID: "BTC-USDT-SWAP", Last: dec("78000"), Ts: 1}

	// 默认行为不变：不给标记价照跑，用最新价顶替
	if _, err := mk(false).Advance(noMark); err != nil {
		t.Errorf("默认不该因缺标记价失败: %v", err)
	}

	s := mk(true)
	if _, e := s.Advance(noMark); e == nil {
		t.Fatal("开启 RequireMarkPx 后，缺标记价应当报错")
	} else if !okxerr.HasCode(e, okxerr.CodeParamEmpty) {
		t.Errorf("错误码 = %v，期望 %s", e, okxerr.CodeParamEmpty)
	}

	// 给了就照常跑
	if _, err := s.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: dec("78000"), MarkPx: dec("78010"), Ts: 2,
	}); err != nil {
		t.Errorf("给了标记价不该失败: %v", err)
	}
	eq(t, s.MarkPx("BTC-USDT-SWAP"), "78010", "标记价应当用 Bar 给的那个")
}

// TestCheckLiquidationCoversBothLevels 导出的强平检查必须两级都查。
//
// 本库明确支持「引擎自行撮合、手工灌 Fill」这条路径，而那条路径不经过 Advance。
// 不显式检查一次，仓位就永远不会爆仓——回测里表现为本该归零的策略一路活到最后。
//
// 坑在于全仓的强平是**币种级**的：只查合约那一级会静默漏掉全仓仓位，
// 那比不导出更糟——使用者以为查过了。
func TestCheckLiquidationCoversBothLevels(t *testing.T) {
	// 逐仓：手工灌成交、不走 Advance，显式检查后应当爆仓
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))
	m, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMarkPx("BTC-USDT-SWAP", m.LiqPx.Mul(dec("0.99"))); err != nil {
		t.Fatal(err)
	}
	liqs, err := s.CheckLiquidation("BTC-USDT-SWAP", 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(liqs) == 0 {
		t.Fatal("逐仓仓位已跌破强平价，显式检查应当爆仓")
	}
	if p, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); ok && !p.IsEmpty() {
		t.Errorf("强平后仓位应当清空，实为 %s 张", p.Pos)
	}

	// 全仓：币种级的强平，只查合约那一级会漏掉
	//
	// 仓位要开到接近满仓才有意义：全仓的兜底是整个币种的权益，小仓位算出来的
	// 强平价会落到负数区（够不着），那不是缺陷而是事实。
	s2 := newSim(t, types.NetMode)
	if _, err := s2.Fill(Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdCross, Side: types.Buy,
		Sz: dec("60"), Px: dec("78000"), ExecType: types.Taker, Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	cm, err := s2.CrossMetricsOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	if !cm.LiqPx.IsPositive() {
		t.Fatalf("拿不到全仓强平价，测不了（权益 %s，维持保证金 %s）",
			cm.Equity, cm.MMR)
	}
	if err := s2.SetMarkPx("BTC-USDT-SWAP", cm.LiqPx.Mul(dec("0.99"))); err != nil {
		t.Fatal(err)
	}
	liqs2, err := s2.CheckLiquidation("BTC-USDT-SWAP", 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(liqs2) == 0 {
		t.Fatal("全仓已跌破币种级强平线，导出的检查应当查到——" +
			"只查合约那一级就会漏在这里")
	}
	for _, l := range liqs2 {
		if l.MgnMode != types.MgnCross {
			t.Errorf("应当是全仓强平，实为 %s", l.MgnMode)
		}
	}
}

// TestMarginExponentStaysBounded 钉住小数指数不随部分平仓增长。
//
// 这条缺陷不会报错，也不影响结果的正确性——它只是让回测**越跑越慢**。
// 根因是 div 固定给出 -20 的指数，而 Mul 把两边指数相加：写成
// base.Mul(div(closedSz, beforeAbs)) 的话，Position.Margin 每经历一次部分平仓
// 就多 20 位小数，无界增长，再经 Sub/Add 永久污染现金余额。
//
// 实测（网格引擎那边报的，本仓已复现）：12 轮部分平仓后系数已 264 位；
// 挂单数 16/80/160 时，5 倍挂单换来 22.8 倍耗时，pprof 指认 decimal.rescale
// -> big.Int.Exp 占约 40% CPU。
//
// 网格做的全是部分平仓，正中靶心；任何做部分平仓的策略都会中招。
func TestMarginExponentStaysBounded(t *testing.T) {
	const rounds = 40
	// 20 是 div 的精度，指数不该比它更负；留一点余量给成交价自带的小数位
	const floor = -32

	for _, mode := range []types.TdMode{types.TdIsolated, types.TdCross} {
		t.Run(string(mode), func(t *testing.T) {
			s := newSim(t, types.NetMode)
			if err := s.Deposit("USDT", dec("5000000")); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < rounds; i++ {
				// 张数取不整除的比例，逼出真实的除法余数
				if _, err := s.Fill(Fill{
					InstID: "BTC-USDT-SWAP", TdMode: mode, Side: types.Buy,
					PosSide: types.PosNet, Sz: dec("3"), Px: dec("78000"),
					ExecType: types.Taker, Ts: int64(i * 2),
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Fill(Fill{
					InstID: "BTC-USDT-SWAP", TdMode: mode, Side: types.Sell,
					PosSide: types.PosNet, Sz: dec("1"), Px: dec("78100"),
					ExecType: types.Taker, Ts: int64(i*2 + 1),
				}); err != nil {
					t.Fatal(err)
				}
			}
			p, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
			cash := s.CashBal("USDT")

			for _, c := range []struct {
				name string
				v    decimal.Decimal
			}{{"仓位保证金", p.Margin}, {"现金余额", cash}} {
				if e := c.v.Exponent(); e < floor {
					t.Errorf("%s 的小数指数 = %d（系数 %d 位），已跌破 %d——"+
						"说明又有人写成了先除后乘，回测会越跑越慢",
						c.name, e, len(c.v.Coefficient().String()), floor)
				}
			}
		})
	}
}

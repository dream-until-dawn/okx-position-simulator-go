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

// TestMarkPxRequiredByDefault 缺标记价默认即报错，不再悄悄降级。
//
// v1.0 起翻了默认值。字段是反过来写的（AllowMarkPxFallback）因为 Go 的零值是
// false，而我们要的默认是「必须给标记价」——只能让字段表达【选择退出】。
//
// 退化的后果很具体：强平判据本该看标记价，用最新成交价会让插针扫掉本不该爆的
// 仓位。对尾部风险就是强平的策略，这是**假阴性**——参数组合被淘汰，而扫描结果里
// 不留任何痕迹。这类错误不该靠人记得去开一个开关才能避免。
func TestMarkPxRequiredByDefault(t *testing.T) {
	mk := func(allowFallback bool) *Simulator {
		t.Helper()
		s, err := New(Config{
			PosMode: types.NetMode, RefData: refdata.MustEmbedded(),
			DefaultLever: decimal.NewFromInt(5), AllowMarkPxFallback: allowFallback,
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

	// 默认（零值配置）拒绝降级
	if _, e := mk(false).Advance(noMark); e == nil {
		t.Fatal("默认就该拒绝缺标记价的 Bar")
	} else if !okxerr.HasCode(e, okxerr.CodeParamEmpty) {
		t.Errorf("错误码 = %v，期望 %s", e, okxerr.CodeParamEmpty)
	}

	// 显式选择退出后才回退到最新价
	s := mk(true)
	if _, err := s.Advance(noMark); err != nil {
		t.Errorf("显式允许回退后不该失败: %v", err)
	}
	eq(t, s.MarkPx("BTC-USDT-SWAP"), "78000", "回退时用最新成交价顶替")

	// 给了标记价，两种配置都照常跑，且用的是给的那个
	for _, allow := range []bool{false, true} {
		s := mk(allow)
		if _, err := s.Advance(Bar{
			InstID: "BTC-USDT-SWAP", Last: dec("78000"), MarkPx: dec("78010"), Ts: 2,
		}); err != nil {
			t.Errorf("给了标记价不该失败(allow=%v): %v", allow, err)
		}
		eq(t, s.MarkPx("BTC-USDT-SWAP"), "78010", "标记价应当用 Bar 给的那个")
	}
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

// TestCashExponentNeverPoisoned 钉住比上一条更隐蔽的那一半。
//
// 净持仓归零时 simulator.go:441 会删掉仓位，Margin 那条链随之重置——**于是在
// 均值回复的策略里，Margin 的指数看着永远正常**。真正记账的是现金：它的指数是
// 【历史最深值的水位线】，取的是曾经加进来的最小指数，**永不回落**。
//
// 后果比「越跑越慢」更难查：一段长的不打平行情把水位线压到很深，此后哪怕策略
// 规规矩矩地每轮打平，整场回测都在那个被污染的精度上跑完。实测旧写法下
// 30 次不打平把现金压到指数 -640、系数 648 位，随后 20 轮干净往返，一位都没退回来。
//
// 网格引擎那边最初把它描述成「现金单调累积」，实际机制是水位线不回落——差别在于
// 损害由【最差的那一段】决定，而不是由平均形态决定。
func TestCashExponentNeverPoisoned(t *testing.T) {
	s := newSim(t, types.NetMode)
	if err := s.Deposit("USDT", dec("50000000")); err != nil {
		t.Fatal(err)
	}
	fill := func(side types.Side, sz, px string, ts int64) {
		t.Helper()
		f := netFill(side, sz, px)
		f.Ts = ts
		mustFill(t, s, f)
	}

	// 一段长的不打平行情，把水位线压下去
	for i := 0; i < 30; i++ {
		fill(types.Buy, "3", "78000", int64(i*2))
		fill(types.Sell, "1", "78050", int64(i*2+1))
	}
	p, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	fill(types.Sell, p.Pos.String(), "78100", 100)

	if q, ok := s.PositionOf("BTC-USDT-SWAP", types.PosNet); ok && !q.IsEmpty() {
		t.Fatalf("这一步该把仓位打平，实为 %s 张", q.Pos)
	}
	// 打平后 Margin 归零，看着一切正常——所以只断言 Margin 是不够的
	after := s.CashBal("USDT")
	if e := after.Exponent(); e < -32 {
		t.Errorf("现金的小数指数 = %d（系数 %d 位）——一段不打平行情把水位线压深了，"+
			"而它永不回落，整场回测都会在这个精度上跑完",
			e, len(after.Coefficient().String()))
	}

	// 此后干净往返也救不回来，所以水位线必须从一开始就不许被压深
	for i := 0; i < 20; i++ {
		fill(types.Buy, "2", "78000", int64(200+i*2))
		fill(types.Sell, "2", "78100", int64(200+i*2+1))
	}
	if e := s.CashBal("USDT").Exponent(); e < -32 {
		t.Errorf("干净往返之后现金指数仍为 %d——水位线确实不回落，"+
			"这条断言本身没问题，问题在前面就该拦住", e)
	}
}

// TestMarkPxFallbackLiquidatesOnAWick 把「回退的代价」变成一个看得见的用例。
//
// 本仓一路在声称「用最新成交价做强平判据会让插针扫掉本不该爆的仓位」，却一直
// 没有测过它。这条补上：同一根 K 线，只差给不给标记价——
//
//	给了标记价（标记价平稳）      不强平
//	不给、退回用最新价（插针）    **强平**
//
// 这就是 v1.0 把默认翻成「必须给标记价」所防的那件事。对尾部风险就是强平的策略
// （如做多网格），它表现为假阴性：参数组合被淘汰，而扫描结果里不留任何痕迹。
//
// 顺带按 okx-tickflow-go 的提醒验数据流而不只是控制流：回退之后 MetricsOf 报出的
// 未实现盈亏与保证金率也跟着用了成交价——断言「打开开关不报错」只覆盖控制流，
// 覆盖不到这里。
func TestMarkPxFallbackLiquidatesOnAWick(t *testing.T) {
	build := func(t *testing.T, allowFallback bool) *Simulator {
		t.Helper()
		s, err := New(Config{
			PosMode: types.NetMode, RefData: refdata.MustEmbedded(),
			DefaultLever: decimal.NewFromInt(5), AllowMarkPxFallback: allowFallback,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Deposit("USDT", dec("10000")); err != nil {
			t.Fatal(err)
		}
		mustFill(t, s, netFill(types.Buy, "4", "78000"))
		return s
	}

	// 先取强平价，据此造一根「最新价插针跌破、标记价没跌破」的 K 线
	probe := build(t, false)
	if err := probe.SetMarkPx("BTC-USDT-SWAP", dec("78000")); err != nil {
		t.Fatal(err)
	}
	m, err := probe.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	liq := m.LiqPx
	if !liq.IsPositive() {
		t.Fatal("拿不到强平价，测不了")
	}
	wick := liq.Mul(dec("0.995")) // 最新价插到强平价之下
	calm := liq.Mul(dec("1.02"))  // 标记价仍在安全区

	// 一、给了标记价：不该强平
	withMark := build(t, false)
	res, err := withMark.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: wick, High: calm, Low: wick,
		MarkPx: calm, Ts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Liquidations) != 0 {
		t.Fatalf("标记价没跌破，不该强平，实为 %d 笔", len(res.Liquidations))
	}
	p, _ := withMark.PositionOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, p.Pos, "4", "仓位应当完好")

	// 二、不给标记价、显式允许回退：同一根 K 线把仓位扫掉了
	fallback := build(t, true)
	res2, err := fallback.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: wick, High: calm, Low: wick, Ts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Liquidations) == 0 {
		t.Fatal("退回用最新价之后，这根插针应当把仓位扫掉——" +
			"若这里不再成立，说明回退的代价变了，本仓一路声称的理由要重新检查")
	}
	if q, ok := fallback.PositionOf("BTC-USDT-SWAP", types.PosNet); ok && !q.IsEmpty() {
		t.Errorf("被强平后仓位应当清空，实剩 %s 张", q.Pos)
	}

	// 三、数据流：回退之后报出的风险指标也是按成交价算的
	quiet := build(t, true)
	if _, err := quiet.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: dec("79000"), High: dec("79000"),
		Low: dec("79000"), Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	eq(t, quiet.MarkPx("BTC-USDT-SWAP"), "79000", "回退把最新价写进了标记价")
	qm, err := quiet.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	// 与「标记价另有其值」的同一时刻对照，未实现盈亏应当不同
	other := build(t, false)
	if _, err := other.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: dec("79000"), High: dec("79000"),
		Low: dec("79000"), MarkPx: dec("78500"), Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	om, err := other.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	if qm.UPL.Equal(om.UPL) || qm.MgnRatio.Equal(om.MgnRatio) {
		t.Errorf("回退与给定标记价应当给出不同的风险指标，"+
			"实为 UPL %s vs %s、保证金率 %s vs %s——若相同，说明标记价没被真正用上",
			qm.UPL, om.UPL, qm.MgnRatio, om.MgnRatio)
	}
}

// TestZeroValuesAreNotSafeDefaults 钉住四处「零值不是安全默认」的语义。
//
// 这一类静默发生在**交界处**：本库工作完全正常、该报的都报了，是调用方没读那个
// 字段，于是一个错误的结论被产出且毫无动静。查起来最难——本库这侧测试全绿，
// 下游那侧结果看起来也正常。
//
// 对应 okx-tickflow-go 那边的 NaN 陷阱（与 NaN 比较永远是 false，一个分支从此
// 再没进去过）。本库不用 NaN，但**零值在阈值比较里扮演了同一个角色**，
// 而且两个方向都有：
//
//	LiqPx = 0     多头永远「还没跌到」，告警永不触发
//	MgnRatio = 0  永远「低于任何阈值」，告警永远在响
//
// 所以「零值安不安全」没有统一答案，只能逐个字段看语义。见 docs/silent-risks.md。
func TestZeroValuesAreNotSafeDefaults(t *testing.T) {
	// 一、没有仓位时 MgnRatio 为零，必须靠 HasPosition 分辨
	s := newSim(t, types.NetMode)
	m, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	if m.HasPosition {
		t.Fatal("空仓时不该报有仓位")
	}
	eq(t, m.MgnRatio, "0", "空仓时保证金率为零")
	if m.MgnRatio.LessThan(dec("1.5")) && !m.HasPosition {
		// 这正是陷阱：一个只看比率的告警会在这里响
		t.Logf("空仓时 MgnRatio=%s 低于任何阈值——只看比率的告警会误报，"+
			"必须先判 HasPosition", m.MgnRatio)
	}

	// 二、强平价够不着时留零，只看数值的告警永不触发
	cm, err := s.CrossMetricsOf("USDT")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, cm.LiqPx, "0", "没有全仓仓位时强平价无定义，留零")
	if cm.HasPosition {
		t.Error("没有全仓仓位时 HasPosition 应为假")
	}

	// 三、强平会撤单，被撤的委托必须出现在结果里——只读 Fills 的策略会丢掉整个簿
	s2 := newSim(t, types.NetMode)
	mustFill(t, s2, netFill(types.Buy, "4", "78000"))
	if _, err := s2.PlaceOrder(Order{
		OrdID: "grid-1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet, OrdType: types.OrdLimit,
		Px: dec("60000"), Sz: dec("1"), Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	m2, err := s2.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.SetMarkPx("BTC-USDT-SWAP", m2.LiqPx.Mul(dec("0.99"))); err != nil {
		t.Fatal(err)
	}
	liqs, err := s2.CheckLiquidation("BTC-USDT-SWAP", 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(liqs) == 0 {
		t.Fatal("应当强平")
	}
	var canceled int
	for _, l := range liqs {
		canceled += len(l.CanceledOrders)
	}
	if canceled == 0 {
		t.Error("强平前撤的单必须报出来——只读 Fills 的策略会丢掉整个挂单簿而不自知")
	}

	// 四、缺指数价时，index 型委托生成一条带 Reason 的 AlgoTrigger，且没有 OrdID
	s3 := newSim(t, types.NetMode)
	mustFill(t, s3, netFill(types.Buy, "1", "78000"))
	if _, err := s3.PlaceAlgoOrder(AlgoOrder{
		AlgoID: "idx", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosNet, OrdType: types.AlgoTrigger,
		TriggerPx: dec("79000"), TriggerPxType: types.TriggerIndex,
		Sz: dec("1"), Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// 不给 IdxPx
	res, err := s3.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: dec("80000"), High: dec("80000"),
		Low: dec("80000"), MarkPx: dec("80000"), Ts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AlgoTriggers) != 1 {
		t.Fatalf("缺指数价时应当报出一条说明，实为 %d 条", len(res.AlgoTriggers))
	}
	at := res.AlgoTriggers[0]
	if at.Reason == "" {
		t.Error("跳过的原因必须写在 Reason 里")
	}
	if at.OrdID != "" {
		t.Errorf("没触发就不该有委托 ID，实为 %q——"+
			"遍历 AlgoTriggers 而不看 Reason 的会把没触发读成触发", at.OrdID)
	}
	if len(res.Fills) != 0 {
		t.Errorf("没触发就不该有成交，实为 %d 笔", len(res.Fills))
	}
}

// TestAffectsAccountStateBothDirections 两个方向都测。
//
// 只测一个方向等于没测——「把强平当成无害的」和「把只挂单被拒当成账户级的」
// 是两个都真实、且后果不同的错：前者让策略在不存在的前提上继续跑且不报错，
// 后者让网格在**第一根 K 线上**就停机。
//
// 白名单方向也一并钉住：一个未列举的取值必须落在「账户级」那一侧——这条保证
// 本库日后新增撤单原因时，调用方不改代码也不会漏。
func TestAffectsAccountStateBothDirections(t *testing.T) {
	for _, r := range []CancelReason{
		CancelUser, CancelPostOnlyWouldTake, CancelImmediateUnfilled,
		CancelInsufficientFunds,
	} {
		if r.AffectsAccountState() {
			t.Errorf("%s 只影响那一笔委托，不该判为账户级——"+
				"这么判会让网格在第一次挂单被拒时就停机，而那是必然发生的事", r)
		}
	}
	if !CancelLiquidation.AffectsAccountState() {
		t.Error("强平会撤光挂单并拿走仓位，必须判为账户级")
	}
	// 未列举的取值（含日后新增的）必须落在安全那一侧
	for _, r := range []CancelReason{"", "adl", "unknown", "某个还没有的原因"} {
		if !r.AffectsAccountState() {
			t.Errorf("未列举的取值 %q 必须当作账户级——"+
				"白名单的方向就是为了让新值落进安全那一侧", r)
		}
	}

	// 真实链路：一次强平里，被撤的单必须带上判为账户级的原因
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "78000"))
	if _, err := s.PlaceOrder(Order{
		OrdID: "g1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet, OrdType: types.OrdLimit,
		Px: dec("60000"), Sz: dec("1"), Ts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	m, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Advance(Bar{
		InstID: "BTC-USDT-SWAP", Last: m.LiqPx.Mul(dec("0.98")),
		High: dec("78000"), Low: m.LiqPx.Mul(dec("0.98")),
		MarkPx: m.LiqPx.Mul(dec("0.98")), Ts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Liquidations) == 0 {
		t.Fatal("应当强平")
	}
	var sawAccountLevel bool
	for _, c := range res.Canceled {
		if c.Reason.AffectsAccountState() {
			sawAccountLevel = true
		}
	}
	if !sawAccountLevel {
		t.Error("强平清场撤的单必须被判为账户级——" +
			"这正是下游漏接、在空簿上按旧状态继续跑的那条路径")
	}
}

// TestZeroConfigDefaultsToIsolatedHedge 钉住「什么都不设」时的默认组合。
//
//	PosMode 留空  ->  开平仓模式（long_short_mode），与 OKX 账户一致
//	TdMode  留空  ->  逐仓
//
// 这是回测引擎最常用的组合。两处的性质不同，值得分开记：
//
//	PosMode  v1.1.0 起由买卖模式改为开平仓模式，**是破坏性变更**——但破得响亮：
//	         开平仓模式要求每笔成交显式给 long 或 short，留空当场报错，
//	         此前依赖默认值的调用方会立刻看到错误而不是拿到一批口径不同的结果
//	TdMode   **纯增量**：此前留空直接报错，所以没有任何既有行为因此改变
func TestZeroConfigDefaultsToIsolatedHedge(t *testing.T) {
	s, err := New(Config{RefData: refdata.MustEmbedded()})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.PosMode(); got != types.LongShortMode {
		t.Fatalf("零配置的持仓方式 = %q，期望开平仓模式", got)
	}
	if err := s.Deposit("USDT", dec("100000")); err != nil {
		t.Fatal(err)
	}

	// TdMode 留空即逐仓
	fr, err := s.Fill(Fill{
		InstID: "BTC-USDT-SWAP", Side: types.Buy, PosSide: types.PosLong,
		Sz: dec("1"), Px: dec("78000"), ExecType: types.Taker, Ts: 1,
	})
	if err != nil {
		t.Fatalf("留空 TdMode 应当按逐仓处理: %v", err)
	}
	if fr.Fill.TdMode != types.TdIsolated {
		t.Errorf("规范化后的 TdMode = %q，期望逐仓", fr.Fill.TdMode)
	}
	p, ok := s.PositionOf("BTC-USDT-SWAP", types.PosLong)
	if !ok {
		t.Fatal("应当建仓")
	}
	if p.MgnMode != types.MgnIsolated {
		t.Errorf("仓位的保证金模式 = %q，期望逐仓", p.MgnMode)
	}
	if !p.Margin.IsPositive() {
		t.Error("逐仓仓位应当划入保证金")
	}

	// PosSide 留空在开平仓模式下必须报错——这就是「破得响亮」那一句的凭据
	if _, err := s.Fill(Fill{
		InstID: "BTC-USDT-SWAP", Side: types.Buy, Sz: dec("1"),
		Px: dec("78000"), ExecType: types.Taker, Ts: 2,
	}); !okxerr.HasCode(err, okxerr.CodeParamError) {
		t.Errorf("开平仓模式下留空 PosSide 应当报错，实为 %v——"+
			"若这里不报错，翻默认值就成了静默的行为变更", err)
	}
	// 挂单那侧同规则
	if _, err := s.PlaceOrder(Order{
		OrdID: "o1", InstID: "BTC-USDT-SWAP", Side: types.Buy,
		PosSide: types.PosLong, OrdType: types.OrdLimit,
		Px: dec("60000"), Sz: dec("1"), Ts: 3,
	}); err != nil {
		t.Errorf("挂单留空 TdMode 也应当按逐仓处理: %v", err)
	}
}

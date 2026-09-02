package okxsim

import (
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/types"
)

// 本文件守住一条规则：**开平仓模式下，平仓量不得超过持仓量。**
//
// 缺了它会出现一种很坏的失败——想平仓，仓位反而变大了，方向还没变，且全程
// 不报错。成因是 signedToOKX 在开平仓模式下取绝对值：溢出部分走进反手分支后
// 符号被抹平，`4 张多头 - 10 张` 变成了 `6 张多头`。
//
// 实测依据：
//
//	裁剪   okx-rules.md §8：拿 1.50 张去平一个 0.15 张的仓位，
//	       OKX 把 sz 裁到 0.15 后成交，订单记录里的 sz 就是 0.15
//	撤单   2026-09-02 模拟盘：持有 137.89 张全仓空头并挂着四笔 buy/short，
//	       第一笔成交把空头平光之后，其余三笔全部被系统撤销，cancelSource=22

// TestOverCloseIsTrimmedInLongShortMode 超量平仓被裁剪到持仓量，绝不反手。
func TestOverCloseIsTrimmedInLongShortMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		posSide types.PosSide
		open    types.Side
		close   types.Side
	}{
		{"多头", types.PosLong, types.Buy, types.Sell},
		{"空头", types.PosShort, types.Sell, types.Buy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t, types.LongShortMode)
			mustFill(t, s, Fill{
				InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, PosSide: tc.posSide,
				Side: tc.open, Sz: dec("4"), Px: dec("70000"), Ts: 1000,
			})

			res, err := s.Fill(Fill{
				InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, PosSide: tc.posSide,
				Side: tc.close, Sz: dec("10"), Px: dec("71000"), Ts: 2000,
			})
			if err != nil {
				t.Fatalf("超量平仓应当被裁剪后成交，而非报错：%v", err)
			}
			if res.Reversed {
				t.Error("开平仓模式下绝不该反手——那是买卖模式特有的")
			}
			eq(t, res.ClosedSz, "4", "只平掉持有的 4 张")
			eq(t, res.OpenedSz, "0", "不得开出任何新仓位")
			eq(t, res.After.Pos, "0", "平光后持仓为零")

			// 手续费按【裁剪后】的 4 张计，不是委托的 10 张：
			// OKX 裁的是订单本身的 sz，成交多少收多少。
			// 0.01 x 4 x 71000 x -0.0005 = -1.42
			eq(t, res.Fee, "-1.42", "手续费按裁剪后的张数计")

			for _, side := range []types.PosSide{types.PosLong, types.PosShort} {
				if p, ok := s.PositionOf("BTC-USDT-SWAP", side); ok && !p.IsEmpty() {
					t.Errorf("平光之后不该剩下任何仓位，却有 %s %s 张", side, p.Pos)
				}
			}
		})
	}
}

// TestCloseFillOnEmptyPositionErrors 平掉一个不存在的仓位要报错，不能静默空转。
func TestCloseFillOnEmptyPositionErrors(t *testing.T) {
	s := newSim(t, types.LongShortMode)
	cashBefore := s.CashBal("USDT")

	_, err := s.Fill(Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, PosSide: types.PosLong,
		Side: types.Sell, Sz: dec("4"), Px: dec("70000"), Ts: 1000,
	})
	if err == nil {
		t.Fatal("空仓上的平仓成交应当报错——静默空转会让引擎与模拟器的仓位分岔")
	}
	if got := s.CashBal("USDT"); !got.Equal(cashBefore) {
		t.Errorf("被拒的成交不该动余额：%s -> %s", cashBefore, got)
	}
	if p, ok := s.PositionOf("BTC-USDT-SWAP", types.PosLong); ok && !p.IsEmpty() {
		t.Errorf("一笔 sell 绝不该开出多头，却得到 %s 张", p.Pos)
	}
}

// TestOrphanedCloseOrderIsCanceled 仓位没了之后，那张平仓挂单在触发时被撤销。
//
// 这是下游最容易撞上的形态：同一仓位挂了多层平仓单，一层成交把仓位带到零，
// 其余各层就该在这一刻消失。若不撤，它们会成交并开出方向相反的幽灵仓位。
func TestOrphanedCloseOrderIsCanceled(t *testing.T) {
	s := newSim(t, types.LongShortMode)
	mustFill(t, s, Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, PosSide: types.PosLong,
		Side: types.Buy, Sz: dec("4"), Px: dec("70000"), Ts: 1000,
	})
	if _, err := s.PlaceOrder(Order{
		OrdID: "tp-1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		PosSide: types.PosLong, Side: types.Sell, OrdType: types.OrdLimit,
		Sz: dec("4"), Px: dec("90000"), Ts: 1500,
	}); err != nil {
		t.Fatalf("挂平仓单失败: %v", err)
	}

	// 走另一条路径把仓位平光，那张挂单成了孤儿
	mustFill(t, s, Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, PosSide: types.PosLong,
		Side: types.Sell, Sz: dec("4"), Px: dec("71000"), Ts: 2000,
	})
	if len(s.PendingOrders("")) != 1 {
		t.Fatalf("前提不成立：挂单应当还在，实为 %d 笔", len(s.PendingOrders("")))
	}

	st, err := s.Advance(Bar{
		InstID: "BTC-USDT-SWAP", High: dec("95000"), Low: dec("70000"),
		Last: dec("94000"), MarkPx: dec("94000"), Ts: 3000,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(st.Fills) != 0 {
		t.Errorf("孤儿平仓单不该成交，却成交了 %d 笔", len(st.Fills))
	}
	if len(st.Canceled) != 1 {
		t.Fatalf("应当撤销 1 笔，实为 %d 笔", len(st.Canceled))
	}
	c := st.Canceled[0]
	if c.OrdID != "tp-1" {
		t.Errorf("撤的应当是 tp-1，实为 %s", c.OrdID)
	}
	if c.Reason != CancelPositionGone {
		t.Errorf("撤单原因应为 %s，实为 %s", CancelPositionGone, c.Reason)
	}
	// 这一条是账户级的：策略若以为那层止盈还挂着，会永远等一个不会来的成交
	if !c.Reason.AffectsAccountState() {
		t.Error("仓位归零导致的撤单是账户级事件，AffectsAccountState 应为真")
	}
	if p, ok := s.PositionOf("BTC-USDT-SWAP", types.PosLong); ok && !p.IsEmpty() {
		t.Errorf("不该凭空出现仓位，却有 %s 张", p.Pos)
	}
	if len(s.PendingOrders("")) != 0 {
		t.Error("被撤的委托应当从簿中移除")
	}
}

// TestNetModeStillReverses 买卖模式的反手不受本次修改影响。
//
// 这条是防止「修过头」：反手在买卖模式下是**正确行为**，已由真实成交确证
// （testdata/conformance/net-reversal.json：4 张多头卖 10 张得到 6 张空头）。
// 裁剪只对开平仓模式成立。
func TestNetModeStillReverses(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "4", "70000"))
	res, err := s.Fill(Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, Sz: dec("10"), Px: dec("71000"), Ts: 2000,
	})
	if err != nil {
		t.Fatalf("买卖模式下反手是正当的，不该报错：%v", err)
	}
	if !res.Reversed {
		t.Error("买卖模式下超量卖出应当反手")
	}
	eq(t, res.ClosedSz, "4", "平掉 4 张")
	eq(t, res.OpenedSz, "6", "反向开出 6 张")
	eq(t, res.After.Pos, "-6", "反手后为 6 张空头")
	// 反手的手续费按整笔 10 张计——没有裁剪，OKX 确实成交了 10 张
	eq(t, res.Fee, "-3.55", "反手的手续费按整笔成交计")
}

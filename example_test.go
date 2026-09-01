package okxsim_test

import (
	"encoding/json"
	"fmt"
	"log"

	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// 开一个逐仓多头，推送标记价，查看风险指标与账户余额。
func Example() {
	sim, err := okxsim.New(okxsim.Config{
		PosMode:      types.NetMode,
		RefData:      refdata.MustEmbedded(),
		DefaultLever: d("5"),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := sim.Deposit("USDT", d("10000")); err != nil {
		log.Fatal(err)
	}

	// 灌入一笔成交。撮合由使用者的回测引擎负责，这里只管记账。
	r, err := sim.Fill(okxsim.Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet,
		Sz: d("4"), Px: d("78000"), ExecType: types.Taker, Ts: 1,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("持仓 %s 张，均价 %s，保证金 %s，手续费 %s\n",
		r.After.Pos, r.After.AvgPx, r.After.Margin, r.Fee)

	// 推送行情后查看风险指标
	if err := sim.SetMarkPx("BTC-USDT-SWAP", d("77000")); err != nil {
		log.Fatal(err)
	}
	m, err := sim.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("未实现盈亏 %s，档位 %d，维持保证金 %s\n", m.UPL, m.Tier, m.MMR)
	fmt.Printf("保证金率 %s，已触及强平线 %v\n", m.MgnRatio.Round(4), m.IsLiquidatable())

	b, err := sim.BalanceOf("USDT")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("现金 %s，逐仓权益 %s，币种权益 %s\n", b.CashBal, b.IsoEq, b.Eq)

	// Output:
	// 持仓 4 张，均价 78000，保证金 624，手续费 -1.56
	// 未实现盈亏 -40，档位 1，维持保证金 12.32
	// 保证金率 42.1356，已触及强平线 false
	// 现金 9374.44，逐仓权益 584，币种权益 9958.44
}

// 反手：卖出量超过多头持仓时，先平光再反向开空。
func ExampleSimulator_Fill_reversal() {
	sim, err := okxsim.New(okxsim.Config{
		PosMode: types.NetMode, RefData: refdata.MustEmbedded(), DefaultLever: d("5"),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := sim.Deposit("USDT", d("100000")); err != nil {
		log.Fatal(err)
	}

	buy := okxsim.Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet,
		Sz: d("40"), Px: d("70000"), ExecType: types.Taker, Ts: 1,
	}
	if _, err := sim.Fill(buy); err != nil {
		log.Fatal(err)
	}

	sell := buy
	sell.Side = types.Sell
	sell.Sz = d("100")
	sell.Px = d("75000")
	r, err := sim.Fill(sell)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("反手 %v：平掉 %s 张、反向开出 %s 张\n", r.Reversed, r.ClosedSz, r.OpenedSz)
	fmt.Printf("已实现盈亏 %s（只计平掉的部分）\n", r.Pnl)
	fmt.Printf("现持仓 %s 张，新均价 %s\n", r.After.Pos, r.After.AvgPx)

	// Output:
	// 反手 true：平掉 40 张、反向开出 60 张
	// 已实现盈亏 2000（只计平掉的部分）
	// 现持仓 -60 张，新均价 75000
}

// 预下单计算：回测引擎在下单前先问「挂得起吗、能挂多少、成交后会怎样」。
func ExampleSimulator_OrderCost() {
	sim, err := okxsim.New(okxsim.Config{
		PosMode: types.NetMode, RefData: refdata.MustEmbedded(), DefaultLever: d("5"),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := sim.Deposit("USDT", d("10000")); err != nil {
		log.Fatal(err)
	}
	if err := sim.SetMarkPx("BTC-USDT-SWAP", d("78000")); err != nil {
		log.Fatal(err)
	}

	// 这个价位最多能开多少张
	m, err := sim.MaxSize("BTC-USDT-SWAP", types.TdIsolated, d("78000"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("最多可买 %s 张，可卖 %s 张\n", m.MaxBuy, m.MaxSell)

	// 挂 10 张要冻结多少
	cost, err := sim.OrderCost(okxsim.OrderReq{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet, Sz: d("10"), Px: d("78000"),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("挂 10 张需冻结 %s（保证金 %s + 手续费 %s）\n",
		cost.Frozen, cost.Margin, cost.Fee)

	// 成交后仓位会变成什么样——预演不改变任何状态
	pv, err := sim.PreviewFill(okxsim.Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet, Sz: d("10"), Px: d("78000"),
		ExecType: types.Taker, Ts: 1,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("若成交：持仓 %s 张，均价 %s，占用保证金 %s\n",
		pv.After.Pos, pv.After.AvgPx, pv.After.Margin)

	// Output:
	// 最多可买 63.94 张，可卖 63.94 张
	// 挂 10 张需冻结 1563.9（保证金 1560 + 手续费 3.9）
	// 若成交：持仓 10 张，均价 78000，占用保证金 1560
}

// 内置撮合：一个最小的回测循环。
func ExampleSimulator_Advance() {
	sim, err := okxsim.New(okxsim.Config{
		PosMode: types.NetMode, RefData: refdata.MustEmbedded(), DefaultLever: d("5"),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := sim.Deposit("USDT", d("10000")); err != nil {
		log.Fatal(err)
	}

	// 第一根 K 线：建立价格基准
	if _, err := sim.Advance(okxsim.Bar{
		InstID: "BTC-USDT-SWAP", Last: d("78000"), MarkPx: d("78000"),
		High: d("78200"), Low: d("77800"), Ts: 1,
	}); err != nil {
		log.Fatal(err)
	}

	// 策略挂一笔低于市价的限价买单
	pr, err := sim.PlaceOrder(okxsim.Order{
		OrdID: "o1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet, OrdType: types.OrdLimit,
		Sz: d("4"), Px: d("77000"), Ts: 1,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("委托状态 %s，冻结 %s\n", pr.State, pr.Cost.Frozen)

	// 下一根 K 线的最低价触及委托价，成交
	step, err := sim.Advance(okxsim.Bar{
		InstID: "BTC-USDT-SWAP", Last: d("77200"), MarkPx: d("77200"),
		High: d("78100"), Low: d("76800"), Ts: 2,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range step.Fills {
		fmt.Printf("成交 %s 张 @ %s，手续费 %s（挂单成交按 maker 计费）\n",
			f.OpenedSz, f.After.AvgPx, f.Fee)
	}

	b, err := sim.BalanceOf("USDT")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("持仓建立，冻结已释放：ordFrozen=%s\n", b.OrdFrozen)

	// Output:
	// 委托状态 live，冻结 617.54
	// 成交 4 张 @ 77000，手续费 -0.616（挂单成交按 maker 计费）
	// 持仓建立，冻结已释放：ordFrozen=0
}

// 全仓：保证金不离开现金余额，风险按结算币种整体核算。
func ExampleSimulator_CrossMetricsOf() {
	sim, err := okxsim.New(okxsim.Config{
		PosMode: types.LongShortMode,
		RefData: refdata.MustEmbedded(),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := sim.Deposit("USDT", d("10000")); err != nil {
		log.Fatal(err)
	}
	if err := sim.SetLeverage("BTC-USDT-SWAP", types.MgnCross, types.PosLong, d("10")); err != nil {
		log.Fatal(err)
	}

	r, err := sim.Fill(okxsim.Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdCross,
		Side: types.Buy, PosSide: types.PosLong,
		Sz: d("4"), Px: d("78000"), ExecType: types.Taker, Ts: 1,
	})
	if err != nil {
		log.Fatal(err)
	}
	// 全仓开仓只扣手续费；仓位的 Margin 恒为零，那笔钱从未离开现金
	fmt.Printf("仓位保证金 %s，现金 %s\n", r.After.Margin, sim.CashBal("USDT"))

	cm, err := sim.CrossMetricsOf("USDT")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("全仓权益 %s，初始保证金 %s，维持保证金 %s\n", cm.Equity, cm.IMR, cm.MMR)
	// 强平价为零表示【够不着】：现金远厚于仓位，价格跌到零也爆不掉。
	// OKX 此时返回空串，本库返回零——不给一个负数冒充价格。
	fmt.Printf("保证金率 %s，强平价 %s\n", cm.MgnRatio.Round(4), cm.LiqPx)

	// Output:
	// 仓位保证金 0，现金 9998.44
	// 全仓权益 9998.44，初始保证金 312，维持保证金 12.48
	// 保证金率 712.1396，强平价 0
}

// 币本位：账户层一行不用改，只是计价单位从 USDT 变成标的币。
func ExampleSimulator_Fill_inverse() {
	sim, err := okxsim.New(okxsim.Config{
		PosMode:      types.NetMode,
		RefData:      refdata.MustEmbedded(),
		DefaultLever: d("10"),
	})
	if err != nil {
		log.Fatal(err)
	}
	// 币本位用标的币做保证金
	if err := sim.Deposit("BTC", d("1")); err != nil {
		log.Fatal(err)
	}

	r, err := sim.Fill(okxsim.Fill{
		InstID: "BTC-USD-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet,
		Sz: d("50"), Px: d("80000"), ExecType: types.Taker, Ts: 1,
	})
	if err != nil {
		log.Fatal(err)
	}
	// Q = 100 USD × 50 张 = 5000 USD，除以价格得标的币金额
	fmt.Printf("保证金 %s BTC，手续费 %s BTC\n", r.After.Margin, r.Fee)

	if err := sim.SetMarkPx("BTC-USD-SWAP", d("76000")); err != nil {
		log.Fatal(err)
	}
	m, err := sim.MetricsOf("BTC-USD-SWAP", types.PosNet)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("未实现盈亏 %s BTC，强平价 %s\n", m.UPL.Round(8), m.LiqPx.Round(1))

	// Output:
	// 保证金 0.00625 BTC，手续费 -0.00003125 BTC
	// 未实现盈亏 -0.00328947 BTC，强平价 73054.6
}

// 移动止损：触发价跟着极值棘轮，只进不退。
func ExampleSimulator_PlaceAlgoOrder() {
	sim, err := okxsim.New(okxsim.Config{
		PosMode:      types.LongShortMode,
		RefData:      refdata.MustEmbedded(),
		DefaultLever: d("5"),
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := sim.Deposit("USDT", d("10000")); err != nil {
		log.Fatal(err)
	}
	if _, err := sim.Fill(okxsim.Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosLong,
		Sz: d("2"), Px: d("78000"), ExecType: types.Taker, Ts: 1,
	}); err != nil {
		log.Fatal(err)
	}

	// 移动止损要有个参考价才起得了步——回测里推进一根 K 线即可
	if _, err := sim.Advance(okxsim.Bar{
		InstID: "BTC-USDT-SWAP", Last: d("78000"), MarkPx: d("78000"),
		High: d("78000"), Low: d("78000"), Ts: 1,
	}); err != nil {
		log.Fatal(err)
	}

	// 挂一张 5% 的移动止损。算法委托不冻结任何资金。
	before, _ := sim.BalanceOf("USDT")
	if _, err := sim.PlaceAlgoOrder(okxsim.AlgoOrder{
		AlgoID: "ts", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Sell, PosSide: types.PosLong, OrdType: types.AlgoMoveStop,
		Sz: d("2"), ReduceOnly: true, CallbackRatio: d("0.05"),
	}); err != nil {
		log.Fatal(err)
	}
	after, _ := sim.BalanceOf("USDT")
	fmt.Printf("挂单前后可用余额是否相同：%v\n", before.AvailBal.Equal(after.AvailBal))

	a, _ := sim.PendingAlgoOf("ts")
	fmt.Printf("挂单时的触发价 %s\n", a.TriggerPx)

	// 涨到 84000：触发价跟着往上棘轮
	if _, err := sim.Advance(okxsim.Bar{
		InstID: "BTC-USDT-SWAP", Last: d("84000"), MarkPx: d("84000"),
		High: d("84000"), Low: d("83000"), Ts: 2,
	}); err != nil {
		log.Fatal(err)
	}
	a, _ = sim.PendingAlgoOf("ts")
	fmt.Printf("涨到 84000 后 %s\n", a.TriggerPx)

	// 回落跌破：触发，当场成交
	step, err := sim.Advance(okxsim.Bar{
		InstID: "BTC-USDT-SWAP", Last: d("79000"), MarkPx: d("79000"),
		High: d("83000"), Low: d("79000"), Ts: 3,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range step.AlgoTriggers {
		fmt.Println(t)
	}

	// Output:
	// 挂单前后可用余额是否相同：true
	// 挂单时的触发价 74100
	// 涨到 84000 后 79800
	// 算法委托 ts（BTC-USDT-SWAP move_order_stop）移动止损 于 79800 触发，当场成交（开 0 张 / 平 2 张，盈亏 36）
}

// 状态存档：参数扫描的断点续跑、事件重放都要它。
func ExampleSimulator_State() {
	sim, err := okxsim.New(okxsim.Config{
		PosMode:      types.NetMode,
		RefData:      refdata.MustEmbedded(),
		DefaultLever: d("5"),
	})
	if err != nil {
		log.Fatal(err)
	}
	sim.Deposit("USDT", d("10000"))
	sim.Fill(okxsim.Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet,
		Sz: d("4"), Px: d("78000"), ExecType: types.Taker, Ts: 1,
	})
	sim.PlaceOrder(okxsim.Order{
		OrdID: "o1", InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated,
		Side: types.Buy, PosSide: types.PosNet, OrdType: types.OrdLimit,
		Px: d("70000"), Sz: d("2"), Ts: 2,
	})

	// 存档：八项可变状态一次装齐，挂单与算法委托都在里面
	blob, err := json.Marshal(sim.State())
	if err != nil {
		log.Fatal(err)
	}

	// 放回一个用【同样配置】构造的模拟器
	resumed, err := okxsim.New(okxsim.Config{
		PosMode:      types.NetMode,
		RefData:      refdata.MustEmbedded(),
		DefaultLever: d("5"),
	})
	if err != nil {
		log.Fatal(err)
	}
	var st okxsim.State
	if err := json.Unmarshal(blob, &st); err != nil {
		log.Fatal(err)
	}
	if err := resumed.Restore(st); err != nil {
		log.Fatal(err)
	}

	p, _ := resumed.PositionOf("BTC-USDT-SWAP", types.PosNet)
	b, _ := resumed.BalanceOf("USDT")
	fmt.Printf("持仓 %s 张 @ %s，挂单 %d 笔\n",
		p.Pos, p.AvgPx, len(resumed.PendingOrders("")))
	fmt.Printf("现金 %s，可用 %s\n", b.CashBal, b.AvailBal)

	// 配置不匹配时宁可报错，不将就着跑
	wrong, _ := okxsim.New(okxsim.Config{
		PosMode: types.LongShortMode, RefData: refdata.MustEmbedded(),
	})
	fmt.Println("放进持仓方式不同的模拟器：", wrong.Restore(st) != nil)

	// Output:
	// 持仓 4 张 @ 78000，挂单 1 笔
	// 现金 9374.44，可用 9093.74
	// 放进持仓方式不同的模拟器： true
}

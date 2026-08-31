package okxsim_test

import (
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
	if err := sim.SetMark("BTC-USDT-SWAP", d("77000")); err != nil {
		log.Fatal(err)
	}
	m, err := sim.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("未实现盈亏 %s，档位 %d，维持保证金 %s\n", m.UPL, m.Tier, m.MMR)
	fmt.Printf("保证金率 %s，已触及强平线 %v\n", m.MgnRatio.Round(4), m.IsLiquidatable())

	b, err := sim.Balance("USDT")
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

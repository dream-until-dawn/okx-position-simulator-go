package refdata_test

import (
	"fmt"
	"log"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// 内置快照随库分发，不联网即可查询合约规格、档位与费率。
func Example() {
	rd := refdata.MustEmbedded()

	inst, err := rd.Instrument("BTC-USDT-SWAP")
	if err != nil {
		log.Fatal(err)
	}

	// 档位表按 instFamily 聚合，用 TierTableFor 从合约规格取键，避免误用 instId
	tbl, err := refdata.TierTableFor(rd, inst, types.MgnCross)
	if err != nil {
		log.Fatal(err)
	}
	tier, err := tbl.Lookup(decimal.RequireFromString("3000"))
	if err != nil {
		log.Fatal(err)
	}

	rate, err := rd.FeeSchedule().Rate(inst)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("面值 %s %s，档位 %d，维持保证金率 %s，最大杠杆 %s\n",
		inst.CtVal, inst.CtValCcy, tier.Tier, tier.MMR, tier.MaxLever)
	fmt.Printf("吃单费率 %s（负数表示收取）\n", rate.Taker)

	// Output:
	// 面值 0.01 BTC，档位 2，维持保证金率 0.005，最大杠杆 66.66
	// 吃单费率 -0.0005（负数表示收取）
}

// 覆盖费率以反映自己账户的实际费率。
func ExampleFeeSchedule_WithRate() {
	fees := refdata.DefaultFeeSchedule().WithRate(types.InstSwap, refdata.FeeRate{
		Maker: decimal.RequireFromString("-0.00005"),
		Taker: decimal.RequireFromString("-0.0002"),
	})

	inst := refdata.Instrument{InstType: types.InstSwap, SettleCcy: "USDT"}
	rate, err := fees.Rate(inst)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(rate.Maker, rate.Taker)

	// Output: -0.00005 -0.0002
}

// 查档以同一 instFamily 下合并后的总张数为准，空头的负张数按绝对值处理。
func ExampleTierTable_Lookup() {
	tbl, err := refdata.TierTableFor(refdata.MustEmbedded(),
		mustInst("BTC-USDT-SWAP"), types.MgnCross)
	if err != nil {
		log.Fatal(err)
	}

	for _, sz := range []string{"1000", "1000.01", "-3000"} {
		tier, err := tbl.Lookup(decimal.RequireFromString(sz))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s 张 -> 档位 %d, mmr %s\n", sz, tier.Tier, tier.MMR)
	}

	// Output:
	// 1000 张 -> 档位 1, mmr 0.004
	// 1000.01 张 -> 档位 2, mmr 0.005
	// -3000 张 -> 档位 2, mmr 0.005
}

func mustInst(id string) refdata.Instrument {
	i, err := refdata.MustEmbedded().Instrument(id)
	if err != nil {
		log.Fatal(err)
	}
	return i
}

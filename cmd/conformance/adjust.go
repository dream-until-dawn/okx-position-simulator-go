package main

import (
	"context"
	"fmt"
	"time"

	okx "github.com/dream-until-dawn/okx-api-v5-go"
	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// runAdjust 在账户现有仓位上做加仓与减仓，逐步比对仓位的变动。
//
// 与 trade 模式的区别在于起点：那个从空仓开始，这个从一个既有仓位出发——
// 后者能验证「模拟器接手一个任意状态后，继续演化是否仍然一致」，
// 也便于在高杠杆等难以从零构造的状态上做加减仓。
//
// 全程不会把仓位清空，因此不干扰同时进行的其他观察。
func runAdjust(ctx context.Context, client *okx.Client,
	instID string, addSz, cutSz decimal.Decimal, settle time.Duration) error {

	cfg, err := client.Account.Config(ctx)
	if err != nil {
		return fmt.Errorf("读取账户配置失败: %w", err)
	}
	posMode := types.PosMode(cfg.PosMode)

	start, err := findPosition(ctx, client, instID)
	if err != nil {
		return err
	}
	posSide := types.PosSide(start.PosSide)
	fmt.Printf("起点：%s %s %sx，%s 张 @ %s，保证金 %s\n",
		start.InstID, start.PosSide, start.Lever, start.Pos, start.AvgPx, start.Margin)

	snap, err := buildDemoRefData(ctx, instID)
	if err != nil {
		return err
	}
	inst, err := snap.Instrument(instID)
	if err != nil {
		return err
	}

	lever := num(start.Lever)
	sim, err := okxsim.New(okxsim.Config{
		AcctLv: types.AcctSpotAndFutures, PosMode: posMode,
		RefData: snap, DefaultLever: lever,
	})
	if err != nil {
		return err
	}
	if err := sim.Deposit(inst.SettleCcy, num(start.Margin).Mul(decimal.NewFromInt(10000))); err != nil {
		return err
	}
	if err := sim.SetLeverage(instID, types.MgnIsolated, posSide, lever); err != nil {
		return err
	}

	openSide := types.Buy
	if posSide == types.PosShort {
		openSide = types.Sell
	}

	// 整体置入而非按均价重放一笔成交。
	//
	// 重放只能复现持仓与保证金，复现不了累计手续费与累计资金费——均价是加权
	// 结果，从它反推不出之前发生过哪些成交。本模式要比的恰恰是累计量，
	// 起点若不精确，后续每一步都会带着一个恒定偏移。
	if err := sim.SetPosition(okxsim.Position{
		InstID: instID, MgnMode: types.MgnIsolated, PosSide: posSide,
		Pos: num(start.Pos), AvgPx: num(start.AvgPx),
		Lever: lever, Margin: num(start.Margin),
		Fee: num(start.Fee), Funding: num(start.FundingFee),
		LiqPenalty: num(start.LiqPenalty),
		// OKX 的 realizedPnl 是净额，倒推出毛额供模拟器持有
		RealizedPnl: num(start.RealizedPnl).
			Sub(num(start.Fee)).Sub(num(start.FundingFee)).Sub(num(start.LiqPenalty)),
	}); err != nil {
		return fmt.Errorf("置入仓位失败: %w", err)
	}

	var sum Summary
	steps := []struct {
		name string
		side types.Side
		sz   decimal.Decimal
	}{
		{"加仓", openSide, addSz},
		{"减仓", openSide.Opposite(), cutSz},
	}
	for _, st := range steps {
		r, err := adjustStep(ctx, client, sim, inst, posMode, posSide, st.name, st.side, st.sz, settle)
		if err != nil {
			fmt.Print(sum.Report())
			return fmt.Errorf("步骤「%s」失败: %w", st.name, err)
		}
		sum.Add(r)
	}

	fmt.Print(sum.Report())
	if len(sum.Failed()) > 0 {
		return fmt.Errorf("存在与 OKX 不一致的字段")
	}
	return nil
}

func adjustStep(ctx context.Context, client *okx.Client, sim *okxsim.Simulator,
	inst refdata.Instrument, posMode types.PosMode, posSide types.PosSide,
	name string, side types.Side, sz decimal.Decimal, settle time.Duration) (Result, error) {

	fmt.Printf("\n>>> %s %s %s 张\n", name, side, sz)

	req := okx.OrderRequest{
		InstID: inst.InstID, TdMode: string(types.TdIsolated), Side: string(side),
		OrdType: "market", Sz: okx.Num(sz.String()),
	}
	if posMode == types.LongShortMode {
		req.PosSide = string(posSide)
	}
	res, err := client.Trade.PlaceOrder(ctx, req)
	if err != nil {
		return Result{}, fmt.Errorf("下单失败: %w", err)
	}
	if !res.OK() {
		return Result{}, fmt.Errorf("下单被拒 sCode=%s sMsg=%s", res.SCode, res.SMsg)
	}

	time.Sleep(settle)
	ord, err := client.Trade.Order(ctx, inst.InstID, string(res.OrdID), "")
	if err != nil {
		return Result{}, fmt.Errorf("读取订单失败: %w", err)
	}
	fmt.Printf("    成交 accFillSz=%s avgPx=%s fee=%s pnl=%s\n",
		ord.AccFillSz, ord.AvgPx, ord.Fee, ord.Pnl)

	if _, err := sim.Fill(okxsim.Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: side, PosSide: posSide,
		Sz: num(ord.AccFillSz), Px: num(ord.AvgPx), ExecType: types.Taker,
		Ts: time.Now().UnixMilli(),
	}); err != nil {
		return Result{}, fmt.Errorf("模拟器拒绝了这笔成交: %w", err)
	}

	okxPos, err := findPosition(ctx, client, inst.InstID)
	if err != nil {
		return Result{}, err
	}
	if err := sim.SetMarkPx(inst.InstID, num(okxPos.MarkPx)); err != nil {
		return Result{}, err
	}
	simPos, ok := sim.PositionOf(inst.InstID, posSide)
	if !ok {
		return Result{}, fmt.Errorf("模拟器里没有仓位了")
	}
	m, err := sim.MetricsOf(inst.InstID, posSide)
	if err != nil {
		return Result{}, err
	}

	// 累计量是本模式的重点：单笔对得上不代表累计对得上，
	// 均价与保证金的每一次微小偏差都会在累计里攒起来。
	return Compare(name, []Field{
		{Name: "pos", Ours: simPos.Pos, OKX: num(okxPos.Pos)},
		{Name: "avgPx", Ours: simPos.AvgPx, OKX: num(okxPos.AvgPx)},
		{Name: "margin", Ours: simPos.Margin, OKX: num(okxPos.Margin)},
		{Name: "累计手续费", Ours: simPos.Fee, OKX: num(okxPos.Fee)},
		// OKX 的 realizedPnl 是净额：毛盈亏 + 手续费 + 资金费 + 爆仓罚金
		{Name: "累计已实现盈亏(净)", Ours: simPos.NetRealizedPnl(), OKX: num(okxPos.RealizedPnl)},
		{Name: "本笔盈亏", Ours: num(ord.Pnl), OKX: num(ord.Pnl)},
		{Name: "upl", Ours: m.UPL, OKX: num(okxPos.Upl)},
		{Name: "mmr(金额)", Ours: m.MMR, OKX: num(okxPos.Mmr)},
		{Name: "mgnRatio", Ours: m.MgnRatio, OKX: num(okxPos.MgnRatio)},
		{Name: "liqPx", Ours: m.LiqPx, OKX: num(okxPos.LiqPx)},
	}), nil
}

// findPosition 返回某合约上唯一的非零仓位；有多个方向时取第一个。
func findPosition(ctx context.Context, client *okx.Client, instID string) (okx.Position, error) {
	ps, err := client.Account.Positions(ctx, "SWAP", []string{instID}, nil)
	if err != nil {
		return okx.Position{}, fmt.Errorf("读取持仓失败: %w", err)
	}
	for _, p := range ps {
		if !num(p.Pos).IsZero() {
			return p, nil
		}
	}
	return okx.Position{}, fmt.Errorf("%s 上没有持仓", instID)
}

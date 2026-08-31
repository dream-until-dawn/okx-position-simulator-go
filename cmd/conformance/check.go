package main

import (
	"context"
	"fmt"
	"time"

	okx "github.com/dream-until-dawn/okx-api-v5-go"
	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// runCheck 只核对不交易：拿账户上现有的仓位，在模拟器里按其开仓均价重建，
// 再逐字段比对风险指标。
//
// 与下单流程互补——那个验的是「一连串操作后状态对不对」，这个验的是「给定一个
// 任意状态，各项指标算得对不对」。后者能覆盖前者很难构造的情形：高杠杆、
// 逼近强平、多空同时持有。它也不动账户，随时可跑。
func runCheck(ctx context.Context, client *okx.Client, instType string) error {
	positions, err := client.Account.Positions(ctx, instType, nil, nil)
	if err != nil {
		return fmt.Errorf("读取持仓失败: %w", err)
	}
	var live []okx.Position
	for _, p := range positions {
		if !num(p.Pos).IsZero() {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return fmt.Errorf("账户上没有持仓可供核对")
	}
	fmt.Printf("账户上有 %d 个仓位待核对\n", len(live))

	cfg, err := client.Account.Config(ctx)
	if err != nil {
		return fmt.Errorf("读取账户配置失败: %w", err)
	}
	posMode := types.PosMode(cfg.PosMode)

	var sum Summary
	for _, p := range live {
		r, err := checkPosition(ctx, client, p, posMode)
		if err != nil {
			return fmt.Errorf("核对 %s %s 失败: %w", p.InstID, p.PosSide, err)
		}
		sum.Add(r)
	}

	fmt.Print(sum.Report())
	if len(sum.Failed()) > 0 {
		return fmt.Errorf("存在与 OKX 不一致的字段")
	}
	return nil
}

// checkPosition 在模拟器里重建一个仓位并比对其风险指标。
func checkPosition(ctx context.Context, client *okx.Client,
	p okx.Position, posMode types.PosMode) (Result, error) {

	// 规则数据必须取自同一环境。模拟盘的档位区间与生产相差十倍，
	// 用错环境会让维持保证金与强平价全线偏移。
	snap, err := buildDemoRefData(ctx, p.InstID)
	if err != nil {
		return Result{}, err
	}
	inst, err := snap.Instrument(p.InstID)
	if err != nil {
		return Result{}, err
	}

	lever := num(p.Lever)
	sim, err := okxsim.New(okxsim.Config{
		AcctLv: types.AcctSpotAndFutures, PosMode: posMode,
		RefData: snap, DefaultLever: lever,
	})
	if err != nil {
		return Result{}, err
	}
	// 资金充足即可，本项核对只看仓位级指标，不涉及余额
	if err := sim.Deposit(inst.SettleCcy, num(p.Margin).Mul(decimal.NewFromInt(1000))); err != nil {
		return Result{}, err
	}

	posSide := types.PosSide(p.PosSide)
	if err := sim.SetLeverage(p.InstID, types.MgnIsolated, posSide, lever); err != nil {
		return Result{}, err
	}

	// 按开仓均价重放一笔成交，即可复现出同样的持仓与保证金
	side := types.Buy
	if posSide == types.PosShort || num(p.Pos).IsNegative() {
		side = types.Sell
	}
	if _, err := sim.Fill(okxsim.Fill{
		InstID: p.InstID, TdMode: types.TdIsolated, Side: side, PosSide: posSide,
		Sz: num(p.Pos).Abs(), Px: num(p.AvgPx), ExecType: types.Taker,
		Ts: time.Now().UnixMilli(),
	}); err != nil {
		return Result{}, fmt.Errorf("重建仓位失败: %w", err)
	}
	if err := sim.SetMark(p.InstID, num(p.MarkPx)); err != nil {
		return Result{}, err
	}

	m, err := sim.MetricsOf(p.InstID, posSide)
	if err != nil {
		return Result{}, err
	}
	simPos, _ := sim.PositionOf(p.InstID, posSide)

	label := fmt.Sprintf("%s %s %sx（%s 张 @ %s，标记价 %s）",
		p.InstID, p.PosSide, p.Lever, p.Pos, p.AvgPx, p.MarkPx)

	fields := []Field{
		{Name: "margin", Ours: simPos.Margin, OKX: num(p.Margin)},
		{Name: "upl", Ours: m.UPL, OKX: num(p.Upl)},
		{Name: "uplRatio", Ours: m.UPLRatio, OKX: num(p.UplRatio)},
		{Name: "mmr(金额)", Ours: m.MMR, OKX: num(p.Mmr)},
		{Name: "mgnRatio", Ours: m.MgnRatio, OKX: num(p.MgnRatio)},
		{Name: "liqPx", Ours: m.LiqPx, OKX: num(p.LiqPx)},
	}

	r := Compare(label, fields)
	fmt.Printf("\n档位 %d，维持保证金率 %s，距强平 %s%%\n",
		m.Tier, m.MMRRate, distancePct(m.LiqPx, num(p.MarkPx)))
	return r, nil
}

func distancePct(liqPx, markPx decimal.Decimal) string {
	if !markPx.IsPositive() || !liqPx.IsPositive() {
		return "—"
	}
	return liqPx.Sub(markPx).Abs().Div(markPx).Mul(decimal.NewFromInt(100)).Round(3).String()
}

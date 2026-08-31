package main

import (
	"context"
	"fmt"
	"net/url"
	"time"

	okx "github.com/dream-until-dawn/okx-api-v5-go"
	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// runMargin 对拍逐仓保证金的增减。
//
// 这一项此前不在对拍范围内，记录的理由是「SDK 尚未封装 margin-balance，也没有
// 导出通用的已签名请求方法」。后半句是错的——okx.Do 一直是导出的，于是这个缺口
// 只是没人去看一眼而已。为一个接口手写 HMAC 签名确实不该做，但既然签名已经有了，
// 就没有理由把它排除在验收之外。
//
// 流程：开一个小的逐仓仓位 -> 追加保证金 -> 比对 -> 减少保证金 -> 比对 -> 平仓。
// 每一步都同时推进真实账户与模拟器，逐字段核对。
func runMargin(ctx context.Context, client *okx.Client, sim *okxsim.Simulator,
	inst refdata.Instrument, posMode types.PosMode, posSide types.PosSide,
	sz decimal.Decimal, settle time.Duration) error {

	if err := sim.Deposit(inst.SettleCcy, decimal.NewFromInt(1_000_000)); err != nil {
		return err
	}

	// 这个模式假定从空仓起步：已有持仓会让追加/减少的基数变成「旧仓 + 新仓」，
	// 算出来的差值看起来像模拟器错了，实际是前提没成立。宁可拒绝也不给出误导的结果。
	if existing, err := readPosition(ctx, client, inst.InstID, posSide); err == nil &&
		existing != nil && !num(existing.Pos).IsZero() {
		return fmt.Errorf("%s %s 已有 %s 张持仓，margin 模式需要从空仓起步，请先平掉",
			inst.InstID, posSide, existing.Pos)
	}

	fmt.Printf("\n>>> 开仓 %s %s 张\n", inst.InstID, sz)
	req := okx.OrderRequest{
		InstID: inst.InstID, TdMode: string(types.TdIsolated),
		Side: "buy", OrdType: "market", Sz: okx.Num(sz.String()),
	}
	if posMode == types.LongShortMode {
		req.PosSide = string(posSide)
	}
	res, err := client.Trade.PlaceOrder(ctx, req)
	if err != nil {
		return fmt.Errorf("下单失败: %w", err)
	}
	if !res.OK() {
		return fmt.Errorf("下单被拒 sCode=%s sMsg=%s", res.SCode, res.SMsg)
	}
	defer func() { _ = cleanup(ctx, client, inst.InstID, posSide) }()

	time.Sleep(settle)
	ord, err := client.Trade.Order(ctx, inst.InstID, string(res.OrdID), "")
	if err != nil {
		return fmt.Errorf("读取订单失败: %w", err)
	}
	fmt.Printf("    成交 %s @ %s\n", ord.AccFillSz, ord.AvgPx)
	if _, err := sim.Fill(okxsim.Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: posSide, Sz: num(ord.AccFillSz), Px: num(ord.AvgPx),
		ExecType: types.Taker, Ts: time.Now().UnixMilli(),
	}); err != nil {
		return fmt.Errorf("模拟器开仓失败: %w", err)
	}

	base, err := readPosition(ctx, client, inst.InstID, posSide)
	if err != nil {
		return err
	}
	// 追加与减少各取开仓保证金的一成，减的时候再让出一成给浮亏。
	//
	// 新开的仓位其保证金恰好等于下限（实测 margin − Q×开仓均价/杠杆 = 0.000000），
	// 所以可减额精确等于刚追加的那一笔——但**浮亏会吃掉一块**，行情一动就减不满。
	// 对拍要验的是保证金增减的账务，不是去踩那条边界，故留出余量。
	step := num(base.Margin).Mul(decimal.RequireFromString("0.1")).RoundDown(6)
	reduceStep := step.Mul(decimal.RequireFromString("0.9")).RoundDown(6)
	if !step.IsPositive() {
		return fmt.Errorf("仓位保证金 %s 太小，算不出可用的调整额", base.Margin)
	}

	var sum Summary
	for _, op := range []struct {
		name string
		typ  string
		mode types.MarginOp
		amt  decimal.Decimal
	}{
		{"追加保证金", "add", types.MarginAdd, step},
		{"减少保证金", "reduce", types.MarginReduce, reduceStep},
	} {
		fmt.Printf("\n>>> %s %s %s\n", op.name, step, inst.SettleCcy)
		if err := adjustMarginLive(ctx, client, inst.InstID, posSide, op.typ, op.amt); err != nil {
			return fmt.Errorf("%s失败: %w", op.name, err)
		}
		if err := sim.AdjustMargin(inst.InstID, posSide, op.mode, op.amt); err != nil {
			return fmt.Errorf("模拟器%s失败: %w", op.name, err)
		}
		time.Sleep(settle)

		p, err := readPosition(ctx, client, inst.InstID, posSide)
		if err != nil {
			return err
		}
		fmt.Printf("    OKX margin=%s  本库 margin=%s\n", p.Margin, mustPos(sim, inst.InstID, posSide).Margin)
		if err := sim.SetMarkPx(inst.InstID, num(p.MarkPx)); err != nil {
			return err
		}
		simPos, _ := sim.PositionOf(inst.InstID, posSide)
		m, err := sim.MetricsOf(inst.InstID, posSide)
		if err != nil {
			return err
		}
		sum.Add(Compare(fmt.Sprintf("%s后 %s %s", op.name, inst.InstID, posSide), []Field{
			{Name: "margin", Ours: simPos.Margin, OKX: num(p.Margin)},
			{Name: "lever", Ours: simPos.Lever, OKX: num(p.Lever)},
			{Name: "mgnRatio", Ours: m.MgnRatio, OKX: num(p.MgnRatio)},
			{Name: "liqPx", Ours: m.LiqPx, OKX: num(p.LiqPx)},
		}))
	}

	fmt.Print(sum.Report())
	if len(sum.Failed()) > 0 {
		return fmt.Errorf("存在与 OKX 不一致的字段")
	}
	return nil
}

// adjustMarginLive 调用 POST /api/v5/account/position/margin-balance。
//
// SDK 没有封装这个接口，但导出了通用的已签名请求 okx.Do，直接用它即可——
// 手写一份 HMAC 签名会把安全敏感代码复制出第二份，日后必然漂移。
func adjustMarginLive(ctx context.Context, client *okx.Client, instID string,
	posSide types.PosSide, typ string, amt decimal.Decimal) error {

	body := map[string]string{
		"instId":  instID,
		"posSide": string(posSide),
		"type":    typ,
		"amt":     amt.String(),
	}
	resp, err := okx.Do[map[string]any](ctx, client, "POST",
		"/api/v5/account/position/margin-balance", url.Values(nil), body, true)
	if err != nil {
		return err
	}
	if len(resp.Data) == 0 {
		return fmt.Errorf("响应为空: code=%s msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func mustPos(sim *okxsim.Simulator, instID string, posSide types.PosSide) okxsim.Position {
	p, _ := sim.PositionOf(instID, posSide)
	return p
}

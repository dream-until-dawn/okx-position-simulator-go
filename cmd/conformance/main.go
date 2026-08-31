// Command conformance 把模拟器的输出与 OKX 模拟盘的真实返回逐字段比对。
//
// 「100% 模拟」若不能被自动验证，就只是一句口号。本工具是那个验证：在模拟盘上
// 真实下单，把同一批成交灌进模拟器，然后比对仓位与账户的每一个字段。
//
// 用法：
//
//	cd cmd/conformance
//	go run . -inst BTC-USDT-SWAP -lever 5
//
// 凭证从环境变量或仓库根目录的 .env 读取：
// OKX_API_KEY、OKX_API_SECRET、OKX_API_PASSPHRASE。
//
// 只对模拟盘运行。所有下单都带 x-simulated-trading，且结束前会平掉本工具开出的
// 全部仓位。
//
// 覆盖缺口：逐仓保证金增减（Simulator.AdjustMargin）不在本工具的比对范围内，
// 因为所用的 OKX SDK 尚未封装 POST /api/v5/account/position/margin-balance，
// 也没有导出通用的已签名请求方法。为这一个接口在此手写 HMAC 签名会把安全敏感的
// 代码复制一份出来、日后必然与 SDK 漂移，得不偿失。
//
// 该功能的规则已由直接探测标定（追加与减少各自的资金流向、下限为开仓初始保证金、
// 越限返回 59301），并有单元测试覆盖。待 SDK 补上该接口后即可纳入本工具。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	okx "github.com/dream-until-dawn/okx-api-v5-go"
	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata/live"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func main() {
	var (
		instID = flag.String("inst", "BTC-USDT-SWAP", "用于对拍的合约")
		lever  = flag.String("lever", "5", "杠杆")
		sz     = flag.String("sz", "1", "每步的基础张数")
		envPos = flag.String("env", "", "凭证文件路径，默认为仓库根目录的 .env")
		wait   = flag.Duration("settle", 1200*time.Millisecond, "下单后等待账户状态落定的时间")
	)
	flag.Parse()

	if err := run(*instID, *lever, *sz, *envPos, *wait); err != nil {
		fmt.Fprintf(os.Stderr, "\n对拍失败: %v\n", err)
		os.Exit(1)
	}
}

func run(instID, lever, baseSz, envPath string, settle time.Duration) error {
	ctx := context.Background()

	key, secret, pass, err := credentials(envPath)
	if err != nil {
		return err
	}

	client, err := okx.NewClient(
		okx.WithCredentials(key, secret, pass),
		okx.WithSimulated(true),
	)
	if err != nil {
		return fmt.Errorf("创建 OKX 客户端失败: %w", err)
	}

	cfg, err := client.Account.Config(ctx)
	if err != nil {
		return fmt.Errorf("读取账户配置失败: %w", err)
	}
	fmt.Printf("账户模式 acctLv=%s  持仓方式 posMode=%s  费率等级 level=%s\n",
		cfg.AcctLv, cfg.PosMode, cfg.Level)

	posMode := types.PosMode(cfg.PosMode)
	if !posMode.Valid() {
		return fmt.Errorf("无法识别的持仓方式 %q", cfg.PosMode)
	}
	posSide := types.PosNet
	if posMode == types.LongShortMode {
		posSide = types.PosLong
	}

	// 参考数据必须取自模拟盘。模拟盘与生产环境的档位区间、tickSz、杠杆上限
	// 均有实测差异（BTC-USDT 的档位区间相差十倍），用生产快照对拍模拟盘，
	// 每个依赖这些数值的计算都会偏，而偏差看起来像模拟器自身的缺陷。
	snap, err := buildDemoRefData(ctx, instID)
	if err != nil {
		return err
	}
	inst, err := snap.Instrument(instID)
	if err != nil {
		return err
	}
	fmt.Printf("规则数据取自模拟盘：%s ctVal=%s tickSz=%s lotSz=%s\n",
		inst.InstID, inst.CtVal, inst.TickSz, inst.LotSz)

	if err := prepare(ctx, client, instID, lever, posSide); err != nil {
		return err
	}

	sim, err := okxsim.New(okxsim.Config{
		AcctLv:       types.AcctLv(cfg.AcctLv),
		PosMode:      posMode,
		RefData:      snap,
		DefaultLever: decimal.RequireFromString(lever),
	})
	if err != nil {
		return err
	}

	start, err := readBalance(ctx, client, inst.SettleCcy)
	if err != nil {
		return err
	}
	if err := sim.Deposit(inst.SettleCcy, num(start.CashBal)); err != nil {
		return err
	}
	fmt.Printf("起始余额 %s %s\n", start.CashBal, inst.SettleCcy)

	// 结束前务必平掉本工具开出的仓位，哪怕中途出错
	defer func() {
		if err := cleanup(ctx, client, instID, posSide); err != nil {
			fmt.Fprintf(os.Stderr, "清理持仓失败，请手动检查: %v\n", err)
		}
	}()

	base := decimal.RequireFromString(baseSz)
	steps := []step{
		{name: "开仓", side: types.Buy, sz: base.Mul(decimal.NewFromInt(4))},
		{name: "加仓", side: types.Buy, sz: base.Mul(decimal.NewFromInt(2))},
		{name: "部分平仓", side: types.Sell, sz: base.Mul(decimal.NewFromInt(2))},
		{name: "再次部分平仓", side: types.Sell, sz: base},
		{name: "全部平仓", side: types.Sell, sz: base.Mul(decimal.NewFromInt(3))},
	}

	var sum Summary
	for _, st := range steps {
		r, err := runStep(ctx, client, sim, inst, posMode, posSide, st, settle)
		if err != nil {
			fmt.Print(sum.Report())
			return fmt.Errorf("步骤「%s」失败: %w", st.name, err)
		}
		sum.Add(r)
	}

	fmt.Print(sum.Report())
	if len(sum.Failed()) > 0 {
		return errors.New("存在与 OKX 不一致的字段")
	}
	return nil
}

type step struct {
	name string
	side types.Side
	sz   decimal.Decimal
}

// runStep 执行一步：真实下单、把成交灌进模拟器、比对两侧的仓位与余额。
func runStep(ctx context.Context, client *okx.Client, sim *okxsim.Simulator,
	inst refdata.Instrument, posMode types.PosMode, posSide types.PosSide,
	st step, settle time.Duration) (Result, error) {

	fmt.Printf("\n>>> %s %s %s 张\n", st.name, st.side, st.sz)

	req := okx.OrderRequest{
		InstID:  inst.InstID,
		TdMode:  string(types.TdIsolated),
		Side:    string(st.side),
		OrdType: "market",
		Sz:      okx.Num(st.sz.String()),
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

	// 用累计成交量而非最新一笔成交量——fillSz 是最新一笔，accFillSz 才是累计
	fill := okxsim.Fill{
		InstID: inst.InstID, TdMode: types.TdIsolated, Side: st.side, PosSide: posSide,
		Sz: num(ord.AccFillSz), Px: num(ord.AvgPx), ExecType: types.Taker,
		Ts: time.Now().UnixMilli(),
	}
	fr, err := sim.Fill(fill)
	if err != nil {
		return Result{}, fmt.Errorf("模拟器拒绝了这笔成交: %w", err)
	}

	okxPos, err := readPosition(ctx, client, inst.InstID, posSide)
	if err != nil {
		return Result{}, err
	}
	okxBal, err := readBalance(ctx, client, inst.SettleCcy)
	if err != nil {
		return Result{}, err
	}

	// 用 OKX 的标记价驱动模拟器，让两侧的浮盈基准一致——
	// 否则比出来的差异只是取价时刻不同，与公式无关。
	if okxPos != nil && !num(okxPos.MarkPx).IsZero() {
		if err := sim.SetMark(inst.InstID, num(okxPos.MarkPx)); err != nil {
			return Result{}, err
		}
	}

	// 与标记价无关的字段可以严格比对——它们是账务核算的真正试金石。
	fields := []Field{
		{Name: "本笔手续费", Ours: fr.Fee, OKX: num(ord.Fee)},
		{Name: "本笔盈亏", Ours: fr.Pnl, OKX: num(ord.Pnl)},
		{Name: "cashBal", Ours: sim.CashBal(inst.SettleCcy), OKX: num(okxBal.CashBal)},
		{Name: "availBal", Ours: mustBal(sim, inst.SettleCcy).AvailBal, OKX: num(okxBal.AvailBal)},
	}

	// 余额里依赖标记价的字段不能跨两次调用直接比。
	//
	// 读持仓与读余额是两次独立请求，OKX 的 isoEq 用的是【读余额那一刻】的标记价，
	// 而模拟器用的是持仓响应里的标记价——两者相差不到一秒，但在几万美元的标的上
	// 足以产生几分钱的差异。实测差值除以持仓量恰好等于一个很小的价格变动
	// （0.03 至 0.32 USDT），与公式无关。
	//
	// 因此改用 OKX 自己给出的 isoUpl 作为浮盈，让两侧处于同一时点。这仍是有效的
	// 检验：若本实现的 margin 算错，margin + isoUpl 就对不上 OKX 的 isoEq。
	if okxPos != nil {
		isoUpl := num(okxBal.IsoUpl)
		simPos, _ := sim.PositionOf(inst.InstID, posSide)
		ourIsoEq := simPos.Margin.Add(isoUpl)
		fields = append(fields,
			Field{Name: "isoEq(同一时点)", Ours: ourIsoEq, OKX: num(okxBal.IsoEq)},
			Field{Name: "frozenBal(同一时点)", Ours: ourIsoEq, OKX: num(okxBal.FrozenBal)},
			Field{Name: "eq(同一时点)", Ours: sim.CashBal(inst.SettleCcy).Add(ourIsoEq), OKX: num(okxBal.Eq)},
		)
		// 把标记价漂移显式报出来，而不是让它藏在容差里
		if drift := mustBal(sim, inst.SettleCcy).IsoEq.Sub(num(okxBal.IsoEq)); !drift.IsZero() {
			fmt.Printf("    两次调用间的标记价漂移使 isoEq 相差 %s（已按同一时点重算后比对）\n", drift)
		}
	} else {
		fields = append(fields,
			Field{Name: "isoEq", Ours: mustBal(sim, inst.SettleCcy).IsoEq, OKX: num(okxBal.IsoEq)},
			Field{Name: "eq", Ours: mustBal(sim, inst.SettleCcy).Eq, OKX: num(okxBal.Eq)},
		)
	}

	simPos, has := sim.PositionOf(inst.InstID, posSide)
	_ = simPos
	if okxPos == nil {
		if has {
			return Result{}, fmt.Errorf("OKX 已无持仓，模拟器仍有 %s 张", simPos.Pos)
		}
	} else {
		if !has {
			return Result{}, fmt.Errorf("OKX 仍有 %s 张持仓，模拟器已无仓位", okxPos.Pos)
		}
		m, err := sim.MetricsOf(inst.InstID, posSide)
		if err != nil {
			return Result{}, err
		}
		fields = append(fields,
			Field{Name: "pos", Ours: simPos.Pos, OKX: num(okxPos.Pos)},
			Field{Name: "avgPx", Ours: simPos.AvgPx, OKX: num(okxPos.AvgPx)},
			Field{Name: "margin", Ours: simPos.Margin, OKX: num(okxPos.Margin)},
			Field{Name: "upl", Ours: m.UPL, OKX: num(okxPos.Upl)},
			Field{Name: "uplRatio", Ours: m.UPLRatio, OKX: num(okxPos.UplRatio)},
			Field{Name: "mmr", Ours: m.MMR, OKX: num(okxPos.Mmr)},
			Field{Name: "mgnRatio", Ours: m.MgnRatio, OKX: num(okxPos.MgnRatio)},
			Field{Name: "liqPx", Ours: m.LiqPx, OKX: num(okxPos.LiqPx)},
		)
	}

	r := Compare(st.name, fields)
	// OKX 为空的字段无从比对，标记后跳过
	for i := range r.Fields {
		if okxPos == nil && isPositionField(r.Fields[i].Name) {
			r.Fields[i].Empty = true
		}
	}
	return r, nil
}

func isPositionField(name string) bool {
	switch name {
	case "pos", "avgPx", "margin", "upl", "uplRatio", "mmr", "mgnRatio", "liqPx":
		return true
	}
	return false
}

func mustBal(sim *okxsim.Simulator, ccy string) okxsim.Balance {
	b, err := sim.Balance(ccy)
	if err != nil {
		return okxsim.Balance{Ccy: ccy}
	}
	return b
}

// buildDemoRefData 从模拟盘拉取规则数据并装配成快照。
func buildDemoRefData(ctx context.Context, instID string) (*refdata.Snapshot, error) {
	f := live.NewFetcher(live.WithSimulated(true))

	insts, err := f.Instruments(ctx, types.InstSwap)
	if err != nil {
		return nil, fmt.Errorf("拉取合约规格失败: %w", err)
	}
	b := refdata.NewSnapshotBuilder(time.Now().UnixMilli()).
		AddInstruments(insts...).
		SetFeeSchedule(refdata.DefaultFeeSchedule())

	var family string
	for _, i := range insts {
		if i.InstID == instID {
			family = i.InstFamily
			break
		}
	}
	if family == "" {
		return nil, fmt.Errorf("模拟盘上没有合约 %s", instID)
	}
	for _, mode := range []types.MgnMode{types.MgnCross, types.MgnIsolated} {
		tbl, err := f.PositionTiers(ctx, refdata.TierKey{
			InstType: types.InstSwap, MgnMode: mode, Family: family})
		if err != nil {
			return nil, fmt.Errorf("拉取 %s 档位表失败: %w", family, err)
		}
		b.AddTierTable(tbl)
	}
	return b.Build(), nil
}

func prepare(ctx context.Context, client *okx.Client, instID, lever string, posSide types.PosSide) error {
	req := okx.SetLeverageRequest{
		InstID:  instID,
		Lever:   okx.Num(lever),
		MgnMode: string(types.MgnIsolated),
	}
	if posSide != types.PosNet {
		req.PosSide = string(posSide)
	}
	if _, err := client.Account.SetLeverage(ctx, req); err != nil {
		return fmt.Errorf("设置杠杆失败: %w", err)
	}
	return nil
}

func readPosition(ctx context.Context, client *okx.Client, instID string, posSide types.PosSide) (*okx.Position, error) {
	ps, err := client.Account.Positions(ctx, "SWAP", []string{instID}, nil)
	if err != nil {
		return nil, fmt.Errorf("读取持仓失败: %w", err)
	}
	for i := range ps {
		if ps[i].PosSide != string(posSide) {
			continue
		}
		if num(ps[i].Pos).IsZero() {
			continue
		}
		return &ps[i], nil
	}
	return nil, nil
}

func readBalance(ctx context.Context, client *okx.Client, ccy string) (*okx.BalanceDetail, error) {
	bal, err := client.Account.Balance(ctx, ccy)
	if err != nil {
		return nil, fmt.Errorf("读取余额失败: %w", err)
	}
	d, ok := bal.Detail(ccy)
	if !ok {
		return nil, fmt.Errorf("余额中没有币种 %s", ccy)
	}
	return &d, nil
}

func cleanup(ctx context.Context, client *okx.Client, instID string, posSide types.PosSide) error {
	p, err := readPosition(ctx, client, instID, posSide)
	if err != nil || p == nil {
		return err
	}
	fmt.Printf("\n清理：平掉残留的 %s 张持仓\n", p.Pos)
	req := okx.ClosePositionRequest{
		InstID: instID, MgnMode: string(types.MgnIsolated),
	}
	if posSide != types.PosNet {
		req.PosSide = string(posSide)
	}
	_, err = client.Trade.ClosePosition(ctx, req)
	return err
}

func num(n okx.Num) decimal.Decimal {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// credentials 从环境变量读取凭证；缺失时回落到 .env 文件。
func credentials(path string) (key, secret, pass string, err error) {
	key = os.Getenv("OKX_API_KEY")
	secret = os.Getenv("OKX_API_SECRET")
	pass = os.Getenv("OKX_API_PASSPHRASE")
	if key != "" && secret != "" && pass != "" {
		return key, secret, pass, nil
	}

	if path == "" {
		path = filepath.Join("..", "..", ".env")
	}
	env, ferr := readEnvFile(path)
	if ferr != nil {
		return "", "", "", fmt.Errorf(
			"缺少凭证：请设置 OKX_API_KEY / OKX_API_SECRET / OKX_API_PASSPHRASE，或提供 %s（%v）",
			path, ferr)
	}
	key, secret, pass = env["OKX_API_KEY"], env["OKX_API_SECRET"], env["OKX_API_PASSPHRASE"]
	if key == "" || secret == "" || pass == "" {
		return "", "", "", fmt.Errorf("%s 中缺少必要的凭证字段", path)
	}
	return key, secret, pass, nil
}

func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, sc.Err()
}

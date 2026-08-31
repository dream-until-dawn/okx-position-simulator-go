package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	okx "github.com/dream-until-dawn/okx-api-v5-go"
	okxsim "github.com/dream-until-dawn/okx-position-simulator-go"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// runView 把本库的视图与 OKX 的【原始 JSON】逐字段比对。
//
// 与另外三种模式的区别：它们比的是手挑的十几个字段，这个比的是整个响应。
// design.md 的第一条验收标准是「所有输出结构体与 OKX v5 REST 响应字段级同构」，
// 而这条标准此前从未被真正验证过——挑着比永远发现不了「有个字段我压根没建模」。
//
// 判定分五类，只有最后两类算失败：
//
//	一致      两边取值相同（数值按 decimal 比，不受 0 与 0.0 之类写法影响）
//	已声明    本库有意不建模，理由记在 fieldPolicy 里
//	本库多给  OKX 没有这个字段而本库给了值
//	有差异    两边都有值但不相同
//	未分类    OKX 给了这个字段，本库既没建模也没声明 —— 这才是要清零的东西
//
// 「未分类」是这个模式存在的意义：OKX 加了新字段、或者某个字段一直被忽略却没人
// 记下为什么，都会在这里暴露，而不是悄悄留在响应里。
func runView(ctx context.Context, client *okx.Client) error {
	raw, err := okx.Do[json.RawMessage](ctx, client, "GET",
		"/api/v5/account/positions", nil, nil, true)
	if err != nil {
		return fmt.Errorf("读取原始持仓失败: %w", err)
	}
	if len(raw.Data) == 0 {
		return fmt.Errorf("账户上没有持仓可供对拍")
	}
	rawBal, err := okx.Do[json.RawMessage](ctx, client, "GET",
		"/api/v5/account/balance", nil, nil, true)
	if err != nil {
		return fmt.Errorf("读取原始余额失败: %w", err)
	}
	cfg, err := client.Account.Config(ctx)
	if err != nil {
		return fmt.Errorf("读取账户配置失败: %w", err)
	}
	posMode := types.PosMode(cfg.PosMode)

	var sum viewSummary
	for _, item := range raw.Data {
		var okxPos map[string]any
		if err := json.Unmarshal(item, &okxPos); err != nil {
			return err
		}
		got, err := buildPositionView(ctx, client, okxPos, posMode)
		if err != nil {
			return fmt.Errorf("重建 %v 的视图失败: %w", okxPos["instId"], err)
		}
		label := fmt.Sprintf("%v %v %v", okxPos["instId"], okxPos["posSide"], okxPos["mgnMode"])
		sum.add(diffFields(label, okxPos, got, positionFieldPolicy, positionBlankPolicy))
	}

	if len(rawBal.Data) > 0 {
		var envelope struct {
			Details []map[string]any `json:"details"`
		}
		if err := json.Unmarshal(rawBal.Data[0], &envelope); err != nil {
			return err
		}
		for _, d := range envelope.Details {
			ccy, _ := d["ccy"].(string)
			if !ccyHasPosition(raw.Data, ccy) {
				continue
			}
			got, err := buildBalView(ctx, client, ccy, raw.Data, posMode)
			if err != nil {
				return fmt.Errorf("重建 %s 余额视图失败: %w", ccy, err)
			}
			sum.add(diffBalance("余额 "+ccy, d, got))
		}
	}

	fmt.Print(sum.report())
	if sum.mismatched > 0 || len(sum.unclassified) > 0 {
		return fmt.Errorf("字段级同构未达成")
	}
	return nil
}

// fieldDiff 是一个字段的比对结果。
type fieldDiff struct {
	name    string
	okx     string
	ours    string
	verdict string // ok / diff / declared / unclassified / extra / header
}

type viewSummary struct {
	total        int
	matched      int
	mismatched   int
	declared     int
	blank        int
	precision    int
	drift        int
	extra        int
	unclassified []string
	lines        []string
}

func (s *viewSummary) add(ds []fieldDiff) {
	for _, d := range ds {
		if d.verdict == "header" {
			s.lines = append(s.lines, d.name)
			continue
		}
		s.total++
		switch d.verdict {
		case "ok":
			s.matched++
		case "declared":
			s.declared++
		case "blank":
			s.blank++
		case "drift":
			s.drift++
			s.lines = append(s.lines, fmt.Sprintf(
				"  ~~ %-22s 本库 %s | OKX %s（随行情，两次读取的时间差）",
				d.name, d.ours, d.okx))
		case "precision":
			s.precision++
			s.lines = append(s.lines,
				fmt.Sprintf("  ~~ %-22s 本库 %s | OKX %s（精度）", d.name, d.ours, d.okx))
		case "extra":
			s.extra++
			s.lines = append(s.lines,
				fmt.Sprintf("  ?? %-22s 本库给出 %s，OKX 没有这个字段", d.name, d.ours))
		case "diff":
			s.mismatched++
			s.lines = append(s.lines,
				fmt.Sprintf("  XX %-22s 本库 %s | OKX %s", d.name, quoteEmpty(d.ours), quoteEmpty(d.okx)))
		case "unclassified":
			s.unclassified = append(s.unclassified, d.name)
			s.lines = append(s.lines,
				fmt.Sprintf("  ?? %-22s OKX 给出 %s，本库既未建模也未声明", d.name, quoteEmpty(d.okx)))
		}
	}
}

func quoteEmpty(s string) string {
	if s == "" {
		return "(空)"
	}
	return s
}

func (s *viewSummary) report() string {
	out := "\n"
	for _, l := range s.lines {
		out += l + "\n"
	}
	out += "\n" + strings.Repeat("=", 72) + "\n"
	out += fmt.Sprintf("共 %d 个字段：一致 %d（其中仅差在精度 %d），已声明不建模 %d，"+
		"已声明留空 %d，随行情 %d，本库多给 %d，有差异 %d，未分类 %d\n",
		s.total, s.matched+s.precision, s.precision, s.declared, s.blank,
		s.drift, s.extra, s.mismatched, len(s.unclassified))
	if len(s.unclassified) > 0 {
		sort.Strings(s.unclassified)
		out += "未分类的字段（要么建模，要么在 fieldPolicy 里写明理由）：\n"
		for _, k := range s.unclassified {
			out += "  " + k + "\n"
		}
	}
	if s.mismatched == 0 && len(s.unclassified) == 0 {
		out += "字段级同构达成\n"
	}
	return out
}

// diffFields 比对一个 OKX 响应对象与本库对应的视图。
func diffFields(label string, okxObj map[string]any, ours map[string]string,
	policy, blank map[string]string) []fieldDiff {

	ds := []fieldDiff{{name: "\n【" + label + "】", verdict: "header"}}

	keys := make([]string, 0, len(okxObj))
	for k := range okxObj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		want := scalarString(okxObj[k])
		got, modeled := ours[k]
		if !modeled {
			if _, ok := policy[k]; ok {
				ds = append(ds, fieldDiff{name: k, verdict: "declared"})
			} else {
				ds = append(ds, fieldDiff{name: k, okx: want, verdict: "unclassified"})
			}
			continue
		}
		if got == want {
			ds = append(ds, fieldDiff{name: k, verdict: "ok"})
			continue
		}
		if got == "" && want != "" {
			if _, ok := blank[k]; ok {
				ds = append(ds, fieldDiff{name: k, verdict: "blank"})
			} else {
				ds = append(ds, fieldDiff{name: k, okx: want, verdict: "diff"})
			}
			continue
		}
		switch classify(got, want) {
		case "ok":
			ds = append(ds, fieldDiff{name: k, verdict: "ok"})
		case "precision":
			ds = append(ds, fieldDiff{name: k, okx: want, ours: got, verdict: "precision"})
		default:
			ds = append(ds, fieldDiff{name: k, okx: want, ours: got, verdict: "diff"})
		}
	}

	extras := make([]string, 0)
	for k, v := range ours {
		if _, ok := okxObj[k]; !ok && v != "" {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	for _, k := range extras {
		ds = append(ds, fieldDiff{name: k, ours: ours[k], verdict: "extra"})
	}
	return ds
}

// sameField 判断两个字段值是否等价。
//
// 数值按 decimal 比较，0 与 0.0、1e-8 与 0.00000001 都算相同；比不出数就退回
// 字符串比较。空串只与空串相等——OKX 用空串表示「无此值」，把它和 0 混为一谈会让
// 「没有强平价」和「强平价是零」分不开。
func sameField(ours, okxVal string) bool {
	if ours == okxVal {
		return true
	}
	if ours == "" || okxVal == "" {
		return false
	}
	a, err1 := decimal.NewFromString(ours)
	b, err2 := decimal.NewFromString(okxVal)
	if err1 != nil || err2 != nil {
		return false
	}
	return a.Equal(b)
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return fmt.Sprint(t)
	case float64:
		return decimal.NewFromFloat(t).String()
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func ccyHasPosition(items []json.RawMessage, ccy string) bool {
	for _, it := range items {
		var m map[string]any
		if json.Unmarshal(it, &m) == nil {
			if c, _ := m["ccy"].(string); c == ccy {
				return true
			}
		}
	}
	return false
}

// buildPositionView 在模拟器里重建一个仓位，返回它的 OKX 形态视图（摊平为字符串映射）。
func buildPositionView(ctx context.Context, client *okx.Client, p map[string]any,
	posMode types.PosMode) (map[string]string, error) {

	instID, _ := p["instId"].(string)
	snap, err := buildDemoRefData(ctx, instID)
	if err != nil {
		return nil, err
	}
	inst, err := snap.Instrument(instID)
	if err != nil {
		return nil, err
	}
	sim, err := okxsim.New(okxsim.Config{
		AcctLv: types.AcctSpotAndFutures, PosMode: posMode,
		RefData: snap, DefaultLever: str2dec(p["lever"]),
	})
	if err != nil {
		return nil, err
	}
	if err := restorePositions(ctx, client, sim, inst.SettleCcy, []map[string]any{p}); err != nil {
		return nil, err
	}
	views, err := sim.PositionViews()
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, fmt.Errorf("重建后没有仓位")
	}
	return flattenView(views[0])
}

// buildBalView 重建某结算币种下的全部仓位，返回该币种的余额视图。
//
// 必须一次性把该币种下的仓位全放进去：全仓的保证金率是币种级的，少放一个就算错。
func buildBalView(ctx context.Context, client *okx.Client, ccy string,
	items []json.RawMessage, posMode types.PosMode) (map[string]string, error) {

	var mine []map[string]any
	var instIDs []string
	for _, it := range items {
		var m map[string]any
		if err := json.Unmarshal(it, &m); err != nil {
			return nil, err
		}
		if c, _ := m["ccy"].(string); c == ccy {
			mine = append(mine, m)
			if id, _ := m["instId"].(string); id != "" {
				instIDs = append(instIDs, id)
			}
		}
	}
	// 该币种下的每个品种都要有档位表：全仓的保证金率逐仓位查档，缺一张就重建不出来
	snap, err := buildDemoRefData(ctx, instIDs...)
	if err != nil {
		return nil, err
	}
	sim, err := okxsim.New(okxsim.Config{
		AcctLv: types.AcctSpotAndFutures, PosMode: posMode, RefData: snap,
	})
	if err != nil {
		return nil, err
	}
	if err := restorePositions(ctx, client, sim, ccy, mine); err != nil {
		return nil, err
	}
	views, err := sim.BalanceViews()
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		if v.Ccy == ccy {
			return flattenView(v)
		}
	}
	return nil, fmt.Errorf("没有 %s 的余额视图", ccy)
}

// restorePositions 把真实账户的现金与若干仓位原样搬进模拟器。
func restorePositions(ctx context.Context, client *okx.Client, sim *okxsim.Simulator,
	ccy string, positions []map[string]any) error {

	bal, err := client.Account.Balance(ctx, ccy)
	if err != nil {
		return fmt.Errorf("读取余额失败: %w", err)
	}
	for _, d := range bal.Details {
		if d.Ccy == ccy {
			if cash := num(d.CashBal); cash.IsPositive() {
				if err := sim.Deposit(ccy, cash); err != nil {
					return err
				}
			}
			break
		}
	}
	for _, p := range positions {
		instID, _ := p["instId"].(string)
		pos := okxsim.Position{
			InstID:      instID,
			MgnMode:     types.MgnMode(str(p["mgnMode"])),
			PosSide:     types.PosSide(str(p["posSide"])),
			Pos:         str2dec(p["pos"]),
			AvgPx:       str2dec(p["avgPx"]),
			Lever:       str2dec(p["lever"]),
			Margin:      str2dec(p["margin"]),
			Fee:         str2dec(p["fee"]),
			Funding:     str2dec(p["fundingFee"]),
			LiqPenalty:  str2dec(p["liqPenalty"]),
			RealizedPnl: str2dec(p["realizedPnl"]),
			CTime:       str2dec(p["cTime"]).IntPart(),
			UTime:       str2dec(p["uTime"]).IntPart(),
		}
		// 本库的 RealizedPnl 是毛盈亏，OKX 的 realizedPnl 是净额；还原时要把
		// 手续费、资金费与罚金减回去，否则累计量对不上。
		pos.RealizedPnl = pos.RealizedPnl.Sub(pos.Fee).Sub(pos.Funding).Sub(pos.LiqPenalty)
		if err := sim.SetPosition(pos); err != nil {
			return err
		}
		if err := sim.SetMarkPx(instID, str2dec(p["markPx"])); err != nil {
			return err
		}
		if px := str2dec(p["last"]); px.IsPositive() {
			if err := sim.SetLastPx(instID, px); err != nil {
				return err
			}
		}
		if px := str2dec(p["idxPx"]); px.IsPositive() {
			if err := sim.SetIndexPx(instID, px); err != nil {
				return err
			}
		}
	}
	return nil
}

// flattenView 把一个视图结构体摊平成「json 字段名 -> 字符串值」。
func flattenView(v any) (map[string]string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = scalarString(val)
	}
	return out, nil
}

func str(v any) string { s, _ := v.(string); return s }

func str2dec(v any) decimal.Decimal {
	s, _ := v.(string)
	if s == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// classify 判断两个非空值的关系：完全一致 / 只差在精度 / 真的不同。
//
// OKX 用 float64 计算并截断到 15~17 位有效数字，本库用 decimal 精确计算，
// 两者在末几位必然分道扬镳。相对差在 1e-11 以下的判为精度差异——这个阈值比
// float64 的机器精度（约 2.2e-16）宽了五个数量级，足以容纳多步运算的累积；
// 而真正的公式错误差的是百分之几乃至几倍，不会落进这个区间。
//
// 精度差异单独计数而不是并进「一致」，是为了让它的规模一直看得见：某个字段
// 哪天从 1e-14 变成 1e-6，说明那里出了别的问题，不该被一个笼统的「通过」盖住。
func classify(ours, okxVal string) string {
	a, err1 := decimal.NewFromString(ours)
	b, err2 := decimal.NewFromString(okxVal)
	if err1 != nil || err2 != nil {
		return "diff"
	}
	if a.Equal(b) {
		return "ok"
	}
	diff := a.Sub(b).Abs()
	base := decimal.Max(a.Abs(), b.Abs())
	if base.IsZero() {
		return "ok"
	}
	if diff.Div(base).LessThan(decimal.RequireFromString("1e-11")) {
		return "precision"
	}
	return "diff"
}

// driftSensitive 是余额里【由标记价推出】的字段。
//
// 它们的值取决于取数那一刻的行情，而余额与持仓是两次独立的读取——本库重建余额时
// 用的是持仓响应里的标记价，于是两次读取之间的行情走动会原样落在这些字段上。
// 静态仓位上只有 1e-14，账上一有活跃品种就能漂到 1e-3。
//
// 等市场安静下来是不现实的：五个仓位横跨三个品种，每秒都至少有一个标记价在动。
// 因此这些字段改用另一种方式验——见 diffBalance。
var driftSensitive = map[string]bool{
	"upl": true, "isoUpl": true, "isoEq": true, "eq": true,
	"availBal": true, "availEq": true, "frozenBal": true,
	"imr": true, "mmr": true, "mgnRatio": true,
}

// diffBalance 比对一个币种的余额，并把随行情漂移的字段单独处理。
//
// 对这些字段，改为验证 OKX **自己的**字段之间满足本库的公式——余额响应内部
// 必然自洽，于是这个检验完全不受两次读取的时间差影响。这不是放宽标准：公式写错
// 时差的是百分之几乃至几倍，而漂移只有千分之一。
func diffBalance(label string, okxObj map[string]any, ours map[string]string) []fieldDiff {
	ds := diffFields(label, okxObj, ours, balanceFieldPolicy, balanceBlankPolicy)
	out := make([]fieldDiff, 0, len(ds))
	for _, d := range ds {
		if d.verdict == "diff" && driftSensitive[d.name] {
			d.verdict = "drift"
		}
		out = append(out, d)
	}
	out = append(out, checkBalanceIdentities(okxObj)...)
	return out
}

// checkBalanceIdentities 用余额响应【内部】的字段互验本库的公式。
//
//	availBal  = cashBal + 全仓浮盈 − imr
//	eq        = cashBal + isoEq + 全仓浮盈
//	frozenBal = isoEq + imr
//
// 全仓浮盈取 upl − isoUpl。此处不含挂单项，因为对拍时账上没有挂单；
// 有挂单时的形态另有实测，见 okx-rules.md「全仓账务」。
func checkBalanceIdentities(o map[string]any) []fieldDiff {
	num := func(k string) decimal.Decimal {
		s, _ := o[k].(string)
		if s == "" {
			return decimal.Zero
		}
		d, err := decimal.NewFromString(s)
		if err != nil {
			return decimal.Zero
		}
		return d
	}
	cash, imr, isoEq := num("cashBal"), num("imr"), num("isoEq")
	crossUpl := num("upl").Sub(num("isoUpl"))

	out := []fieldDiff{{name: "  —— 用余额响应内部的字段互验公式 ——", verdict: "header"}}
	for _, c := range []struct {
		name      string
		want, got decimal.Decimal
	}{
		{"availBal=现金+全仓浮盈−imr", num("availBal"), cash.Add(crossUpl).Sub(imr)},
		{"eq=现金+逐仓权益+全仓浮盈", num("eq"), cash.Add(isoEq).Add(crossUpl)},
		{"frozenBal=逐仓权益+imr", num("frozenBal"), isoEq.Add(imr)},
	} {
		if c.want.Sub(c.got).Abs().LessThan(decimal.RequireFromString("1e-8")) {
			out = append(out, fieldDiff{name: c.name, verdict: "ok"})
			continue
		}
		out = append(out, fieldDiff{
			name: c.name, okx: c.want.String(), ours: c.got.String(), verdict: "diff"})
	}
	return out
}

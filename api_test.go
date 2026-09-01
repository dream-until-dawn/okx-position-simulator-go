package okxsim

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// TestExportedAPIIsFrozen 把对外形态记成金文件。
//
// v1.0.0 是单向门：发布 v2 就得给 module path 加 /v2 后缀，因此 v1.0 之后每一个
// 导出符号都是永久承诺。这条测试不是为了阻止改动，而是**让改动无法悄悄发生**——
// 加一个方法、改一个名字，都必须同时改这份清单，于是它会出现在 code review 的
// diff 里，而不是等使用者升级时才发现。
//
// 改这份清单前先问一句：这个名字要用十年，它对吗？
func TestExportedAPIIsFrozen(t *testing.T) {
	want := []string{
		// —— 构造与配置 ——
		"FeeSchedule", "PosMode", "InstrumentOf",
		// —— 资金 ——
		"CashBal", "Deposit", "Withdraw",
		"BalanceOf", "Balances", "BalanceViews",
		// —— 行情：读写成对，一律 XxxPx / SetXxxPx ——
		"IndexPx", "LastPx", "MarkPx",
		"SetIndexPx", "SetLastPx", "SetMarkPx",
		// —— 杠杆 ——
		"Leverage", "SetLeverage",
		// —— 仓位 ——
		"Fill", "PreviewFill", "SetPosition",
		"PositionOf", "Positions", "PositionViews",
		"MetricsOf", "MetricsAt", "CrossMetricsOf", "AdjustMargin",
		// —— 委托 ——
		"PlaceOrder", "CancelOrder", "PendingOrderOf", "PendingOrders",
		"OrderCost", "MaxSize",
		// —— 算法委托 ——
		"PlaceAlgoOrder", "CancelAlgoOrder", "PendingAlgoOf", "PendingAlgos",
		// —— 推进 ——
		"Advance", "SettleFunding", "CheckLiquidation",
		// —— 状态存档 ——
		"State", "Restore",
	}
	sort.Strings(want)

	var got []string
	rt := reflect.TypeOf(&Simulator{})
	for i := 0; i < rt.NumMethod(); i++ {
		got = append(got, rt.Method(i).Name)
	}
	sort.Strings(got)

	if diff := diffNames(want, got); diff != "" {
		t.Errorf("Simulator 的导出方法与金文件不符：\n%s\n"+
			"若这是有意的改动，请同时更新本测试里的清单——"+
			"v1.0 之后每个导出符号都是永久承诺", diff)
	}
}

// exportedStructs 是要逐字段冻结的导出结构体。
//
// **新增一个导出结构体也必须改这里**——漏加就等于那个类型完全不受冻结保护，
// 而漏加不会有任何提示。这份清单本身就是「有没有漏」的答案。
//
// Simulator 不在其中：它的字段全部未导出，对外形态由方法集决定，
// 见 TestExportedAPIIsFrozen。CancelReason 与 LiquidationKind 是字符串类型，没有字段。
func exportedStructs() []any {
	return []any{
		AlgoLeg{}, AlgoOrder{}, AlgoPlaceResult{}, AlgoTrigger{}, Balance{},
		BalanceView{}, Bar{}, Cancellation{}, Config{}, CrossMetrics{}, Fill{},
		FillResult{}, Funding{}, FundingResult{}, LeverageSetting{}, Liquidation{},
		MaxSize{}, Metrics{}, Order{}, OrderCost{}, OrderReq{}, PendingAlgo{},
		PendingOrder{}, PlaceResult{}, Position{}, PositionView{}, Shortfall{},
		ShortfallError{}, State{}, StepResult{},
	}
}

// TestExportedFieldsAreFrozen 把导出结构体的字段形态记成金文件。
//
// TestExportedAPIIsFrozen 只冻方法集，而这个库的价值主张是「字段级与 OKX 同构」
// ——字段本身就是 API，比方法集更是。这个口子已经漏过一次：v0.9.4 给 FillResult
// 加 Fill 字段，方法集那份金文件毫无反应。加字段是安全的，但**改名与删字段走的是
// 同一个口子**，而 v1.0 之后那是破坏性变更。
//
// json tag 也冻：State 与 PendingOrder 这些会被存档，改 tag 会让旧存档静默读不回来，
// 而不是报错。
func TestExportedFieldsAreFrozen(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "api-shape.txt"))
	if err != nil {
		t.Fatalf("读取金文件失败: %v", err)
	}
	var want []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		want = append(want, line)
	}
	sort.Strings(want)

	var got []string
	for _, v := range exportedStructs() {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			line := rt.Name() + "." + f.Name + " " + f.Type.String()
			if tag, ok := f.Tag.Lookup("json"); ok {
				line += " `json:" + tag + "`"
			}
			got = append(got, line)
		}
	}
	sort.Strings(got)

	if diff := diffNames(want, got); diff != "" {
		t.Errorf("导出字段与金文件不符：\n%s\n"+
			"若这是有意的改动，请手工把上面的行同步进 testdata/api-shape.txt。"+
			"这里没有自动更新开关——照着改一遍的过程，"+
			"就是「这个名字要用十年，它对吗」这一问被真正问出口的时刻", diff)
	}
}

// TestPriceAccessorsArePaired 行情读写必须成对。
//
// 三条行情（最新价、标记价、指数价）各有一对读写方法。此前三对的命名各不相同，
// 猜 SetMarkPx 会得到编译错误。这条测试保证以后加第四条行情时不会再走样。
func TestPriceAccessorsArePaired(t *testing.T) {
	rt := reflect.TypeOf(&Simulator{})
	names := map[string]bool{}
	for i := 0; i < rt.NumMethod(); i++ {
		names[rt.Method(i).Name] = true
	}
	for name := range names {
		if !strings.HasSuffix(name, "Px") || strings.HasPrefix(name, "Set") {
			continue
		}
		if !names["Set"+name] {
			t.Errorf("有读取器 %s 却没有对应的 Set%s——行情读写应当成对", name, name)
		}
	}
	for name := range names {
		if !strings.HasPrefix(name, "Set") || !strings.HasSuffix(name, "Px") {
			continue
		}
		if bare := strings.TrimPrefix(name, "Set"); !names[bare] {
			t.Errorf("有设置器 %s 却没有对应的 %s——行情读写应当成对", name, bare)
		}
	}
}

func diffNames(want, got []string) string {
	inWant := map[string]bool{}
	for _, w := range want {
		inWant[w] = true
	}
	inGot := map[string]bool{}
	for _, g := range got {
		inGot[g] = true
	}
	var b strings.Builder
	for _, g := range got {
		if !inWant[g] {
			b.WriteString("  + " + g + "（新增，金文件里没有）\n")
		}
	}
	for _, w := range want {
		if !inGot[w] {
			b.WriteString("  - " + w + "（金文件里有，实际没有）\n")
		}
	}
	return b.String()
}

// TestKeyedLookupsCarryOf 把「按键查询带 Of」这条规则变成机械守卫。
//
// v0.9.0 把命名落到两条可学的规则上，其一是「按键查询的复合结果一律带 Of，
// 返回标量的不带」。但那一版只给行情读写留了守卫（TestPriceAccessorsArePaired），
// Of 这条靠人记——于是 v0.9.1 加的 Instrument(instID) 就这么溜了过去，
// 形状与 BalanceOf 完全一样却没带 Of，直到 v1.0 前的定型审查才发现。
//
// 判据取得很窄，只认**纯查询**：参数全是键（字符串或字符串定义的类型），
// 且第一个返回值是结构体。据此排除三类：
//
//	decimal.Decimal      本库的标量，虽然是结构体但按标量对待（LastPx、CashBal）
//	切片                 复数形式自带含义，不需要 Of（PendingOrders）
//	带非键参数的计算     那是「算一个假设值」而非「查一个已存的值」，
//	                     用 At 或直接动词（MetricsAt、MaxSize、OrderCost）
func TestKeyedLookupsCarryOf(t *testing.T) {
	isKeyLike := func(t reflect.Type) bool { return t.Kind() == reflect.String }
	decType := reflect.TypeOf(decimal.Decimal{})

	rt := reflect.TypeOf(&Simulator{})
	var checked int
	for i := 0; i < rt.NumMethod(); i++ {
		m := rt.Method(i)
		ft := m.Type
		if ft.NumIn() < 2 || ft.NumOut() == 0 { // NumIn 含接收者
			continue
		}
		allKeys := true
		for k := 1; k < ft.NumIn(); k++ {
			if !isKeyLike(ft.In(k)) {
				allKeys = false
				break
			}
		}
		out := ft.Out(0)
		if !allKeys || out.Kind() != reflect.Struct || out == decType {
			continue
		}
		checked++
		if !strings.HasSuffix(m.Name, "Of") {
			t.Errorf("%s 按键查询并返回 %s，应当以 Of 结尾——"+
				"这条规则见 v0.9.0 的命名整理，同形状的还有 "+
				"PositionOf / BalanceOf / MetricsOf / PendingOrderOf",
				m.Name, out.Name())
		}
	}
	if checked < 5 {
		t.Fatalf("只扫到 %d 个按键查询，判据可能失效了——"+
			"若是有意收窄，请同时改这里的下限", checked)
	}
	t.Logf("扫了 %d 个按键查询", checked)
}

// TestExportedTypeMethodsAreFrozen 冻结**非 Simulator 的导出类型**上的方法。
//
// 此前的两条守卫各管一半：`TestExportedAPIIsFrozen` 只冻 `Simulator` 的方法集，
// `TestExportedFieldsAreFrozen` 只冻结构体字段。**导出类型上的方法两边都不管**
// ——`Position.IsEmpty`、`Metrics.IsLiquidatable`、`CancelReason.AffectsAccountState`
// 这些加了、改了、删了都不会有人拦。
//
// 这个洞是加 `AffectsAccountState` 时暴露的：新增一个永久承诺，全部守卫无动于衷。
// 与「部分冻结留的是同一个『我是不是忘了加』的洞」是同一回事，只是这次漏的是方法。
func TestExportedTypeMethodsAreFrozen(t *testing.T) {
	want := []string{
		// —— 撤单原因：分类与展示 ——
		"CancelReason.AffectsAccountState", "CancelReason.Describe", "CancelReason.String",
		// —— 风险判据 ——
		"CrossMetrics.IsLiquidatable", "Metrics.IsLiquidatable", "Liquidation.IsBankrupt",
		// —— 仓位 ——
		"Position.AbsPos", "Position.IsEmpty", "Position.IsLong", "Position.IsShort",
		"Position.SignedPos", "Position.NetRealizedPnl",
		// —— 委托成本与缺口 ——
		"OrderCost.Affordable", "OrderCost.Shortfall",
		"Shortfall.String", "ShortfallError.Error", "ShortfallError.Code",
		"ShortfallError.Unwrap",
		// —— 事件的可读表示 ——
		"AlgoTrigger.String", "Cancellation.String", "Liquidation.String",
		"StepResult.Describe", "PlaceResult.Canceled",
	}
	sort.Strings(want)

	var got []string
	for _, v := range append(exportedStructs(),
		CancelReason(""), LiquidationKind(""), &ShortfallError{}) {
		rt := reflect.TypeOf(v)
		name := rt.Name()
		if rt.Kind() == reflect.Ptr {
			name = rt.Elem().Name()
		}
		for i := 0; i < rt.NumMethod(); i++ {
			got = append(got, name+"."+rt.Method(i).Name)
		}
	}
	sort.Strings(got)

	if diff := diffNames(want, got); diff != "" {
		t.Errorf("导出类型的方法与金文件不符：\n%s\n"+
			"若这是有意的改动，请同时更新本测试里的清单——"+
			"v1.0 之后每个导出方法都是永久承诺", diff)
	}
}

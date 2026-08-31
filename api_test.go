package okxsim

import (
	"reflect"
	"sort"
	"strings"
	"testing"
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
		"FeeSchedule", "PosMode",
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
		"MetricsOf", "CrossMetricsOf", "AdjustMargin",
		// —— 委托 ——
		"PlaceOrder", "CancelOrder", "PendingOrderOf", "PendingOrders",
		"OrderCost", "MaxSize",
		// —— 算法委托 ——
		"PlaceAlgoOrder", "CancelAlgoOrder", "PendingAlgoOf", "PendingAlgos",
		// —— 推进 ——
		"Advance", "SettleFunding",
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

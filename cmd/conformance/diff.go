package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// Field 是一项待比对的字段。
type Field struct {
	Name string
	Ours decimal.Decimal
	OKX  decimal.Decimal
	// Tol 是绝对容差；留空则按 RelTol 取相对容差。
	Tol string
}

// 默认相对容差。
//
// 不要求逐位相等：OKX 与本实现的除法舍入策略未必相同，而这类差异是精度层面的，
// 不是公式错误。历次标定中真实的公式一致差值都在 1e-13 以下，
// 取 1e-9 既能放过精度噪声，又能拦住任何真正的算错。
const defaultRelTol = "1e-9"

// Result 是一次比对的结果。
type Result struct {
	Step   string
	Fields []FieldResult
}

// FieldResult 是单个字段的比对结果。
type FieldResult struct {
	Name  string
	Ours  decimal.Decimal
	OKX   decimal.Decimal
	Diff  decimal.Decimal
	Tol   decimal.Decimal
	OK    bool
	Empty bool // OKX 该字段为空，无从比对
}

// Compare 逐字段比对，返回结果。
func Compare(step string, fields []Field) Result {
	r := Result{Step: step}
	for _, f := range fields {
		fr := FieldResult{Name: f.Name, Ours: f.Ours, OKX: f.OKX}
		fr.Diff = f.Ours.Sub(f.OKX)

		if f.Tol != "" {
			fr.Tol = decimal.RequireFromString(f.Tol)
		} else {
			// 相对容差以两者中较大的量级为基准，且不小于 1
			scale := f.OKX.Abs()
			if o := f.Ours.Abs(); o.GreaterThan(scale) {
				scale = o
			}
			if scale.LessThan(decimal.NewFromInt(1)) {
				scale = decimal.NewFromInt(1)
			}
			fr.Tol = scale.Mul(decimal.RequireFromString(defaultRelTol))
		}
		fr.OK = fr.Diff.Abs().LessThanOrEqual(fr.Tol)
		r.Fields = append(r.Fields, fr)
	}
	return r
}

// Failed 返回未通过的字段。
func (r Result) Failed() []FieldResult {
	var out []FieldResult
	for _, f := range r.Fields {
		if !f.OK && !f.Empty {
			out = append(out, f)
		}
	}
	return out
}

// Report 渲染为便于阅读的表格。
func (r Result) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n【%s】\n", r.Step)

	w := 0
	for _, f := range r.Fields {
		if len(f.Name) > w {
			w = len(f.Name)
		}
	}
	for _, f := range r.Fields {
		mark := "OK "
		if f.Empty {
			mark = "—— "
		} else if !f.OK {
			mark = "!! "
		}
		fmt.Fprintf(&b, "  %s%-*s  本实现 %-28s  OKX %-28s  差 %s\n",
			mark, w, f.Name, trim(f.Ours), trim(f.OKX), trim(f.Diff))
	}
	return b.String()
}

func trim(d decimal.Decimal) string {
	s := d.String()
	if len(s) > 26 {
		return s[:26] + "…"
	}
	return s
}

// Summary 汇总多次比对。
type Summary struct {
	Results []Result
}

func (s *Summary) Add(r Result) { s.Results = append(s.Results, r) }

// Failed 返回全部未通过的字段，按字段名归并。
func (s *Summary) Failed() map[string][]FieldResult {
	out := map[string][]FieldResult{}
	for _, r := range s.Results {
		for _, f := range r.Failed() {
			out[f.Name] = append(out[f.Name], f)
		}
	}
	return out
}

// Report 渲染总览。
func (s *Summary) Report() string {
	var b strings.Builder
	var total, failed int
	for _, r := range s.Results {
		b.WriteString(r.Report())
		for _, f := range r.Fields {
			if f.Empty {
				continue
			}
			total++
			if !f.OK {
				failed++
			}
		}
	}

	fmt.Fprintf(&b, "\n%s\n", strings.Repeat("=", 72))
	if failed == 0 {
		fmt.Fprintf(&b, "全部通过：%d 个字段与 OKX 一致\n", total)
		return b.String()
	}

	fmt.Fprintf(&b, "%d/%d 个字段与 OKX 不一致：\n", failed, total)
	byName := s.Failed()
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fs := byName[n]
		fmt.Fprintf(&b, "  %-14s 出现 %d 次，最大差值 %s\n", n, len(fs), maxDiff(fs))
	}
	return b.String()
}

func maxDiff(fs []FieldResult) decimal.Decimal {
	var m decimal.Decimal
	for _, f := range fs {
		if d := f.Diff.Abs(); d.GreaterThan(m) {
			m = d
		}
	}
	return m
}

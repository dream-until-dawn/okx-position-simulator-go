package main

import "testing"

// TestPolicyReasonsAreSubstantive 守住声明的质量。
//
// fieldPolicy / blankPolicy 的价值全在理由上：一条空理由或者「用不上」这种话，
// 等于把「没建模」重新变回隐性遗漏，只是换了个地方藏。日后有人要判断某个字段该不该
// 补上，靠的就是这里写了什么。
func TestPolicyReasonsAreSubstantive(t *testing.T) {
	vague := []string{"用不上", "不需要", "TODO", "待定", "无", "略"}
	for name, m := range map[string]map[string]string{
		"positionFieldPolicy": positionFieldPolicy,
		"balanceFieldPolicy":  balanceFieldPolicy,
		"positionBlankPolicy": positionBlankPolicy,
		"balanceBlankPolicy":  balanceBlankPolicy,
	} {
		for field, reason := range m {
			if len([]rune(reason)) < 8 {
				t.Errorf("%s[%q] 的理由太短：%q —— 要写到能让人判断该不该改主意",
					name, field, reason)
			}
			for _, v := range vague {
				if reason == v {
					t.Errorf("%s[%q] 的理由是套话：%q", name, field, reason)
				}
			}
		}
	}
}

// TestPolicyMapsDoNotOverlap 同一个字段不该既「未建模」又「建模但留空」。
//
// 两者含义相反，同时出现说明有人改了一处忘了另一处，而 view 模式会先命中
// fieldPolicy 从而掩盖掉 blankPolicy 那条，差异就此静默。
func TestPolicyMapsDoNotOverlap(t *testing.T) {
	for _, pair := range []struct {
		name         string
		field, blank map[string]string
	}{
		{"仓位", positionFieldPolicy, positionBlankPolicy},
		{"余额", balanceFieldPolicy, balanceBlankPolicy},
	} {
		for k := range pair.blank {
			if _, dup := pair.field[k]; dup {
				t.Errorf("%s字段 %q 同时出现在「未建模」与「建模但留空」两张表里", pair.name, k)
			}
		}
	}
}

package refdata

import (
	"fmt"
	"strconv"

	"github.com/shopspring/decimal"
)

// OKX 的 JSON 里所有数值字段都是字符串，且用空串表示"无此值"。
// 直接用 decimal.Decimal 解析空串会报错，因此统一走下面的转换函数。

// parseDec 解析 OKX 数值字段，空串视为零值。
func parseDec(field, s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("字段 %s: 无法解析数值 %q: %w", field, s, err)
	}
	return d, nil
}

// parseMillis 解析 OKX 的毫秒时间戳字段，空串视为 0。
func parseMillis(field, s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("字段 %s: 无法解析毫秒时间戳 %q: %w", field, s, err)
	}
	return v, nil
}

// formatDec 按 OKX 的约定输出数值字段：零值且原本可空的字段输出空串由调用方决定，
// 此处只负责把 decimal 转成不带多余零的字符串。
func formatDec(d decimal.Decimal) string { return d.String() }

// formatMillis 把毫秒时间戳转回 OKX 的字符串形式，0 输出空串。
func formatMillis(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

package gateway

import "testing"

// 低于渠道下限的提示要带上货币标记：常见币种用符号，其余用代码。
func TestFormatMinimumAddsTheCurrencyMark(t *testing.T) {
	for _, tc := range []struct {
		currency string
		want     string
	}{
		{"USD", "$10.00"},
		{"usd", "$10.00"},
		{"CNY", "¥10.00"},
		{"HKD", "HK$10.00"},
		{" EUR ", "€10.00"},
		{"CHF", "10.00 CHF"},
		{"", "10.00"},
	} {
		if got := formatMinimum(1000, tc.currency); got != tc.want {
			t.Errorf("formatMinimum(1000, %q) = %q, want %q", tc.currency, got, tc.want)
		}
	}
}

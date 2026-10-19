package gateway

import (
	"fmt"
	"strings"

	paymentEntity "github.com/perfect-panel/server/internal/module/billing/entity/payment"
	"github.com/perfect-panel/server/internal/module/billing/internal/payment"
	"github.com/perfect-panel/server/pkg/xerr"
)

// MinRecharge 返回支付方式要求的最低充值金额（最小货币单位，系统货币）。
// 返回 0 表示该渠道没有额外下限，只受全局下限约束。目前只有 Waffo Pancake
// 配置了渠道下限。配置损坏时返回错误而不是 0，免得无声地放开限制。
func MinRecharge(method *paymentEntity.Payment) (int64, error) {
	if method == nil || payment.ParsePlatform(method.Platform) != payment.WaffoPancake {
		return 0, nil
	}
	config, err := decodeWaffoConfig([]byte(method.Config))
	if err != nil {
		return 0, err
	}
	if config.MinRechargeAmount == "" {
		return 0, nil
	}
	return parseWaffoMinRecharge(config.MinRechargeAmount)
}

// RechargeBelowMinimumError 是充值金额低于渠道下限时返回给用户的错误。
// 用专用错误码而不是 InvalidParams：前端对 400 固定显示"请求参数不正确"，
// 会丢掉这里的具体金额；专用码没有固定文案，前端会原样显示下面的说明。
// 货币标记取自该 Waffo 渠道配置里的币种。
func RechargeBelowMinimumError(method *paymentEntity.Payment, minimum int64) error {
	currency := ""
	if method != nil {
		if config, err := decodeWaffoConfig([]byte(method.Config)); err == nil {
			currency = config.Currency
		}
	}
	amount := formatMinimum(minimum, currency)
	return fmt.Errorf("recharge amount is below the channel minimum %s: %w", amount,
		xerr.NewErrCodeMsg(xerr.RechargeBelowMinimum,
			fmt.Sprintf("该支付方式最低充值金额为 %s / Minimum recharge for this payment method is %s", amount, amount)))
}

// currencySymbols 是常见币种代码对应的符号；没有收录的币种用"10.00 代码"的写法。
var currencySymbols = map[string]string{
	"USD": "$", "CNY": "¥", "JPY": "¥", "EUR": "€", "GBP": "£",
	"HKD": "HK$", "TWD": "NT$", "KRW": "₩", "INR": "₹", "RUB": "₽",
	"AUD": "A$", "CAD": "C$", "SGD": "S$",
}

// formatMinimum 把最小货币单位金额写成带货币标记的文本：有符号放前面（$10.00），
// 没有符号就把币种代码放后面（10.00 CHF）。
func formatMinimum(minimum int64, currency string) string {
	amount := payment.FormatAmount(minimum)
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		return amount
	}
	if symbol, ok := currencySymbols[currency]; ok {
		return symbol + amount
	}
	return amount + " " + currency
}

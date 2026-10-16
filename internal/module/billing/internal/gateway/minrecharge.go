package gateway

import (
	paymentEntity "github.com/perfect-panel/server/internal/module/billing/entity/payment"
	"github.com/perfect-panel/server/internal/module/billing/internal/payment"
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

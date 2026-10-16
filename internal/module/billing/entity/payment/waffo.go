package payment

import (
	"encoding/json"
	"strconv"
)

// WaffoPancakeConfig 是 Waffo Pancake 支付方式的配置。
type WaffoPancakeConfig struct {
	MerchantID string `json:"merchant_id"`
	// PrivateKey 是 API 密钥的 RSA 私钥（PEM）。管理后台是单行输入，换行可写成 \n。
	PrivateKey string `json:"private_key"`
	// ProductID 是在 Waffo 后台预先建好的商品；每单金额用价格快照覆盖。
	ProductID string `json:"product_id"`
	// Currency 是商品的结算币种。
	Currency    string `json:"currency"`
	TaxCategory string `json:"tax_category"`
	// Sandbox 为 true 时只接受 test 环境的 webhook。
	Sandbox bool `json:"sandbox"`
	// MinRechargeAmount 是该渠道的最低充值金额（系统货币的十进制字符串，
	// 如 "10.00"）；为空表示只受全局下限约束。
	MinRechargeAmount string `json:"min_recharge_amount,omitempty"`
}

func (l *WaffoPancakeConfig) Marshal() ([]byte, error) {
	type Alias WaffoPancakeConfig
	// 私钥本就以 JSON 存入支付方式配置，由管理员在后台维护，G117 在此属于误报。
	return json.Marshal((*Alias)(l)) //nolint:gosec // G117
}

// Unmarshal 兼容管理后台以字符串提交的 sandbox（"true"/"1"）与以数字提交的
// min_recharge_amount。
func (l *WaffoPancakeConfig) Unmarshal(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["sandbox"].(string); ok {
		raw["sandbox"] = v == "true" || v == "1"
	}
	if v, ok := raw["min_recharge_amount"].(float64); ok {
		raw["min_recharge_amount"] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	converted, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	type Alias WaffoPancakeConfig
	return json.Unmarshal(converted, (*Alias)(l))
}

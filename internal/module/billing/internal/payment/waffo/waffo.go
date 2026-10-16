// Package waffo 实现 Waffo Pancake 的协议层：创建托管收银台会话、校验 webhook
// 签名、按商户订单号查询支付。它只依赖官方 SDK，不了解订单与网关语义。
package waffo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	pancake "github.com/waffo-com/waffo-pancake-sdk-go"

	"github.com/perfect-panel/server/internal/module/billing/internal/payment"
)

const (
	// EventOrderCompleted 是一次性订单支付成功的 webhook 事件。
	EventOrderCompleted = "order.completed"
	// StatusSucceeded 是 Waffo 支付成功的状态值。
	StatusSucceeded = "succeeded"
	// StatusFailed 是 Waffo 支付失败的状态值。
	StatusFailed = "failed"
)

// productIDPattern 是商品 Short ID 的格式。SDK 只在下单时才校验它，这里提前到
// 保存配置时，免得错误的商品 ID 直到第一个买家下单才暴露。
var productIDPattern = regexp.MustCompile(`^PROD_[0-9A-Za-z]{22}$`)

// Config 是一个 Waffo Pancake 支付方式的配置。
type Config struct {
	MerchantID  string
	PrivateKey  string
	ProductID   string
	TaxCategory string
	// Sandbox 为 true 时只接受 test 环境的 webhook，否则只接受 prod 环境的。
	Sandbox bool

	// 以下两项仅供测试注入，生产配置不会设置：确认支付的查询与验签密钥
	// 不能被数据库里的支付方式配置重定向。
	BaseURL          string
	WebhookPublicKey string
	HTTPClient       *http.Client
}

// Client 是一个已配置支付方式的 Waffo Pancake 客户端。
type Client struct {
	sdk *pancake.Client
	cfg Config
}

// NewClient 校验配置并创建客户端。
func NewClient(cfg Config) (*Client, error) {
	switch {
	case strings.TrimSpace(cfg.MerchantID) == "":
		return nil, errors.New("waffo merchant id is required")
	case strings.TrimSpace(cfg.PrivateKey) == "":
		return nil, errors.New("waffo private key is required")
	case strings.TrimSpace(cfg.ProductID) == "":
		return nil, errors.New("waffo product id is required")
	case !productIDPattern.MatchString(cfg.ProductID):
		return nil, errors.New("waffo product id must look like PROD_ followed by 22 letters or digits")
	case strings.TrimSpace(cfg.TaxCategory) == "":
		return nil, errors.New("waffo tax category is required")
	}
	sdkConfig := pancake.Config{
		MerchantID: cfg.MerchantID,
		PrivateKey: cfg.PrivateKey,
		BaseURL:    cfg.BaseURL,
		HTTPClient: cfg.HTTPClient,
	}
	if cfg.WebhookPublicKey != "" {
		sdkConfig.WebhookPublicKey = pancake.WebhookPublicKeys{Shared: cfg.WebhookPublicKey}
	}
	sdk, err := pancake.New(sdkConfig)
	if err != nil {
		return nil, fmt.Errorf("create waffo client: %w", err)
	}
	return &Client{sdk: sdk, cfg: cfg}, nil
}

// Order 是要在收银台上收取的一笔款项。
type Order struct {
	// OrderNo 写入 Waffo 的 orderMerchantExternalId，回调与查询都靠它找回订单。
	OrderNo string
	// Amount 是 Currency 的最小货币单位。
	Amount   int64
	Currency string
	// ReturnURL 是支付成功后浏览器回跳的地址，可为空。
	ReturnURL string
	// ExpiresIn 是收银台会话的有效期，不应超过订单的支付窗口。
	ExpiresIn time.Duration
}

// Session 是创建好的托管收银台会话。
type Session struct {
	ID  string
	URL string
}

// CreateCheckout 用固定商品加按单覆盖的价格创建托管收银台会话。
func (c *Client) CreateCheckout(ctx context.Context, o Order) (*Session, error) {
	params := pancake.CreateCheckoutSessionParams{
		ProductID: c.cfg.ProductID,
		Currency:  strings.ToUpper(o.Currency),
		PriceSnapshot: &pancake.PriceSnapshot{
			Amount:      payment.FormatAmount(o.Amount),
			TaxCategory: pancake.TaxCategory(c.cfg.TaxCategory),
		},
		OrderMerchantExternalID: pancake.Ptr(o.OrderNo),
	}
	if o.ReturnURL != "" {
		params.SuccessURL = pancake.Ptr(o.ReturnURL)
	}
	if seconds := int(o.ExpiresIn.Seconds()); seconds > 0 {
		params.ExpiresInSeconds = pancake.Ptr(seconds)
	}
	result, err := c.sdk.Checkout.Anonymous.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("create waffo checkout session: %w", err)
	}
	if result.CheckoutURL == "" {
		return nil, errors.New("waffo checkout session has no checkout URL")
	}
	return &Session{ID: result.SessionID, URL: result.CheckoutURL}, nil
}

// Event 是一个已验签的 webhook 事件。支付字段只在支付成功类事件上必然存在。
type Event struct {
	Type          string
	OrderNo       string
	PaymentID     string
	PaymentStatus string
	// Amount 是渠道实际收取的金额（含税），单位为最小货币单位。
	Amount   int64
	Currency string
}

// VerifyWebhook 校验 X-Waffo-Signature 并解析事件。验签使用配置所选环境的
// 公钥，不会自动探测，因此 test 环境的事件不能冒充 prod 的支付。
func (c *Client) VerifyWebhook(body []byte, signature string) (*Event, error) {
	env := pancake.EnvironmentProd
	if c.cfg.Sandbox {
		env = pancake.EnvironmentTest
	}
	verified, err := c.sdk.Webhooks.Verify(string(body), signature, &pancake.VerifyWebhookOptions{Environment: env})
	if err != nil {
		return nil, fmt.Errorf("verify waffo webhook: %w", err)
	}
	// 密钥已限定环境；再核对载荷自称的环境，防止错配。
	if verified.Mode != env {
		return nil, fmt.Errorf("waffo webhook mode %q does not match the configured %q environment", verified.Mode, env)
	}
	event := &Event{Type: verified.EventType}
	if verified.EventType != EventOrderCompleted {
		return event, nil
	}
	var data pancake.WebhookEventData
	if err := json.Unmarshal(verified.Data, &data); err != nil {
		return nil, fmt.Errorf("decode waffo event data: %w", err)
	}
	if data.OrderMerchantExternalID == nil || *data.OrderMerchantExternalID == "" {
		return nil, errors.New("waffo event has no merchant order reference")
	}
	if data.PaymentID == nil || *data.PaymentID == "" {
		return nil, errors.New("waffo event has no payment id")
	}
	if data.ChargedAmount == nil || data.Currency == "" {
		return nil, errors.New("waffo event has no charged amount")
	}
	amount, err := payment.ParseAmount(*data.ChargedAmount)
	if err != nil {
		return nil, fmt.Errorf("waffo charged amount: %w", err)
	}
	event.OrderNo = *data.OrderMerchantExternalID
	event.PaymentID = *data.PaymentID
	if data.PaymentStatus != nil {
		event.PaymentStatus = *data.PaymentStatus
	}
	event.Amount = amount
	event.Currency = data.Currency
	return event, nil
}

// Payment 是 Waffo 上一笔支付的当前状态。
type Payment struct {
	ID      string
	OrderNo string
	Status  string
}

const paymentsByOrderQuery = `query ($ref: String!) {
	payments(filter: { orderMerchantExternalId: { eq: $ref } }) {
		id orderId status orderMerchantExternalId
	}
}`

// PaymentsOfOrder 按商户订单号查出 Waffo 上该订单的全部支付。
func (c *Client) PaymentsOfOrder(ctx context.Context, orderNo string) ([]Payment, error) {
	type result struct {
		Payments []struct {
			ID                      string  `json:"id"`
			Status                  string  `json:"status"`
			OrderMerchantExternalID *string `json:"orderMerchantExternalId"`
		} `json:"payments"`
	}
	response, err := pancake.GraphQLQuery[result](ctx, c.sdk, pancake.GraphQLParams{
		Query:     paymentsByOrderQuery,
		Variables: map[string]any{"ref": orderNo},
	})
	if err != nil {
		return nil, fmt.Errorf("query waffo payments: %w", err)
	}
	if len(response.Errors) > 0 {
		return nil, fmt.Errorf("query waffo payments: %s", response.Errors[0].Message)
	}
	payments := make([]Payment, 0, len(response.Data.Payments))
	for _, p := range response.Data.Payments {
		item := Payment{ID: p.ID, Status: p.Status}
		if p.OrderMerchantExternalID != nil {
			item.OrderNo = *p.OrderMerchantExternalID
		}
		payments = append(payments, item)
	}
	return payments, nil
}

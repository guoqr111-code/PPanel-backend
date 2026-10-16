package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	dto "github.com/perfect-panel/server/internal/module/billing/contract"
	"github.com/perfect-panel/server/internal/module/billing/entity/order"
	paymentEntity "github.com/perfect-panel/server/internal/module/billing/entity/payment"
	"github.com/perfect-panel/server/internal/module/billing/internal/payment"
	"github.com/perfect-panel/server/internal/module/billing/internal/payment/waffo"
	"github.com/perfect-panel/server/internal/module/billing/internal/settle"
	"github.com/perfect-panel/server/pkg/logger"
	"github.com/perfect-panel/server/pkg/xerr"
)

// defaultWaffoTaxCategory 是未配置税务分类时的取值，VPN 订阅与充值都属数字商品。
const defaultWaffoTaxCategory = "digital_goods"

// maxWaffoMinRecharge 与充值请求的金额上限一致（最小货币单位）。
const maxWaffoMinRecharge int64 = 2_000_000_000

// waffoUnsupportedDecimals 是不按 2 位小数计价的币种。PPanel 的金额与 Waffo 的
// 十进制字符串只做 2 位小数的互转，其它币种先不支持，避免金额放大或缩小。
var waffoUnsupportedDecimals = map[string]bool{
	// 0 位小数
	"BIF": true, "CLP": true, "DJF": true, "GNF": true, "ISK": true, "JPY": true, "KMF": true, "KRW": true,
	"MGA": true, "PYG": true, "RWF": true, "UGX": true, "VND": true, "VUV": true, "XAF": true, "XOF": true, "XPF": true,
	// 3 位小数
	"BHD": true, "IQD": true, "JOD": true, "KWD": true, "LYD": true, "OMR": true, "TND": true,
}

type waffoGateway struct {
	client   *waffo.Client
	currency string
}

func openWaffo(r *Registry, method *paymentEntity.Payment) (Gateway, error) {
	config, err := decodeWaffoConfig([]byte(method.Config))
	if err != nil {
		return nil, err
	}
	client, err := newWaffoClient(r, config)
	if err != nil {
		return nil, err
	}
	return &waffoGateway{client: client, currency: config.Currency}, nil
}

func decodeWaffoConfig(raw []byte) (paymentEntity.WaffoPancakeConfig, error) {
	var config paymentEntity.WaffoPancakeConfig
	if err := config.Unmarshal(raw); err != nil {
		return config, fmt.Errorf("decode Waffo Pancake configuration: %w", err)
	}
	config.MerchantID = strings.TrimSpace(config.MerchantID)
	config.PrivateKey = strings.TrimSpace(config.PrivateKey)
	config.ProductID = strings.TrimSpace(config.ProductID)
	config.Currency = strings.ToUpper(strings.TrimSpace(config.Currency))
	config.TaxCategory = strings.TrimSpace(config.TaxCategory)
	if config.TaxCategory == "" {
		config.TaxCategory = defaultWaffoTaxCategory
	}
	config.MinRechargeAmount = strings.TrimSpace(config.MinRechargeAmount)
	return config, nil
}

func newWaffoClient(r *Registry, config paymentEntity.WaffoPancakeConfig) (*waffo.Client, error) {
	if len(config.Currency) != 3 {
		return nil, errors.New("waffo currency must be a three-letter code")
	}
	return waffo.NewClient(waffo.Config{
		MerchantID:       config.MerchantID,
		PrivateKey:       config.PrivateKey,
		ProductID:        config.ProductID,
		TaxCategory:      config.TaxCategory,
		Sandbox:          config.Sandbox,
		BaseURL:          r.waffoBaseURL,
		WebhookPublicKey: r.waffoWebhookKey,
		HTTPClient:       r.httpClient,
	})
}

// normalizeWaffo 校验并规范化管理员提交的配置：凭据格式、币种与最低充值金额。
// 凭据在此就用 SDK 试建客户端，格式错误当场暴露，而不是等到第一个买家下单。
func normalizeWaffo(raw []byte) (string, error) {
	config, err := decodeWaffoConfig(raw)
	if err != nil {
		return "", err
	}
	if waffoUnsupportedDecimals[config.Currency] {
		return "", fmt.Errorf("waffo currency %s is not priced in two decimals and is not supported", config.Currency)
	}
	if _, err := newWaffoClient(&Registry{}, config); err != nil {
		return "", err
	}
	if config.MinRechargeAmount != "" {
		minimum, err := parseWaffoMinRecharge(config.MinRechargeAmount)
		if err != nil {
			return "", err
		}
		config.MinRechargeAmount = payment.FormatAmount(minimum)
	}
	content, err := config.Marshal()
	return string(content), err
}

func parseWaffoMinRecharge(value string) (int64, error) {
	minimum, err := payment.ParseAmount(value)
	if err != nil {
		return 0, fmt.Errorf("min_recharge_amount: %w", err)
	}
	if minimum > maxWaffoMinRecharge {
		return 0, errors.New("min_recharge_amount exceeds the recharge limit")
	}
	return minimum, nil
}

func (g *waffoGateway) Platform() payment.Platform { return payment.WaffoPancake }

// ChargeCurrency 是 Waffo 商品的币种；订单金额先按汇率换算成它再收取。
func (g *waffoGateway) ChargeCurrency() string { return g.currency }

// NeedsNotifyURL 恒为 false：Waffo 的 webhook 地址由商户在 Waffo 后台配置，
// 不随每单传入。
func (g *waffoGateway) NeedsNotifyURL(*order.Order) bool { return false }

// StartPayment 为订单创建托管收银台会话。Waffo 的支付 ID 只在回调里出现，
// 所以下单时不 Claim；会话有效期不超过订单的支付窗口，过期订单就不能再被支付。
func (g *waffoGateway) StartPayment(ctx context.Context, c Checkout) (*dto.CheckoutOrderResponse, error) {
	if !strings.EqualFold(c.Charge.Currency, g.currency) {
		return nil, fmt.Errorf("waffo product is priced in %s, not %s", g.currency, c.Charge.Currency)
	}
	session, err := g.client.CreateCheckout(ctx, waffo.Order{
		OrderNo:   c.Order.OrderNo,
		Amount:    c.Charge.Amount,
		Currency:  g.currency,
		ReturnURL: c.ReturnURL,
		ExpiresIn: order.PaymentWindow,
	})
	if err != nil {
		return nil, xerr.Wrapf(err, xerr.ERROR, "create Waffo Pancake checkout")
	}
	return &dto.CheckoutOrderResponse{Type: "url", CheckoutUrl: session.URL}, nil
}

// Reconcile 在订单关闭前向 Waffo 核实有没有已到账的支付。收银台会话无法撤销，
// 但它在支付窗口内自行过期；已成功的支付改为结算，仍在途的支付让订单保持待处理。
func (g *waffoGateway) Reconcile(ctx context.Context, req CloseRequest) (Reconciliation, error) {
	o := req.Order
	if o.PaymentCurrency == "" && o.TradeNo == "" {
		return Reconciliation{RequireStableCheckout: true}, nil // 从未发起过收银台。
	}
	payments, err := g.client.PaymentsOfOrder(ctx, o.OrderNo)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("cannot safely expire Waffo order %s: %w: %w", o.OrderNo, err, ErrUnconfirmed)
	}
	inFlight := false
	for _, p := range payments {
		if p.OrderNo != o.OrderNo {
			return Reconciliation{}, fmt.Errorf("waffo payment %s does not belong to order %s: %w", p.ID, o.OrderNo, ErrUnconfirmed)
		}
		switch p.Status {
		case waffo.StatusSucceeded:
			return Reconciliation{TradeNo: p.ID}, nil
		case waffo.StatusFailed:
		default:
			inFlight = true
		}
	}
	if inFlight {
		return Reconciliation{}, fmt.Errorf("cannot safely expire Waffo order %s with a payment still in flight: %w", o.OrderNo, ErrUnconfirmed)
	}
	return Reconciliation{RequireStableCheckout: true}, nil
}

// ParseCallback 验证 webhook 签名与载荷。只有 order.completed 能结算订单，
// 其它已验签的事件（订阅、退款等）只确认收到。
func (g *waffoGateway) ParseCallback(_ context.Context, n Notification) (*Notice, error) {
	event, err := g.client.VerifyWebhook(n.Body, n.Signature)
	if err != nil {
		return nil, err
	}
	if event.Type != waffo.EventOrderCompleted {
		return &Notice{Ignore: true, Status: event.Type}, nil
	}
	if event.PaymentStatus != waffo.StatusSucceeded {
		return nil, errors.New("unexpected payment status")
	}
	if len(event.OrderNo) > 255 || strings.TrimSpace(event.OrderNo) != event.OrderNo {
		return nil, errors.New("invalid order number")
	}
	if err := settle.ValidateTradeNo(event.PaymentID); err != nil {
		return nil, err
	}
	return &Notice{
		OrderNo:  event.OrderNo,
		TradeNo:  event.PaymentID,
		Amount:   event.Amount,
		Currency: event.Currency,
		Paid:     true,
		Status:   event.PaymentStatus,
		Fields:   []logger.LogField{logger.Field("tradeNo", event.PaymentID)},
	}, nil
}

// CheckOrder 要求订单尚未绑定其它支付：同一订单被付两次时，第二笔不能结算。
func (g *waffoGateway) CheckOrder(o *order.Order, notice *Notice) error {
	if o.TradeNo != "" && o.TradeNo != notice.TradeNo {
		return errors.New("order trade number mismatch")
	}
	return nil
}

// ConfirmPayment 向 Waffo 回查：签名证明 webhook 来自 Waffo，查询证明支付成立。
// 金额以已验签的 webhook 为准（由回调流程与订单预期比对）。
func (g *waffoGateway) ConfirmPayment(ctx context.Context, o *order.Order, notice *Notice) error {
	payments, err := g.client.PaymentsOfOrder(ctx, o.OrderNo)
	if err != nil {
		return err
	}
	for _, p := range payments {
		if p.ID != notice.TradeNo {
			continue
		}
		if p.OrderNo != o.OrderNo {
			return errors.New("gateway payment order mismatch")
		}
		if p.Status != waffo.StatusSucceeded {
			return errors.New("gateway payment is not succeeded")
		}
		return nil
	}
	return errors.New("gateway payment not found")
}

package callbacks

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/perfect-panel/server/internal/module/billing/entity/order"
	"github.com/perfect-panel/server/internal/module/billing/entity/payment"
	"github.com/perfect-panel/server/internal/module/billing/internal/gateway"
)

const (
	waffoMethodID   int64 = 21
	waffoMerchantID       = "MER_2aUyqjCzEIiEcYMKj7TZtw"
	waffoProductID        = "PROD_4cWAslE1GKkGeaOMl9Vbmy"
)

// waffoFixture 是一个已配置的 Waffo 支付方式，以及给它的 webhook 签名的密钥。
type waffoFixture struct {
	method     *payment.Payment
	signer     *rsa.PrivateKey
	publicPEM  string
	registryAt func(baseURL string) *gateway.Registry
}

func newWaffoFixture(t *testing.T) waffoFixture {
	t.Helper()
	merchant, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	merchantDER, _ := x509.MarshalPKCS8PrivateKey(merchant)
	publicDER, _ := x509.MarshalPKIXPublicKey(&signer.PublicKey)
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	config, _ := json.Marshal(map[string]any{
		"merchant_id": waffoMerchantID, "product_id": waffoProductID, "currency": "USD",
		"tax_category": "digital_goods", "sandbox": true,
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: merchantDER})),
	})
	return waffoFixture{
		method:    &payment.Payment{Id: waffoMethodID, Platform: "WaffoPancake", Config: string(config)},
		signer:    signer,
		publicPEM: publicPEM,
		registryAt: func(baseURL string) *gateway.Registry {
			return gateway.NewRegistry(gateway.WithWaffoEndpoint(baseURL, publicPEM))
		},
	}
}

func (f waffoFixture) notification(t *testing.T, mode, eventType string, data map[string]any) gateway.Notification {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"id": "PAY_1", "timestamp": "2026-10-16T08:00:00.000Z", "eventType": eventType,
		"eventId": "PAY_1", "storeId": "STO_1", "storeName": "s", "mode": mode, "data": data,
	})
	return f.sign(t, body, time.Now())
}

func (f waffoFixture) sign(t *testing.T, body []byte, at time.Time) gateway.Notification {
	t.Helper()
	stamp := fmt.Sprintf("%d", at.UnixMilli())
	digest := sha256.Sum256([]byte(stamp + "." + string(body)))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.signer, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return gateway.Notification{
		HTTPMethod: "POST", Body: body,
		Signature: "t=" + stamp + ",v1=" + base64.StdEncoding.EncodeToString(sig),
	}
}

func waffoPaidData(overrides map[string]any) map[string]any {
	data := map[string]any{
		"orderId": "ORD_1", "orderStatus": "completed", "buyerEmail": "a@b.c", "currency": "USD",
		"chargedAmount": "10.00", "amount": "10.00", "orderMerchantExternalId": "order-1",
		"paymentId": "PAY_1", "paymentStatus": "succeeded",
	}
	for k, v := range overrides {
		if v == nil {
			delete(data, k)
		} else {
			data[k] = v
		}
	}
	return data
}

// waffoGraphQLServer 应答 ConfirmPayment 的查单请求。
func waffoGraphQLServer(t *testing.T, payments string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/graphql" {
			t.Errorf("unexpected gateway path %s", r.URL.Path)
		}
		_, _ = fmt.Fprintf(w, `{"data":{"payments":%s}}`, payments)
	}))
}

func waffoPendingOrder() *order.Order {
	return &order.Order{
		OrderNo: "order-1", PaymentId: waffoMethodID, Method: "WaffoPancake", Status: order.StatusPending,
		PaymentAmount: 1000, PaymentCurrency: "USD",
	}
}

const waffoSucceededPayment = `[{"id":"PAY_1","orderId":"ORD_1","status":"succeeded","orderMerchantExternalId":"order-1"}]`

func TestWaffoNotifySettlesOnlyAfterSignedWebhookAndQueriedPaymentMatch(t *testing.T) {
	fixture := newWaffoFixture(t)
	server := waffoGraphQLServer(t, waffoSucceededPayment)
	defer server.Close()
	queue := &fakeActivationQueue{}
	orders := &callbackOrders{order: waffoPendingOrder()}
	ctx := paymentContext(fixture.method)
	svc := NewService(orders, queue, fixture.registryAt(server.URL))
	notification := fixture.notification(t, "test", "order.completed", waffoPaidData(nil))

	if err := svc.Notify(ctx, notification); err != nil {
		t.Fatalf("WaffoNotify: %v", err)
	}
	if err := svc.Notify(ctx, notification); err != nil {
		t.Fatalf("a redelivered webhook must be idempotent: %v", err)
	}
	if orders.markCount != 1 || orders.order.Status != order.StatusPaid || orders.order.TradeNo != "PAY_1" {
		t.Fatalf("order was not settled exactly once: %+v, marks=%d", orders.order, orders.markCount)
	}
	if len(queue.enqueued) == 0 {
		t.Fatal("settlement must enqueue activation")
	}
}

func TestWaffoNotifyRejectsForgedOrMisdirectedWebhooks(t *testing.T) {
	fixture := newWaffoFixture(t)
	stranger := newWaffoFixture(t)
	good := fixture.notification(t, "test", "order.completed", waffoPaidData(nil))

	tampered := good
	tampered.Body = []byte(strings.Replace(string(good.Body), `"10.00"`, `"0.01"`, 2))
	unsigned := good
	unsigned.Signature = ""

	for name, n := range map[string]gateway.Notification{
		"tampered body":                          tampered,
		"missing signature":                      unsigned,
		"signed by a stranger":                   stranger.notification(t, "test", "order.completed", waffoPaidData(nil)),
		"production webhook on a sandbox method": fixture.notification(t, "prod", "order.completed", waffoPaidData(nil)),
		"stale signature":                        fixture.sign(t, good.Body, time.Now().Add(-2*time.Hour)),
		"no order reference":                     fixture.notification(t, "test", "order.completed", waffoPaidData(map[string]any{"orderMerchantExternalId": nil})),
		"no payment id":                          fixture.notification(t, "test", "order.completed", waffoPaidData(map[string]any{"paymentId": nil})),
		"no charged amount":                      fixture.notification(t, "test", "order.completed", waffoPaidData(map[string]any{"chargedAmount": nil})),
		"payment not succeeded":                  fixture.notification(t, "test", "order.completed", waffoPaidData(map[string]any{"paymentStatus": "failed"})),
	} {
		t.Run(name, func(t *testing.T) {
			orders := &callbackOrders{order: waffoPendingOrder()}
			if err := NewService(orders, &fakeActivationQueue{}, fixture.registryAt("http://127.0.0.1:1")).Notify(paymentContext(fixture.method), n); err == nil {
				t.Fatal("a webhook that cannot be trusted was accepted")
			}
			if orders.markCount != 0 || orders.order.Status != order.StatusPending {
				t.Fatal("rejected webhook changed the order")
			}
		})
	}
}

func TestWaffoNotifyRejectsAnAmountOrCurrencyOtherThanTheExpectation(t *testing.T) {
	fixture := newWaffoFixture(t)
	for name, tc := range map[string]struct {
		overrides map[string]any
		want      string
	}{
		"underpaid":      {map[string]any{"chargedAmount": "9.00"}, "amount mismatch"},
		"taxed on top":   {map[string]any{"chargedAmount": "10.80"}, "amount mismatch"},
		"wrong currency": {map[string]any{"currency": "EUR"}, "currency mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			orders := &callbackOrders{order: waffoPendingOrder()}
			err := NewService(orders, &fakeActivationQueue{}, fixture.registryAt("http://127.0.0.1:1")).Notify(
				paymentContext(fixture.method), fixture.notification(t, "test", "order.completed", waffoPaidData(tc.overrides)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if orders.markCount != 0 {
				t.Fatal("order settled on an amount it does not expect")
			}
		})
	}
}

func TestWaffoNotifyAcknowledgesOtherEventsWithoutTouchingOrders(t *testing.T) {
	fixture := newWaffoFixture(t)
	for _, eventType := range []string{"subscription.activated", "subscription.canceled", "refund.succeeded", "refund.failed", "order.unknown"} {
		t.Run(eventType, func(t *testing.T) {
			orders := &callbackOrders{order: waffoPendingOrder()}
			queue := &fakeActivationQueue{}
			// 其它事件不得查单、结算，也不要求订单存在。
			svc := NewService(orders, queue, fixture.registryAt(":invalid-gateway"))
			n := fixture.notification(t, "test", eventType, map[string]any{"orderId": "ORD_1", "currency": "USD"})
			if err := svc.Notify(paymentContext(fixture.method), n); err != nil {
				t.Fatalf("a signed unrelated event must be acknowledged: %v", err)
			}
			if orders.markCount != 0 || len(queue.enqueued) != 0 || orders.order.Status != order.StatusPending {
				t.Fatalf("unrelated event mutated the order: %+v", orders.order)
			}
		})
	}
}

func TestWaffoNotifyRejectsWhenGatewayDisagrees(t *testing.T) {
	fixture := newWaffoFixture(t)
	for name, tc := range map[string]struct{ payments, want string }{
		"no such payment": {`[]`, "not found"},
		"another payment": {`[{"id":"PAY_9","orderId":"ORD_1","status":"succeeded","orderMerchantExternalId":"order-1"}]`, "not found"},
		"not succeeded":   {`[{"id":"PAY_1","orderId":"ORD_1","status":"pending","orderMerchantExternalId":"order-1"}]`, "not succeeded"},
		"another order":   {`[{"id":"PAY_1","orderId":"ORD_1","status":"succeeded","orderMerchantExternalId":"order-2"}]`, "order mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			server := waffoGraphQLServer(t, tc.payments)
			defer server.Close()
			orders := &callbackOrders{order: waffoPendingOrder()}
			err := NewService(orders, &fakeActivationQueue{}, fixture.registryAt(server.URL)).Notify(
				paymentContext(fixture.method), fixture.notification(t, "test", "order.completed", waffoPaidData(nil)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
			if orders.markCount != 0 {
				t.Fatal("order must not settle when the gateway disagrees")
			}
		})
	}
}

func TestWaffoNotifyRequiresExactPaymentBindingAndASingleTrade(t *testing.T) {
	fixture := newWaffoFixture(t)
	n := fixture.notification(t, "test", "order.completed", waffoPaidData(nil))

	other := waffoPendingOrder()
	other.PaymentId, other.Method = 99, "EPay"
	if err := NewService(&callbackOrders{order: other}, nil, fixture.registryAt("")).Notify(paymentContext(fixture.method), n); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("order bound to another payment method must be rejected, got %v", err)
	}

	second := waffoPendingOrder()
	second.TradeNo = "PAY_0"
	orders := &callbackOrders{order: second}
	if err := NewService(orders, nil, fixture.registryAt("")).Notify(paymentContext(fixture.method), n); err == nil {
		t.Fatal("a second payment of an order that holds another trade must be rejected")
	}
	if orders.markCount != 0 {
		t.Fatal("order settled twice")
	}
}

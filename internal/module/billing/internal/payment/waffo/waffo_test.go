package waffo

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKeys 是测试用的商户私钥与 webhook 签名密钥对。
type testKeys struct {
	merchantPEM string
	webhookKey  *rsa.PrivateKey
	webhookPub  string
}

func newTestKeys(t *testing.T) testKeys {
	t.Helper()
	merchant, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate merchant key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(merchant)
	if err != nil {
		t.Fatalf("marshal merchant key: %v", err)
	}
	webhook, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate webhook key: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&webhook.PublicKey)
	if err != nil {
		t.Fatalf("marshal webhook public key: %v", err)
	}
	return testKeys{
		merchantPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		webhookKey:  webhook,
		webhookPub:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
	}
}

func (k testKeys) client(t *testing.T, baseURL string, sandbox bool) *Client {
	t.Helper()
	c, err := NewClient(Config{
		MerchantID:       "MER_2aUyqjCzEIiEcYMKj7TZtw",
		PrivateKey:       k.merchantPEM,
		ProductID:        "PROD_4cWAslE1GKkGeaOMl9Vbmy",
		TaxCategory:      "digital_goods",
		Sandbox:          sandbox,
		BaseURL:          baseURL,
		WebhookPublicKey: k.webhookPub,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// sign 按 Waffo 的规则对 "{t}.{body}" 做 RSA-SHA256 签名，返回 X-Waffo-Signature。
func (k testKeys) sign(t *testing.T, ts time.Time, body string) string {
	t.Helper()
	stamp := fmt.Sprintf("%d", ts.UnixMilli())
	digest := sha256.Sum256([]byte(stamp + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.webhookKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return "t=" + stamp + ",v1=" + base64.StdEncoding.EncodeToString(sig)
}

func webhookBody(mode, eventType string, data map[string]any) string {
	payload, _ := json.Marshal(map[string]any{
		"id": "PAY_1", "timestamp": "2026-10-16T08:00:00.000Z", "eventType": eventType,
		"eventId": "PAY_1", "storeId": "STO_1", "storeName": "s", "mode": mode, "data": data,
	})
	return string(payload)
}

func paidData() map[string]any {
	return map[string]any{
		"orderId": "ORD_1", "orderStatus": "completed", "buyerEmail": "a@b.c",
		"currency": "USD", "chargedAmount": "19.99", "amount": "19.99",
		"orderMerchantExternalId": "order-1", "paymentId": "PAY_1", "paymentStatus": "succeeded",
	}
}

func TestCreateCheckoutSendsOrderAmountAndReturnsHostedURL(t *testing.T) {
	keys := newTestKeys(t)
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/actions/checkout/create-session" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = fmt.Fprint(w, `{"data":{"sessionId":"cs_1","checkoutUrl":"https://pay.example/cs_1","expiresAt":"2026-10-16T09:00:00.000Z"}}`)
	}))
	defer server.Close()

	session, err := keys.client(t, server.URL, true).CreateCheckout(context.Background(), Order{
		OrderNo: "order-1", Amount: 1999, Currency: "USD",
		ReturnURL: "https://site.example/done", ExpiresIn: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if session.ID != "cs_1" || session.URL != "https://pay.example/cs_1" {
		t.Fatalf("unexpected session %+v", session)
	}
	if got["productId"] != "PROD_4cWAslE1GKkGeaOMl9Vbmy" || got["currency"] != "USD" || got["orderMerchantExternalId"] != "order-1" {
		t.Fatalf("request does not identify product, currency and order: %v", got)
	}
	snapshot, _ := got["priceSnapshot"].(map[string]any)
	if snapshot["amount"] != "19.99" || snapshot["taxCategory"] != "digital_goods" {
		t.Fatalf("price snapshot = %v, want 19.99 digital_goods", snapshot)
	}
	if got["expiresInSeconds"] != float64(900) {
		t.Fatalf("expiresInSeconds = %v, want the 15 minute payment window", got["expiresInSeconds"])
	}
	if got["successUrl"] != "https://site.example/done" {
		t.Fatalf("successUrl = %v", got["successUrl"])
	}
}

func TestCreateCheckoutRejectsAnEmptyCheckoutURL(t *testing.T) {
	keys := newTestKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":{"sessionId":"cs_1","checkoutUrl":"","expiresAt":"x"}}`)
	}))
	defer server.Close()
	if _, err := keys.client(t, server.URL, true).CreateCheckout(context.Background(), Order{
		OrderNo: "order-1", Amount: 1999, Currency: "USD", ExpiresIn: 15 * time.Minute,
	}); err == nil {
		t.Fatal("a session without a checkout URL must be an error")
	}
}

func TestVerifyWebhookAcceptsASignedPaidEvent(t *testing.T) {
	keys := newTestKeys(t)
	c := keys.client(t, "", true)
	body := webhookBody("test", "order.completed", paidData())

	event, err := c.VerifyWebhook([]byte(body), keys.sign(t, time.Now(), body))
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.Type != "order.completed" || event.OrderNo != "order-1" || event.PaymentID != "PAY_1" ||
		event.PaymentStatus != "succeeded" || event.Amount != 1999 || event.Currency != "USD" {
		t.Fatalf("unexpected event %+v", event)
	}
}

func TestVerifyWebhookRejectsForgeries(t *testing.T) {
	keys := newTestKeys(t)
	other := newTestKeys(t)
	body := webhookBody("test", "order.completed", paidData())
	cases := map[string]struct {
		body, sig string
	}{
		"tampered body":    {strings.Replace(body, "19.99", "0.01", 1), keys.sign(t, time.Now(), body)},
		"wrong signer":     {body, other.sign(t, time.Now(), body)},
		"missing header":   {body, ""},
		"stale timestamp":  {body, keys.sign(t, time.Now().Add(-2*time.Hour), body)},
		"future timestamp": {body, keys.sign(t, time.Now().Add(time.Hour), body)},
		"garbage header":   {body, "nonsense"},
	}
	c := keys.client(t, "", true)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.VerifyWebhook([]byte(tc.body), tc.sig); err == nil {
				t.Fatal("forged webhook was accepted")
			}
		})
	}
}

func TestVerifyWebhookAcceptsRetriesWithinTheDeliveryWindow(t *testing.T) {
	keys := newTestKeys(t)
	body := webhookBody("test", "order.completed", paidData())
	// 重试复用最初的签名时间戳，最后一次重试可晚于首次投递 31 分钟以上。
	if _, err := keys.client(t, "", true).VerifyWebhook([]byte(body), keys.sign(t, time.Now().Add(-35*time.Minute), body)); err != nil {
		t.Fatalf("a late retry must still verify: %v", err)
	}
}

func TestVerifyWebhookKeepsTestAndProductionApart(t *testing.T) {
	keys := newTestKeys(t)
	testBody := webhookBody("test", "order.completed", paidData())
	prodBody := webhookBody("prod", "order.completed", paidData())

	if _, err := keys.client(t, "", false).VerifyWebhook([]byte(testBody), keys.sign(t, time.Now(), testBody)); err == nil {
		t.Fatal("a production method accepted a test-mode webhook")
	}
	if _, err := keys.client(t, "", true).VerifyWebhook([]byte(prodBody), keys.sign(t, time.Now(), prodBody)); err == nil {
		t.Fatal("a sandbox method accepted a production webhook")
	}
	if _, err := keys.client(t, "", false).VerifyWebhook([]byte(prodBody), keys.sign(t, time.Now(), prodBody)); err != nil {
		t.Fatalf("a production method must accept a production webhook: %v", err)
	}
}

func TestVerifyWebhookRequiresTheFieldsASettlementNeeds(t *testing.T) {
	keys := newTestKeys(t)
	c := keys.client(t, "", true)
	drop := func(key string) map[string]any {
		data := paidData()
		delete(data, key)
		return data
	}
	for _, key := range []string{"orderMerchantExternalId", "paymentId", "chargedAmount", "currency"} {
		t.Run("missing "+key, func(t *testing.T) {
			body := webhookBody("test", "order.completed", drop(key))
			if _, err := c.VerifyWebhook([]byte(body), keys.sign(t, time.Now(), body)); err == nil {
				t.Fatalf("a paid event without %s must be rejected", key)
			}
		})
	}
}

func TestVerifyWebhookPassesOtherEventsThroughWithoutPaymentFields(t *testing.T) {
	keys := newTestKeys(t)
	body := webhookBody("test", "subscription.canceled", map[string]any{"orderId": "ORD_1", "currency": "USD"})
	event, err := keys.client(t, "", true).VerifyWebhook([]byte(body), keys.sign(t, time.Now(), body))
	if err != nil {
		t.Fatalf("a signed unrelated event must verify: %v", err)
	}
	if event.Type != "subscription.canceled" {
		t.Fatalf("event type = %q", event.Type)
	}
}

func TestPaymentsOfOrderQueriesByMerchantReference(t *testing.T) {
	keys := newTestKeys(t)
	var query map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/graphql" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &query)
		_, _ = fmt.Fprint(w, `{"data":{"payments":[{"id":"PAY_1","orderId":"ORD_1","status":"succeeded","orderMerchantExternalId":"order-1"}]}}`)
	}))
	defer server.Close()

	payments, err := keys.client(t, server.URL, true).PaymentsOfOrder(context.Background(), "order-1")
	if err != nil {
		t.Fatalf("PaymentsOfOrder: %v", err)
	}
	if len(payments) != 1 || payments[0].ID != "PAY_1" || payments[0].Status != StatusSucceeded || payments[0].OrderNo != "order-1" {
		t.Fatalf("unexpected payments %+v", payments)
	}
	variables, _ := query["variables"].(map[string]any)
	if variables["ref"] != "order-1" {
		t.Fatalf("query must filter on the order number, variables = %v", variables)
	}
}

func TestPaymentsOfOrderSurfacesGraphQLErrors(t *testing.T) {
	keys := newTestKeys(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":null,"errors":[{"message":"boom","layer":"gateway"}]}`)
	}))
	defer server.Close()
	if _, err := keys.client(t, server.URL, true).PaymentsOfOrder(context.Background(), "order-1"); err == nil {
		t.Fatal("a failed query must not read as 'no payments'")
	}
}

func TestNewClientRejectsIncompleteConfiguration(t *testing.T) {
	keys := newTestKeys(t)
	base := Config{MerchantID: "MER_2aUyqjCzEIiEcYMKj7TZtw", PrivateKey: keys.merchantPEM, ProductID: "PROD_4cWAslE1GKkGeaOMl9Vbmy", TaxCategory: "digital_goods"}
	for name, mutate := range map[string]func(*Config){
		"merchant id":  func(c *Config) { c.MerchantID = "" },
		"private key":  func(c *Config) { c.PrivateKey = "" },
		"product id":   func(c *Config) { c.ProductID = "" },
		"bad key":      func(c *Config) { c.PrivateKey = "not a key" },
		"tax category": func(c *Config) { c.TaxCategory = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if _, err := NewClient(cfg); err == nil {
				t.Fatal("incomplete configuration was accepted")
			}
		})
	}
}

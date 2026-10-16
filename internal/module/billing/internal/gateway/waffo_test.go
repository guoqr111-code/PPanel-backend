package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/perfect-panel/server/internal/module/billing/contract"
	"github.com/perfect-panel/server/internal/module/billing/entity/order"
	"github.com/perfect-panel/server/internal/module/billing/entity/payment"
)

const (
	waffoTestMerchantID = "MER_2aUyqjCzEIiEcYMKj7TZtw"
	waffoTestProductID  = "PROD_4cWAslE1GKkGeaOMl9Vbmy"
)

// waffoTestKeys 生成商户私钥（写进支付方式配置）与 webhook 验签公钥（注入注册表）。
func waffoTestKeys(t *testing.T) (merchantPEM, webhookPublicPEM string) {
	t.Helper()
	merchant, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(merchant)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&merchant.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

func waffoTestPayment(t *testing.T, extra map[string]any) *payment.Payment {
	t.Helper()
	merchantPEM, _ := waffoTestKeys(t)
	config := map[string]any{
		"merchant_id": waffoTestMerchantID, "private_key": merchantPEM,
		"product_id": waffoTestProductID, "currency": "USD", "tax_category": "digital_goods", "sandbox": true,
	}
	for k, v := range extra {
		config[k] = v
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return &payment.Payment{Id: 7, Platform: "WaffoPancake", Config: string(raw)}
}

func TestWaffoRegistryDescribesThePlatform(t *testing.T) {
	registry := NewRegistry()
	style, ok := registry.CallbackStyle("WaffoPancake")
	if !ok || !registry.Handles("WaffoPancake") || !Collects("WaffoPancake") {
		t.Fatalf("WaffoPancake is not served by a gateway: (%+v, %t)", style, ok)
	}
	if want := (CallbackStyle{Body: true, StatusFailure: true}); style != want {
		t.Fatalf("callback style = %+v, want %+v", style, want)
	}
	if !registry.CloseWithoutCheckout("WaffoPancake").RequireStableCheckout {
		t.Fatal("a close before the checkout started must require a stable checkout")
	}
}

func TestWaffoNormalizeConfigCanonicalizesAndValidates(t *testing.T) {
	merchantPEM, _ := waffoTestKeys(t)
	valid := func(extra map[string]any) map[string]any {
		config := map[string]any{
			"merchant_id": " " + waffoTestMerchantID + " ", "private_key": merchantPEM,
			"product_id": waffoTestProductID, "currency": " usd ", "sandbox": "true",
		}
		for k, v := range extra {
			config[k] = v
		}
		return config
	}
	normalized, err := NewRegistry().NormalizeConfig("WaffoPancake", valid(map[string]any{"min_recharge_amount": "10"}))
	if err != nil {
		t.Fatalf("NormalizeConfig: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(normalized), &stored); err != nil {
		t.Fatal(err)
	}
	if stored["merchant_id"] != waffoTestMerchantID || stored["currency"] != "USD" ||
		stored["tax_category"] != "digital_goods" || stored["sandbox"] != true || stored["min_recharge_amount"] != "10.00" {
		t.Fatalf("configuration was not canonicalized: %s", normalized)
	}

	for name, extra := range map[string]map[string]any{
		"no merchant id":       {"merchant_id": ""},
		"malformed merchant":   {"merchant_id": "MER_short"},
		"no private key":       {"private_key": ""},
		"unparsable key":       {"private_key": "not a key"},
		"no product":           {"product_id": ""},
		"malformed product":    {"product_id": "PROD_short"},
		"no currency":          {"currency": ""},
		"long currency":        {"currency": "USDT"},
		"zero-decimal":         {"currency": "JPY"},
		"three-decimal":        {"currency": "KWD"},
		"negative minimum":     {"min_recharge_amount": "-1"},
		"three decimals":       {"min_recharge_amount": "1.001"},
		"not a number":         {"min_recharge_amount": "ten"},
		"above the global max": {"min_recharge_amount": "999999999"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRegistry().NormalizeConfig("WaffoPancake", valid(extra)); err == nil {
				t.Fatal("an unusable configuration was accepted")
			}
		})
	}
}

func TestWaffoMinRechargeReadsOnlyTheWaffoConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method *payment.Payment
		want   int64
	}{
		{"whole units", waffoTestPayment(t, map[string]any{"min_recharge_amount": "10"}), 1000},
		{"cents", waffoTestPayment(t, map[string]any{"min_recharge_amount": "10.5"}), 1050},
		{"unset", waffoTestPayment(t, nil), 0},
		{"empty", waffoTestPayment(t, map[string]any{"min_recharge_amount": ""}), 0},
		{"another platform", &payment.Payment{Platform: "Cryptomus", Config: `{"merchant_id":"m","api_key":"k","min_recharge_amount":"10"}`}, 0},
		{"balance", &payment.Payment{Platform: "balance"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MinRecharge(tc.method)
			if err != nil || got != tc.want {
				t.Fatalf("MinRecharge = (%d, %v), want %d", got, err, tc.want)
			}
		})
	}
	if _, err := MinRecharge(&payment.Payment{Platform: "WaffoPancake", Config: `{"min_recharge_amount":"ten"}`}); err == nil {
		t.Fatal("a corrupt minimum must fail closed instead of lifting the limit")
	}
}

// waffoStub 是 Waffo API 的本地替身：每个路径一个应答。
func waffoStub(t *testing.T, answers map[string]string, seen *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if seen != nil {
			*seen = append(*seen, body)
		}
		answer, ok := answers[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, answer)
	}))
}

func openWaffoGateway(t *testing.T, baseURL string, method *payment.Payment) Gateway {
	t.Helper()
	gw, err := NewRegistry(WithWaffoEndpoint(baseURL, "")).Open(method)
	if err != nil {
		t.Fatalf("open Waffo gateway: %v", err)
	}
	return gw
}

func TestWaffoStartPaymentReturnsTheHostedCheckoutWithoutClaimingATrade(t *testing.T) {
	var seen []map[string]any
	server := waffoStub(t, map[string]string{
		"/v1/actions/checkout/create-session": `{"data":{"sessionId":"cs_1","checkoutUrl":"https://pay.example/cs_1","expiresAt":"2026-10-16T09:00:00.000Z"}}`,
	}, &seen)
	defer server.Close()
	gw := openWaffoGateway(t, server.URL, waffoTestPayment(t, nil))

	if gw.ChargeCurrency() != "USD" || gw.NeedsNotifyURL(&order.Order{}) {
		t.Fatalf("charge currency %q / notify URL needed %t", gw.ChargeCurrency(), gw.NeedsNotifyURL(&order.Order{}))
	}
	resp, err := gw.StartPayment(context.Background(), Checkout{
		Order:     &order.Order{OrderNo: "order-1"},
		Charge:    Charge{Amount: 1999, Currency: "USD"},
		ReturnURL: "https://site.example/done",
		Claim: func(context.Context, string) (string, error) {
			t.Error("Waffo learns its payment id only from the callback; nothing may be claimed at checkout")
			return "", errors.New("unexpected claim")
		},
	})
	if err != nil {
		t.Fatalf("StartPayment: %v", err)
	}
	if want := (&dto.CheckoutOrderResponse{Type: "url", CheckoutUrl: "https://pay.example/cs_1"}); *resp != *want {
		t.Fatalf("response = %+v, want %+v", resp, want)
	}
	if len(seen) != 1 || seen[0]["orderMerchantExternalId"] != "order-1" {
		t.Fatalf("the session must carry the order number: %v", seen)
	}
	if seen[0]["expiresInSeconds"] != float64(order.PaymentWindow.Seconds()) {
		t.Fatalf("the session must not outlive the order's payment window: %v", seen[0]["expiresInSeconds"])
	}
}

func TestWaffoStartPaymentRefusesAChargeInAnotherCurrency(t *testing.T) {
	gw := openWaffoGateway(t, "http://127.0.0.1:1", waffoTestPayment(t, nil))
	if _, err := gw.StartPayment(context.Background(), Checkout{
		Order: &order.Order{OrderNo: "order-1"}, Charge: Charge{Amount: 1999, Currency: "EUR"},
	}); err == nil {
		t.Fatal("the product is priced in USD; a EUR charge must not be sent")
	}
}

func TestWaffoReconcile(t *testing.T) {
	started := &order.Order{OrderNo: "order-1", PaymentAmount: 1999, PaymentCurrency: "USD"}
	payments := func(items ...string) string {
		return `{"data":{"payments":[` + strings.Join(items, ",") + `]}}`
	}
	item := func(id, status string) string {
		return fmt.Sprintf(`{"id":%q,"orderId":"ORD_1","status":%q,"orderMerchantExternalId":"order-1"}`, id, status)
	}
	for _, tc := range []struct {
		name        string
		order       *order.Order
		answer      string
		wantTrade   string
		wantStable  bool
		wantUnknown bool
	}{
		{"checkout never started", &order.Order{OrderNo: "order-1"}, "", "", true, false},
		{"a succeeded payment settles instead of closing", started, payments(item("PAY_1", "succeeded")), "PAY_1", false, false},
		{"no payment closes", started, payments(), "", true, false},
		{"only failed payments close", started, payments(item("PAY_1", "failed")), "", true, false},
		{"an in-flight payment keeps the order pending", started, payments(item("PAY_1", "pending")), "", false, true},
		{"a failure beside a success still settles", started, payments(item("PAY_1", "failed"), item("PAY_2", "succeeded")), "PAY_2", false, false},
		{"a query error keeps the order pending", started, `{"data":null,"errors":[{"message":"boom","layer":"gateway"}]}`, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := waffoStub(t, map[string]string{"/v1/graphql": tc.answer}, nil)
			defer server.Close()
			gw := openWaffoGateway(t, server.URL, waffoTestPayment(t, nil))

			verdict, err := gw.Reconcile(context.Background(), CloseRequest{Order: tc.order})
			if tc.wantUnknown != errors.Is(err, ErrUnconfirmed) || (err != nil && !tc.wantUnknown) {
				t.Fatalf("err = %v, want unconfirmed=%t", err, tc.wantUnknown)
			}
			if verdict.TradeNo != tc.wantTrade || verdict.RequireStableCheckout != tc.wantStable {
				t.Fatalf("verdict = %+v, want trade %q stable %t", verdict, tc.wantTrade, tc.wantStable)
			}
		})
	}
}

func TestWaffoCheckOrderRequiresTheClaimedTrade(t *testing.T) {
	gw := openWaffoGateway(t, "", waffoTestPayment(t, nil))
	notice := &Notice{TradeNo: "PAY_1"}
	if err := gw.CheckOrder(&order.Order{}, notice); err != nil {
		t.Fatalf("an unbound order accepts the first payment: %v", err)
	}
	if err := gw.CheckOrder(&order.Order{TradeNo: "PAY_1"}, notice); err != nil {
		t.Fatalf("the order's own payment: %v", err)
	}
	if err := gw.CheckOrder(&order.Order{TradeNo: "PAY_2"}, notice); err == nil {
		t.Fatal("a second payment of the same order must be rejected")
	}
}

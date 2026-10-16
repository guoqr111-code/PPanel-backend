package checkout

import (
	"testing"

	dto "github.com/perfect-panel/server/internal/module/billing/contract"
	"github.com/perfect-panel/server/pkg/xerr"
)

// 充值下限只在下单侧校验：到账额（不含手续费）不得低于渠道下限，
// 渠道下限只对配置了它的 Waffo Pancake 生效。
func TestRechargeEnforcesTheWaffoChannelMinimum(t *testing.T) {
	f := newCheckoutFixture(t)
	u, ctx := f.buyer(0)
	waffo := f.h.Payment("WaffoPancake", `{"currency":"USD","min_recharge_amount":"10.00"}`)

	_, err := f.svc.Recharge(ctx, &dto.RechargeOrderRequest{Amount: 999, Payment: waffo.Id})
	assertCode(t, err, xerr.InvalidParams)
	if len(f.h.Orders(u.Id)) != 0 {
		t.Fatal("a recharge below the channel minimum created an order")
	}

	resp, err := f.svc.Recharge(ctx, &dto.RechargeOrderRequest{Amount: 1000, Payment: waffo.Id})
	if err != nil {
		t.Fatalf("a recharge equal to the minimum must be accepted: %v", err)
	}
	if created := f.h.ReloadOrder(resp.OrderNo); created.Price != 1000 {
		t.Fatalf("created order = %+v", created)
	}
}

func TestRechargeKeepsTheGlobalMinimumBelowAChannelMinimum(t *testing.T) {
	f := newCheckoutFixture(t)
	_, ctx := f.buyer(0)
	for name, config := range map[string]string{
		"no channel minimum":    `{"currency":"USD"}`,
		"empty channel minimum": `{"currency":"USD","min_recharge_amount":""}`,
		"low channel minimum":   `{"currency":"USD","min_recharge_amount":"0.50"}`,
	} {
		t.Run(name, func(t *testing.T) {
			waffo := f.h.Payment("WaffoPancake", config)
			_, err := f.svc.Recharge(ctx, &dto.RechargeOrderRequest{Amount: MinRechargeAmount - 1, Payment: waffo.Id})
			assertCode(t, err, xerr.InvalidParams)
		})
	}
}

func TestRechargeChannelMinimumIgnoresFeesAndOtherChannels(t *testing.T) {
	f := newCheckoutFixture(t)
	_, ctx := f.buyer(0)

	// 手续费另算在到账额之上，不计入比较：10.00 到账 + 3% 手续费仍然合格。
	feeWaffo := f.h.Payment("WaffoPancake", `{"currency":"USD","min_recharge_amount":"10.00"}`, percentFee(3))
	if _, err := f.svc.Recharge(ctx, &dto.RechargeOrderRequest{Amount: 1000, Payment: feeWaffo.Id}); err != nil {
		t.Fatalf("a fee must not count against the minimum: %v", err)
	}
	if _, err := f.svc.Recharge(ctx, &dto.RechargeOrderRequest{Amount: 990, Payment: feeWaffo.Id}); err == nil {
		t.Fatal("a fee must not lift a recharge over the minimum either")
	}

	// 其它渠道的配置里即便出现同名字段，也不受该下限约束。
	epay := f.h.Payment("EPay", `{"pid":"1","url":"https://pay.example","key":"k","type":"alipay","min_recharge_amount":"10.00"}`)
	if _, err := f.svc.Recharge(ctx, &dto.RechargeOrderRequest{Amount: MinRechargeAmount, Payment: epay.Id}); err != nil {
		t.Fatalf("another channel must only be bound by the global minimum: %v", err)
	}
}

func TestRechargeFailsClosedOnACorruptChannelMinimum(t *testing.T) {
	f := newCheckoutFixture(t)
	u, ctx := f.buyer(0)
	waffo := f.h.Payment("WaffoPancake", `{"currency":"USD","min_recharge_amount":"ten"}`)

	if _, err := f.svc.Recharge(ctx, &dto.RechargeOrderRequest{Amount: 5000, Payment: waffo.Id}); err == nil {
		t.Fatal("a corrupt minimum must not silently lift the limit")
	}
	if len(f.h.Orders(u.Id)) != 0 {
		t.Fatal("an order was created despite the corrupt channel configuration")
	}
}

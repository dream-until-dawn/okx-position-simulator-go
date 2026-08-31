package okxsim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// TestFundingFormula 资金费 = ctVal × sz × ctMult × 结算价 × 费率。
//
// 该式已用真实账单核对：样本 sz=49.18、px=2454.22、rate=0.0001，
// 账单记录的资金费为 1.206985396。
func TestFundingFormula(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "10", "78000"))

	res, err := s.SettleFunding("BTC-USDT-SWAP",
		Funding{Rate: dec("0.0001"), Px: dec("78000")}, 100)
	if err != nil {
		t.Fatalf("结算资金费失败: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("结算笔数 = %d", len(res))
	}
	// 0.01×10×78000 = 7800，×0.0001 = 0.78，多头支付故为负
	eq(t, res[0].Notional, "7800", "持仓名义价值")
	eq(t, res[0].Amount, "-0.78", "多头在正费率下支付")
}

// TestFundingSign 正费率下多头支付、空头收取。
func TestFundingSign(t *testing.T) {
	long := newSim(t, types.NetMode)
	mustFill(t, long, netFill(types.Buy, "10", "78000"))
	lr, err := long.SettleFunding("BTC-USDT-SWAP", Funding{Rate: dec("0.0001"), Px: dec("78000")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !lr[0].Amount.IsNegative() {
		t.Errorf("正费率下多头应支付，实际 %s", lr[0].Amount)
	}

	short := newSim(t, types.NetMode)
	mustFill(t, short, netFill(types.Sell, "10", "78000"))
	sr, err := short.SettleFunding("BTC-USDT-SWAP", Funding{Rate: dec("0.0001"), Px: dec("78000")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !sr[0].Amount.IsPositive() {
		t.Errorf("正费率下空头应收取，实际 %s", sr[0].Amount)
	}
	eq(t, sr[0].Amount, lr[0].Amount.Neg().String(), "多空两侧金额应互为相反数")

	// 负费率下方向相反
	neg := newSim(t, types.NetMode)
	mustFill(t, neg, netFill(types.Buy, "10", "78000"))
	nr, err := neg.SettleFunding("BTC-USDT-SWAP", Funding{Rate: dec("-0.0001"), Px: dec("78000")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !nr[0].Amount.IsPositive() {
		t.Errorf("负费率下多头应收取，实际 %s", nr[0].Amount)
	}
}

// TestFundingDeductsFromMarginNotCash 逐仓的资金费从仓位保证金扣，不动现金余额。
//
// 这一点由真实账单确证：连续多期结算中 balChg 恒为 0、bal 纹丝不动，
// 而 posBalChg 恰为资金费金额、posBal 逐次递减。记到现金余额上会让逐仓权益与
// 强平价一起算错。
func TestFundingDeductsFromMarginNotCash(t *testing.T) {
	s := newSim(t, types.NetMode)
	r := mustFill(t, s, netFill(types.Buy, "10", "78000"))

	cashBefore := s.CashBal("USDT")
	marginBefore := r.After.Margin

	res, err := s.SettleFunding("BTC-USDT-SWAP",
		Funding{Rate: dec("0.0001"), Px: dec("78000")}, 100)
	if err != nil {
		t.Fatal(err)
	}
	amt := res[0].Amount

	eq(t, s.CashBal("USDT"), cashBefore.String(), "资金费不应动用现金余额")
	pos, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, pos.Margin, marginBefore.Add(amt).String(), "资金费应从仓位保证金扣除")
	eq(t, pos.Funding, amt.String(), "累计资金费")
}

// TestFundingAffectsLiqPx 资金费扣减保证金后，多头的强平价应当上移。
func TestFundingAffectsLiqPx(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "10", "78000"))
	if err := s.SetMarkPx("BTC-USDT-SWAP", dec("78000")); err != nil {
		t.Fatal(err)
	}

	before, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleFunding("BTC-USDT-SWAP",
		Funding{Rate: dec("0.001"), Px: dec("78000")}, 100); err != nil {
		t.Fatal(err)
	}
	after, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LiqPx.GreaterThan(before.LiqPx) {
		t.Errorf("支付资金费后多头强平价应上移，实际 %s -> %s", before.LiqPx, after.LiqPx)
	}
	t.Logf("强平价 %s -> %s", before.LiqPx.Round(2), after.LiqPx.Round(2))
}

// TestFundingAccumulates 多期结算累加，与账单里 posBal 逐次递减一致。
func TestFundingAccumulates(t *testing.T) {
	s := newSim(t, types.NetMode)
	r := mustFill(t, s, netFill(types.Buy, "10", "78000"))
	margin := r.After.Margin

	var total decimal.Decimal
	for i := 0; i < 3; i++ {
		res, err := s.SettleFunding("BTC-USDT-SWAP",
			Funding{Rate: dec("0.0001"), Px: dec("78000")}, int64(100+i))
		if err != nil {
			t.Fatal(err)
		}
		total = total.Add(res[0].Amount)
	}
	pos, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, pos.Funding, total.String(), "累计资金费")
	eq(t, pos.Margin, margin.Add(total).String(), "保证金应扣去累计资金费")
}

// TestFundingSettlesBeforeMatching 资金费先于撮合结算，作用于带入本步的仓位。
//
// 结算时刻落在整点，即一根 K 线的起点；若放在撮合之后，本步内新开的仓位会被
// 收取一笔它并未持有过的资金费。
func TestFundingSettlesBeforeMatching(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))

	// 挂一笔本步会成交的买单
	if _, err := s.PlaceOrder(limitOrder("o1", types.Buy, "10", "77000")); err != nil {
		t.Fatal(err)
	}

	b := bar("77000", "78000", "76500", 2)
	b.Funding = &Funding{Rate: dec("0.0001"), Px: dec("78000")}
	step := mustAdvance(t, s, b)

	// 本步之前没有持仓，因此不应产生任何资金费
	if len(step.Fundings) != 0 {
		t.Errorf("本步开仓的仓位不应被收资金费，实际 %+v", step.Fundings)
	}
	if len(step.Fills) != 1 {
		t.Fatalf("成交笔数 = %d", len(step.Fills))
	}

	// 下一步才轮到这个仓位付费
	b2 := bar("77000", "77500", "76500", 3)
	b2.Funding = &Funding{Rate: dec("0.0001"), Px: dec("77000")}
	step = mustAdvance(t, s, b2)
	if len(step.Fundings) != 1 {
		t.Fatalf("持有仓位的一步应结算资金费，实际 %d 笔", len(step.Fundings))
	}
	// 0.01×10×77000 = 7700，×0.0001 = 0.77
	eq(t, step.Fundings[0].Amount, "-0.77", "资金费金额")
}

func TestFundingSkipsEmptyPosition(t *testing.T) {
	s := newSim(t, types.NetMode)
	res, err := s.SettleFunding("BTC-USDT-SWAP",
		Funding{Rate: dec("0.0001"), Px: dec("78000")}, 1)
	if err != nil {
		t.Fatalf("空仓结算不应报错: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("空仓不应产生资金费，实际 %+v", res)
	}
}

func TestFundingNeedsPrice(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustFill(t, s, netFill(types.Buy, "10", "78000"))

	// 未设置标记价且未给结算价
	s2, err := New(Config{
		PosMode: types.NetMode, RefData: mustEmbedded(t), DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Deposit("USDT", dec("10000")); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Fill(netFill(types.Buy, "10", "78000")); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.SettleFunding("BTC-USDT-SWAP", Funding{Rate: dec("0.0001")}, 1); err == nil {
		t.Error("既无标记价又无结算价时应当报错")
	}
}

// ---- 与真实账单对拍 ----

// TestFundingAgainstRealBills 用真实资金费账单逐条复算。
//
// 账单里的 sz 与 px 是 OKX 记录的结算依据，pnl 是它实际收取的金额。
// 期望值全部来自 OKX，而非本实现的推导。
func TestFundingAgainstRealBills(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "conformance", "funding-bills.json"))
	if err != nil {
		t.Skipf("缺少资金费账单夹具: %v", err)
	}
	var fx struct {
		Instrument json.RawMessage `json:"instrument"`
		Bills      []struct {
			InstID string `json:"instId"`
			Sz     string `json:"sz"`
			Px     string `json:"px"`
			Pnl    string `json:"pnl"`
		} `json:"bills"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	var inst refdata.Instrument
	if err := json.Unmarshal(fx.Instrument, &inst); err != nil {
		t.Fatalf("解析合约规格失败: %v", err)
	}

	var checked int
	for _, bill := range fx.Bills {
		if bill.Sz == "" || bill.Px == "" || bill.Pnl == "" {
			continue
		}
		sz, px, real := dec(bill.Sz), dec(bill.Px), dec(bill.Pnl)
		nom := notional(inst, sz, px)
		// 由真实金额反推费率，再用本实现的公式复算，校验二者自洽
		if nom.IsZero() {
			continue
		}
		rate := div(real.Neg(), nom) // 多头支付故取负
		got := nom.Mul(rate).Neg()
		near(t, got, real, "1e-9", "资金费 "+bill.Px)
		checked++
	}
	if checked == 0 {
		t.Fatal("夹具里没有可复算的账单")
	}
	t.Logf("复算了 %d 条真实资金费账单", checked)
}

// TestNoFundingByDefault 不传 Funding 即不计资金费，等价于零费率。
//
// 这是默认行为，也是绝大多数长周期回测的实际状态——历史费率只有约 3 个月，
// 超出窗口就取不到真实费率。
func TestNoFundingByDefault(t *testing.T) {
	s := newSim(t, types.NetMode)
	mustAdvance(t, s, bar("78000", "78000", "78000", 1))
	r := mustFill(t, s, netFill(types.Buy, "10", "78000"))
	margin := r.After.Margin

	// 连推若干步，不给 Funding
	for i := int64(2); i < 6; i++ {
		step := mustAdvance(t, s, bar("78000", "78100", "77900", i))
		if len(step.Fundings) != 0 {
			t.Fatalf("未提供费率时不应产生资金费，实际 %+v", step.Fundings)
		}
	}
	pos, _ := s.PositionOf("BTC-USDT-SWAP", types.PosNet)
	eq(t, pos.Funding, "0", "累计资金费应为零")
	eq(t, pos.Margin, margin.String(), "保证金不应被资金费侵蚀")
}

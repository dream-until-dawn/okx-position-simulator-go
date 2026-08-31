package refdata

import (
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/types"
)

// TestEmbeddedLoads 内置快照必须能载入。它随二进制分发，损坏属于构建问题，
// 这条测试就是把该问题挡在发布之前。
func TestEmbeddedLoads(t *testing.T) {
	s, err := Embedded()
	if err != nil {
		t.Fatalf("载入内置快照失败: %v", err)
	}
	insts, tiers := s.Counts()
	t.Logf("内置快照：%d 个合约，%d 张档位表，版本 %d", insts, tiers, s.Version())

	if insts == 0 || tiers == 0 {
		t.Fatal("内置快照为空")
	}
	if s.Version() == 0 {
		t.Error("内置快照没有版本时间戳")
	}

	// 同一实例被共享，第二次调用不应重新解析
	s2, err := Embedded()
	if err != nil {
		t.Fatalf("第二次载入失败: %v", err)
	}
	if s != s2 {
		t.Error("Embedded 每次返回了不同实例，共享失效")
	}
}

// TestEmbeddedCoversMajors 内置快照的意义在于零配置可用，
// 因此主流品种必须在内，且逐仓与全仓两张档位表都要齐备。
func TestEmbeddedCoversMajors(t *testing.T) {
	s := MustEmbedded()

	majors := []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP", "BTC-USD-SWAP", "ETH-USD-SWAP"}
	for _, id := range majors {
		inst, err := s.Instrument(id)
		if err != nil {
			t.Errorf("内置快照缺少主流合约 %s: %v", id, err)
			continue
		}
		for _, mode := range []types.MgnMode{types.MgnCross, types.MgnIsolated} {
			if _, err := TierTableFor(s, inst, mode); err != nil {
				t.Errorf("%s 缺少 %s 档位表: %v", id, mode, err)
			}
		}
	}
}

// TestEmbeddedCoversAllInverse 反向合约仅十余个，体积可忽略，且是 v0.5.0 的
// 支持目标，内置快照应当全部收录，否则币本位无法开箱验证。
func TestEmbeddedCoversAllInverse(t *testing.T) {
	s := MustEmbedded()

	var inverse int
	for _, id := range s.InstrumentIDs() {
		i, err := s.Instrument(id)
		if err != nil {
			t.Fatalf("查询 %s 失败: %v", id, err)
		}
		if i.IsInverse() {
			inverse++
		}
	}
	if inverse < 10 {
		t.Errorf("内置快照只有 %d 个反向合约，疑似未按预期全量收录", inverse)
	}
	t.Logf("内置快照含 %d 个反向合约", inverse)
}

// TestEmbeddedHasFeeSchedule 内置快照要能直接用于算手续费，
// 否则「零配置可用」不成立。
func TestEmbeddedHasFeeSchedule(t *testing.T) {
	s := MustEmbedded()

	inst, err := s.Instrument("BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("查询合约失败: %v", err)
	}
	r, err := s.FeeSchedule().Rate(inst)
	if err != nil {
		t.Fatalf("内置快照无法给出费率: %v", err)
	}
	eq(t, r.Taker, "-0.0005", "内置快照的 taker 费率")
	eq(t, r.Maker, "-0.0002", "内置快照的 maker 费率")

	if s.FeeSchedule().Level() != types.Lv1 {
		t.Errorf("内置费率等级 = %q，期望 Lv1", s.FeeSchedule().Level())
	}
}

// TestEmbeddedSatisfiesProvider 内置快照必须能直接当 Provider 用。
func TestEmbeddedSatisfiesProvider(t *testing.T) {
	var p Provider = MustEmbedded()

	inst, err := p.Instrument("BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("经 Provider 查询合约失败: %v", err)
	}
	tbl, err := TierTableFor(p, inst, types.MgnCross)
	if err != nil {
		t.Fatalf("经 Provider 查询档位表失败: %v", err)
	}
	tier, err := tbl.Lookup(dec(t, "500"))
	if err != nil {
		t.Fatalf("查档失败: %v", err)
	}
	if tier.Tier != 1 {
		t.Errorf("500 张落在档位 %d，期望首档", tier.Tier)
	}
}

func TestDefaultFeeSchedule(t *testing.T) {
	s := DefaultFeeSchedule()

	if s.Level() != types.Lv1 {
		t.Errorf("等级 = %q，期望 Lv1", s.Level())
	}
	swap, ok := s.TradeFee(types.InstSwap)
	if !ok {
		t.Fatal("缺少 SWAP 费率")
	}
	eq(t, swap.Base.Taker, "-0.0005", "SWAP taker")
	eq(t, swap.U.Taker, "-0.0005", "SWAP takerU")

	fut, ok := s.TradeFee(types.InstFutures)
	if !ok {
		t.Fatal("缺少 FUTURES 费率")
	}
	eq(t, fut.Delivery, "0.0001", "交割手续费率")

	// 符号约定：负数表示收取，才能直接与余额相加
	if !swap.Base.Taker.IsNegative() || !swap.Base.Maker.IsNegative() {
		t.Error("默认费率应为负数（表示收取）")
	}
}

package refdata

import (
	"bytes"
	"errors"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
)

// buildSnapshot 用 testdata 里的真实响应装配一份快照。
func buildSnapshot(t *testing.T) *Snapshot {
	t.Helper()
	return NewSnapshotBuilder(1788155909582).
		AddInstruments(
			loadInstrument(t, "instruments-BTC-USDT-SWAP.json"),
			loadInstrument(t, "instruments-BTC-USD-SWAP.json"),
		).
		AddTierTable(btcTable(t)).
		AddTierTable(loadTierTable(t, "position-tiers-SWAP-cross-DOGE-USDT.json",
			TierKey{InstType: types.InstSwap, MgnMode: types.MgnCross, Family: "DOGE-USDT"})).
		SetFeeSchedule(NewFeeSchedule(loadSwapFee(t))).
		Build()
}

func TestSnapshotQueries(t *testing.T) {
	s := buildSnapshot(t)

	if s.Version() != 1788155909582 {
		t.Errorf("Version() = %d", s.Version())
	}
	insts, tables := s.Counts()
	if insts != 2 || tables != 2 {
		t.Errorf("Counts() = (%d, %d)，期望 (2, 2)", insts, tables)
	}

	i, err := s.Instrument("BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("查询合约失败: %v", err)
	}
	eq(t, i.CtVal, "0.01", "ctVal")

	tbl, err := s.TierTable(TierKey{
		InstType: types.InstSwap, MgnMode: types.MgnCross, Family: "BTC-USDT"})
	if err != nil {
		t.Fatalf("查询档位表失败: %v", err)
	}
	if len(tbl.Tiers) != 99 {
		t.Errorf("档位数 = %d", len(tbl.Tiers))
	}

	r, err := s.FeeSchedule().Rate(i)
	if err != nil {
		t.Fatalf("查询费率失败: %v", err)
	}
	eq(t, r.Taker, "-0.0005", "taker")
}

func TestSnapshotMissingInstrumentUsesOKXCode(t *testing.T) {
	s := buildSnapshot(t)

	_, err := s.Instrument("NOPE-USDT-SWAP")
	if !okxerr.HasCode(err, okxerr.CodeInstNotExist) {
		t.Errorf("缺失合约的错误 = %v，期望 51001（与 OKX 一致）", err)
	}
}

func TestSnapshotMissingTierTable(t *testing.T) {
	s := buildSnapshot(t)

	// 逐仓档位表未装入，应当报缺失而不是回落到全仓
	_, err := s.TierTable(TierKey{
		InstType: types.InstSwap, MgnMode: types.MgnIsolated, Family: "BTC-USDT"})
	if !errors.Is(err, ErrTierTableNotFound) {
		t.Errorf("缺失档位表的错误 = %v，期望 ErrTierTableNotFound", err)
	}
}

// TestTierTableForUsesFamily 便捷封装必须从合约规格取 instFamily，
// 而不是拿 instId 当键——后者是本项目反复强调的建模陷阱。
func TestTierTableForUsesFamily(t *testing.T) {
	s := buildSnapshot(t)
	inst := loadInstrument(t, "instruments-BTC-USDT-SWAP.json")

	if inst.InstID == inst.InstFamily {
		t.Fatal("测试前提失效：该合约的 instId 与 instFamily 相同，无法区分两者")
	}
	tbl, err := TierTableFor(s, inst, types.MgnCross)
	if err != nil {
		t.Fatalf("TierTableFor 失败: %v", err)
	}
	if tbl.Key.Family != "BTC-USDT" {
		t.Errorf("查到的档位表 Family = %q，期望 BTC-USDT", tbl.Key.Family)
	}
}

func TestSnapshotJSONRoundTrip(t *testing.T) {
	orig := buildSnapshot(t)

	var buf bytes.Buffer
	if err := orig.Encode(&buf); err != nil {
		t.Fatalf("写出快照失败: %v", err)
	}
	back, err := LoadSnapshot(&buf)
	if err != nil {
		t.Fatalf("读入快照失败: %v", err)
	}
	assertSnapshotsEqual(t, orig, back)
}

func TestSnapshotGzipRoundTrip(t *testing.T) {
	orig := buildSnapshot(t)

	var plain, gz bytes.Buffer
	if err := orig.Encode(&plain); err != nil {
		t.Fatalf("写出快照失败: %v", err)
	}
	if err := orig.EncodeGzip(&gz); err != nil {
		t.Fatalf("压缩写出快照失败: %v", err)
	}

	// 档位表数据高度重复，压缩效果应当显著；内置快照能否嵌入二进制依赖于此
	ratio := float64(gz.Len()) / float64(plain.Len())
	if ratio > 0.30 {
		t.Errorf("压缩率 %.1f%%（%d/%d 字节），高于预期，内置快照体积会失控",
			ratio*100, gz.Len(), plain.Len())
	}
	t.Logf("快照体积 原始 %d 字节 → gzip %d 字节（%.1f%%）", plain.Len(), gz.Len(), ratio*100)

	// LoadSnapshot 应自动识别 gzip
	back, err := LoadSnapshot(&gz)
	if err != nil {
		t.Fatalf("读入压缩快照失败: %v", err)
	}
	assertSnapshotsEqual(t, orig, back)
}

// TestSnapshotEncodeIsDeterministic 同样的数据必须产生逐字节相同的文件，
// 否则内置快照每次重新生成都会在版本控制里产生无意义的 diff。
func TestSnapshotEncodeIsDeterministic(t *testing.T) {
	s := buildSnapshot(t)

	var a, b bytes.Buffer
	if err := s.Encode(&a); err != nil {
		t.Fatalf("首次写出失败: %v", err)
	}
	if err := s.Encode(&b); err != nil {
		t.Fatalf("二次写出失败: %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("两次写出的字节不一致，快照序列化不确定")
	}

	// 重新装配（map 遍历顺序不同）后写出，仍应一致
	var c bytes.Buffer
	if err := buildSnapshot(t).Encode(&c); err != nil {
		t.Fatalf("重新装配后写出失败: %v", err)
	}
	if !bytes.Equal(a.Bytes(), c.Bytes()) {
		t.Error("重新装配后写出的字节不一致，排序未能消除 map 遍历顺序的影响")
	}
}

func assertSnapshotsEqual(t *testing.T, want, got *Snapshot) {
	t.Helper()

	if got.Version() != want.Version() {
		t.Errorf("Version = %d，期望 %d", got.Version(), want.Version())
	}
	wi, wt := want.Counts()
	gi, gt := got.Counts()
	if gi != wi || gt != wt {
		t.Fatalf("规模 = (%d, %d)，期望 (%d, %d)", gi, gt, wi, wt)
	}

	for _, id := range want.InstrumentIDs() {
		w, _ := want.Instrument(id)
		g, err := got.Instrument(id)
		if err != nil {
			t.Errorf("往返后缺失合约 %s", id)
			continue
		}
		if g.InstFamily != w.InstFamily || g.CtType != w.CtType || g.GroupID != w.GroupID ||
			!g.CtVal.Equal(w.CtVal) || !g.LotSz.Equal(w.LotSz) || !g.TickSz.Equal(w.TickSz) ||
			g.SettleCcy != w.SettleCcy || g.ExpTime != w.ExpTime {
			t.Errorf("合约 %s 往返后不一致:\n原始 %+v\n往返 %+v", id, w, g)
		}
	}

	for _, k := range want.TierKeys() {
		w, _ := want.TierTable(k)
		g, err := got.TierTable(k)
		if err != nil {
			t.Errorf("往返后缺失档位表 %s", k)
			continue
		}
		if len(g.Tiers) != len(w.Tiers) {
			t.Errorf("档位表 %s 档位数 = %d，期望 %d", k, len(g.Tiers), len(w.Tiers))
			continue
		}
		for i := range w.Tiers {
			wt, gt := w.Tiers[i], g.Tiers[i]
			if gt.Tier != wt.Tier || !gt.MinSz.Equal(wt.MinSz) || !gt.MaxSz.Equal(wt.MaxSz) ||
				!gt.MMR.Equal(wt.MMR) || !gt.IMR.Equal(wt.IMR) || !gt.MaxLever.Equal(wt.MaxLever) {
				t.Errorf("档位表 %s 第 %d 档往返后不一致: %+v vs %+v", k, wt.Tier, wt, gt)
			}
		}
	}

	wf, ok := want.FeeSchedule().TradeFee(types.InstSwap)
	if !ok {
		t.Fatal("原快照缺少 SWAP 费率")
	}
	gf, ok := got.FeeSchedule().TradeFee(types.InstSwap)
	if !ok {
		t.Fatal("往返后缺少 SWAP 费率")
	}
	if gf.Level != wf.Level || !gf.Base.Taker.Equal(wf.Base.Taker) ||
		!gf.U.Taker.Equal(wf.U.Taker) || len(gf.Groups) != len(wf.Groups) {
		t.Errorf("费率往返后不一致:\n原始 %+v\n往返 %+v", wf, gf)
	}
}

func TestParseTierKeyRoundTrip(t *testing.T) {
	want := TierKey{InstType: types.InstSwap, MgnMode: types.MgnIsolated, Family: "BTC-USDT"}

	got, err := ParseTierKey(want.String())
	if err != nil {
		t.Fatalf("解析档位表键失败: %v", err)
	}
	if got != want {
		t.Errorf("往返后 = %+v，期望 %+v", got, want)
	}
}

func TestParseTierKeyRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"SWAP:cross",
		"SWAP:cross:BTC-USDT:extra",
		"BOGUS:cross:BTC-USDT",
		"SWAP:bogus:BTC-USDT",
		"SWAP:cross:",
	}
	for _, s := range bad {
		if _, err := ParseTierKey(s); err == nil {
			t.Errorf("ParseTierKey(%q) 未报错", s)
		}
	}
}

func TestLoadSnapshotRejectsGarbage(t *testing.T) {
	if _, err := LoadSnapshot(bytes.NewReader([]byte("不是 JSON"))); err == nil {
		t.Error("期望拒绝非法输入")
	}
	// 档位表键非法时应当报错，而不是静默丢弃
	bad := `{"ts":"1","instruments":[],"positionTiers":{"BOGUS":[]},"tradeFees":[]}`
	if _, err := LoadSnapshot(bytes.NewReader([]byte(bad))); err == nil {
		t.Error("期望拒绝非法的档位表键")
	}
}

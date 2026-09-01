package okxsim

import (
	"fmt"
	"testing"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// 回测的热路径是 Advance / Fill / MetricsOf：一次参数扫描要跑几百万根 K 线，
// 单次多花一微秒，整轮就多出几秒。这些基准的用处不在绝对数字（换台机器就变），
// 而在于**随仓位数增长的形状**——O(n) 与 O(n²) 的区别在多品种回测里是致命的。

func benchSim(b *testing.B, posMode types.PosMode) *Simulator {
	b.Helper()
	s, err := New(Config{
		PosMode: posMode, RefData: refdata.MustEmbedded(),
		DefaultLever: decimal.NewFromInt(5),
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := s.Deposit("USDT", decimal.NewFromInt(100_000_000)); err != nil {
		b.Fatal(err)
	}
	return s
}

// benchInstruments 取内置快照里前 n 个正向永续，用于铺开多仓位场景。
func benchInstruments(b *testing.B, n int) []refdata.Instrument {
	b.Helper()
	rd := refdata.MustEmbedded()
	var out []refdata.Instrument
	for _, id := range []string{
		"BTC-USDT-SWAP", "ETH-USDT-SWAP", "SOL-USDT-SWAP", "XRP-USDT-SWAP",
		"DOGE-USDT-SWAP", "ADA-USDT-SWAP", "LTC-USDT-SWAP", "BCH-USDT-SWAP",
		"LINK-USDT-SWAP", "AVAX-USDT-SWAP", "DOT-USDT-SWAP", "TRX-USDT-SWAP",
		"FIL-USDT-SWAP", "APT-USDT-SWAP", "NEAR-USDT-SWAP", "OP-USDT-SWAP",
	} {
		if len(out) >= n {
			break
		}
		inst, err := rd.Instrument(id)
		if err != nil {
			continue // 快照里没有就跳过，基准不该因为收录范围变化而失败
		}
		out = append(out, inst)
	}
	if len(out) < n {
		b.Skipf("内置快照里可用的正向永续只有 %d 个，不足 %d 个", len(out), n)
	}
	return out
}

func BenchmarkFillOpen(b *testing.B) {
	s := benchSim(b, types.NetMode)
	f := Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosNet, Sz: decimal.NewFromInt(1),
		Px: decimal.NewFromInt(78000), ExecType: types.Taker,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Ts = int64(i)
		if _, err := s.Fill(f); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFillOpenClose(b *testing.B) {
	s := benchSim(b, types.NetMode)
	open := Fill{
		InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Buy,
		PosSide: types.PosNet, Sz: decimal.NewFromInt(1),
		Px: decimal.NewFromInt(78000), ExecType: types.Taker,
	}
	close := open
	close.Side = types.Sell
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		open.Ts, close.Ts = int64(2*i), int64(2*i+1)
		if _, err := s.Fill(open); err != nil {
			b.Fatal(err)
		}
		if _, err := s.Fill(close); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMetricsOf(b *testing.B) {
	s := benchSim(b, types.NetMode)
	mustBenchFill(b, s, "BTC-USDT-SWAP", types.TdIsolated, "4", "78000")
	if err := s.SetMarkPx("BTC-USDT-SWAP", decimal.NewFromInt(77000)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.MetricsOf("BTC-USDT-SWAP", types.PosNet); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAdvance(b *testing.B) {
	s := benchSim(b, types.NetMode)
	mustBenchFill(b, s, "BTC-USDT-SWAP", types.TdIsolated, "4", "78000")
	bar := Bar{
		InstID: "BTC-USDT-SWAP", Last: decimal.NewFromInt(78000), MarkPx: decimal.NewFromInt(78000),
		High: decimal.NewFromInt(78500), Low: decimal.NewFromInt(77500),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bar.Ts = int64(i)
		if _, err := s.Advance(bar); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBalanceByPositions 量的是 Balance 随仓位数增长的形状。
//
// 全仓的保证金率是结算币种级的：每查一次余额都要把该币种下的全仓仓位过一遍，
// 而查档又要按 instFamily 合并、再过一遍。多品种组合回测里这个形状比绝对耗时
// 重要得多。
func BenchmarkBalanceByPositions(b *testing.B) {
	for _, n := range []int{1, 4, 16} {
		for _, mode := range []struct {
			name   string
			tdMode types.TdMode
		}{{"逐仓", types.TdIsolated}, {"全仓", types.TdCross}} {
			b.Run(fmt.Sprintf("%s/%d个仓位", mode.name, n), func(b *testing.B) {
				s := benchSim(b, types.NetMode)
				for _, inst := range benchInstruments(b, n) {
					mustBenchFill(b, s, inst.InstID, mode.tdMode, "1", "100")
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := s.BalanceOf("USDT"); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkFillByPositions 量的是 Fill 随仓位数增长的形状。
//
// Fill 会查一次可用余额做资金校验，而全仓的可用余额要过一遍仓位——两层叠起来
// 就是仓位数的平方。这个基准就是用来盯住它的。
func BenchmarkFillByPositions(b *testing.B) {
	for _, n := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("全仓/%d个仓位", n), func(b *testing.B) {
			s := benchSim(b, types.NetMode)
			insts := benchInstruments(b, n)
			for _, inst := range insts {
				mustBenchFill(b, s, inst.InstID, types.TdCross, "1", "100")
			}
			f := Fill{
				InstID: insts[0].InstID, TdMode: types.TdCross, Side: types.Buy,
				PosSide: types.PosNet, Sz: decimal.NewFromInt(1),
				Px: decimal.NewFromInt(100), ExecType: types.Taker,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f.Ts = int64(i)
				if _, err := s.Fill(f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func mustBenchFill(b *testing.B, s *Simulator, instID string,
	tdMode types.TdMode, sz, px string) {

	b.Helper()
	if _, err := s.Fill(Fill{
		InstID: instID, TdMode: tdMode, Side: types.Buy, PosSide: types.PosNet,
		Sz: decimal.RequireFromString(sz), Px: decimal.RequireFromString(px),
		ExecType: types.Taker, Ts: 1,
	}); err != nil {
		b.Fatalf("%s 建仓失败: %v", instID, err)
	}
}

// BenchmarkAdvanceCross 盯住全仓那条判据路径的开销。
//
// BenchmarkAdvance 用的是逐仓仓位，全仓的判据在那里没有仓位可算，改动的收益
// 看不见。网格这类策略常跑全仓，这条基准把它单独钉住。
func BenchmarkAdvanceCross(b *testing.B) {
	s := benchSim(b, types.NetMode)
	mustBenchFill(b, s, "BTC-USDT-SWAP", types.TdCross, "4", "78000")
	bar := Bar{
		InstID: "BTC-USDT-SWAP", Last: decimal.NewFromInt(78000),
		High: decimal.NewFromInt(78500), Low: decimal.NewFromInt(77500),
		MarkPx: decimal.NewFromInt(78000),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bar.Ts = int64(i)
		if _, err := s.Advance(bar); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPartialCloseChain 盯住「部分平仓不会让后续越算越慢」。
//
// decimal 的指数一旦随每次部分平仓无界增长，系数就变成几百位的大整数，
// 此后每一次加减乘都要在这个大整数上做——回测越跑越慢，而且慢在一条不会报错、
// 不影响结果正确性的路径上。这条基准把它变成一个看得见的数字。
func BenchmarkPartialCloseChain(b *testing.B) {
	s := benchSim(b, types.NetMode)
	if err := s.Deposit("USDT", decimal.NewFromInt(500000000)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Fill(Fill{
			InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Buy,
			PosSide: types.PosNet, Sz: decimal.NewFromInt(3),
			Px: decimal.NewFromInt(78000), ExecType: types.Taker, Ts: int64(i * 2),
		}); err != nil {
			b.Fatal(err)
		}
		if _, err := s.Fill(Fill{
			InstID: "BTC-USDT-SWAP", TdMode: types.TdIsolated, Side: types.Sell,
			PosSide: types.PosNet, Sz: decimal.NewFromInt(1),
			Px: decimal.NewFromInt(78100), ExecType: types.Taker, Ts: int64(i*2 + 1),
		}); err != nil {
			b.Fatal(err)
		}
	}
}

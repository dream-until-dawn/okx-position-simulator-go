package live

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
)

// fakeOKX 是一个最小的假 OKX 公共接口服务端。
//
// 用它而不是打真实接口，是为了让测试离线可跑、可复现，并且能构造出真实接口
// 无法按需触发的场景——比如「刷新失败」和「规则发生变更」。
type fakeOKX struct {
	mu sync.Mutex

	ctVal   string // 合约面值，改动它即模拟规则变更
	tierMMR string // 首档维持保证金率，同上
	fail    bool   // 置真则所有请求返回 500
	calls   int
	agents  []string // 收到的 User-Agent，用于确认已显式设置
}

func (f *fakeOKX) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		f.agents = append(f.agents, r.Header.Get("User-Agent"))
		fail, ctVal, mmr := f.fail, f.ctVal, f.tierMMR
		f.mu.Unlock()

		if fail {
			http.Error(w, `{"code":"50001","msg":"服务不可用"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/v5/public/instruments":
			fmt.Fprintf(w, `{"code":"0","msg":"","data":[
				{"instType":"SWAP","instId":"BTC-USDT-SWAP","instFamily":"BTC-USDT","uly":"BTC-USDT",
				 "settleCcy":"USDT","ctVal":%q,"ctMult":"1","ctValCcy":"BTC","ctType":"linear",
				 "groupId":"4","lever":"100","tickSz":"0.1","lotSz":"0.01","minSz":"0.01",
				 "maxLmtSz":"100000000","maxMktSz":"35000","listTime":"1573557408000",
				 "expTime":"","state":"live"}]}`, ctVal)

		case "/api/v5/public/position-tiers":
			if r.URL.Query().Get("instFamily") != "BTC-USDT" {
				fmt.Fprint(w, `{"code":"51001","msg":"Instrument ID doesn't exist.","data":[]}`)
				return
			}
			fmt.Fprintf(w, `{"code":"0","msg":"","data":[
				{"tier":"1","minSz":"0","maxSz":"1000","mmr":%q,"imr":"0.01","maxLever":"100"},
				{"tier":"2","minSz":"1000.01","maxSz":"5000","mmr":"0.005","imr":"0.015","maxLever":"66.66"}]}`, mmr)

		case "/api/v5/market/tickers":
			fmt.Fprint(w, `{"code":"0","msg":"","data":[
				{"instId":"BTC-USDT-SWAP","last":"77950.1","volCcy24h":"65347.8649"}]}`)

		default:
			http.NotFound(w, r)
		}
	})
}

func newFake(t *testing.T) (*fakeOKX, *Fetcher) {
	t.Helper()
	f := &fakeOKX{ctVal: "0.01", tierMMR: "0.004"}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	// 测试里不需要限速，去掉以免拖慢
	return f, NewFetcher(WithBaseURL(srv.URL), WithMinInterval(0))
}

func TestFetcherInstruments(t *testing.T) {
	_, fetcher := newFake(t)

	insts, err := fetcher.Instruments(context.Background(), types.InstSwap)
	if err != nil {
		t.Fatalf("拉取合约规格失败: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("取得 %d 个合约，期望 1 个", len(insts))
	}
	if insts[0].InstFamily != "BTC-USDT" || insts[0].GroupID != "4" {
		t.Errorf("解析结果不符: %+v", insts[0])
	}
}

// TestFetcherSetsUserAgent 实测 OKX 会以 403 拒绝某些客户端库的默认 User-Agent，
// 因此必须显式设置。这条测试守卫该设置不被误删。
func TestFetcherSetsUserAgent(t *testing.T) {
	fake, fetcher := newFake(t)

	if _, err := fetcher.Instruments(context.Background(), types.InstSwap); err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.agents) == 0 {
		t.Fatal("服务端没有收到请求")
	}
	ua := fake.agents[0]
	if ua == "" || strings.HasPrefix(ua, "Go-http-client") {
		t.Errorf("User-Agent = %q，未被显式设置", ua)
	}
	if !strings.Contains(ua, "okx-position-simulator-go") {
		t.Errorf("User-Agent = %q，未标识本库", ua)
	}
}

func TestFetcherPositionTiersPropagatesOKXError(t *testing.T) {
	_, fetcher := newFake(t)

	_, err := fetcher.PositionTiers(context.Background(), refdata.TierKey{
		InstType: types.InstSwap, MgnMode: types.MgnCross, Family: "NOPE-USDT"})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !strings.Contains(err.Error(), "51001") {
		t.Errorf("错误 = %v，期望携带 OKX 错误码 51001", err)
	}
}

// TestTurnover24hUsesNotional 成交额必须是 volCcy24h × last。
//
// 直接拿 volCcy24h 排序是错的：该字段是标的币的数量，不同币种相差若干数量级，
// 会让 SATS、PEPE 这类币占满榜首而把 BTC 挤出去。这条测试固定折算口径。
func TestTurnover24hUsesNotional(t *testing.T) {
	_, fetcher := newFake(t)

	m, err := fetcher.Turnover24h(context.Background(), types.InstSwap)
	if err != nil {
		t.Fatalf("拉取成交额失败: %v", err)
	}
	got, ok := m["BTC-USDT-SWAP"]
	if !ok {
		t.Fatal("结果里没有 BTC-USDT-SWAP")
	}
	// 65347.8649 × 77950.1 = 5093872603.74149
	const want = "5093872603.74149"
	if got.String() != want {
		t.Errorf("成交额 = %s，期望 %s（volCcy24h × last）", got, want)
	}
}

func newTestProvider(t *testing.T, f *Fetcher, opts ...ProviderOption) *Provider {
	t.Helper()
	base := []ProviderOption{
		WithFamilies("BTC-USDT"),
		WithRefreshInterval(0), // 默认不开后台循环，需要时单独覆盖
	}
	p := NewProvider(f, append(base, opts...)...)
	t.Cleanup(p.Stop)
	return p
}

func TestProviderRefreshAndQuery(t *testing.T) {
	_, fetcher := newFake(t)
	p := newTestProvider(t, fetcher)

	if p.Version() != 0 {
		t.Errorf("刷新前 Version = %d，期望 0", p.Version())
	}
	if _, err := p.Instrument("BTC-USDT-SWAP"); !errors.Is(err, ErrNoData) {
		t.Errorf("无数据时的错误 = %v，期望 ErrNoData", err)
	}

	if _, err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if p.Version() == 0 {
		t.Error("刷新后 Version 仍为 0")
	}

	inst, err := p.Instrument("BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("查询合约失败: %v", err)
	}
	tbl, err := refdata.TierTableFor(p, inst, types.MgnCross)
	if err != nil {
		t.Fatalf("查询档位表失败: %v", err)
	}
	if len(tbl.Tiers) != 2 {
		t.Errorf("档位数 = %d，期望 2", len(tbl.Tiers))
	}
	if p.FeeSchedule().Level() != types.Lv1 {
		t.Errorf("费率等级 = %q，期望 Lv1（默认值）", p.FeeSchedule().Level())
	}
}

// TestProviderKeepsDataOnRefreshFailure 是本包最重要的一条测试。
//
// 拉取失败时必须保留原有数据：陈旧的规则远好过没有规则。若刷新失败会清空数据，
// 一次网络抖动就能让整个系统的风险计算全部失效。
func TestProviderKeepsDataOnRefreshFailure(t *testing.T) {
	fake, fetcher := newFake(t)
	p := newTestProvider(t, fetcher)

	if _, err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("首次刷新失败: %v", err)
	}
	goodVersion := p.Version()

	fake.mu.Lock()
	fake.fail = true
	fake.mu.Unlock()

	if _, err := p.Refresh(context.Background()); err == nil {
		t.Fatal("服务端故障时期望刷新报错")
	}

	if p.Version() != goodVersion {
		t.Errorf("刷新失败后 Version 变成 %d，原为 %d：旧数据被破坏了",
			p.Version(), goodVersion)
	}
	inst, err := p.Instrument("BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("刷新失败后旧数据不可用: %v", err)
	}
	if inst.CtVal.String() != "0.01" {
		t.Errorf("刷新失败后 ctVal = %s，期望保留原值 0.01", inst.CtVal)
	}
	tbl, err := refdata.TierTableFor(p, inst, types.MgnCross)
	if err != nil {
		t.Fatalf("刷新失败后档位表不可用: %v", err)
	}
	if tbl.Tiers[0].MMR.String() != "0.004" {
		t.Errorf("刷新失败后首档 mmr = %s，期望保留原值 0.004", tbl.Tiers[0].MMR)
	}
}

func TestProviderDetectsChanges(t *testing.T) {
	fake, fetcher := newFake(t)
	p := newTestProvider(t, fetcher)

	if _, err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("首次刷新失败: %v", err)
	}

	// 模拟 OKX 调整了合约面值与首档维持保证金率
	fake.mu.Lock()
	fake.ctVal = "0.001"
	fake.tierMMR = "0.0045" // 仍小于次档的 0.005，保持单调不减
	fake.mu.Unlock()

	changes, err := p.Refresh(context.Background())
	if err != nil {
		t.Fatalf("二次刷新失败: %v", err)
	}
	if changes.IsEmpty() {
		t.Fatal("规则已变，却报告无变化")
	}

	if len(changes.Instruments) != 1 || changes.Instruments[0].Kind != Modified {
		t.Fatalf("合约变更 = %+v", changes.Instruments)
	}
	if got := changes.Instruments[0].Fields; len(got) != 1 || got[0] != "ctVal" {
		t.Errorf("变更字段 = %v，期望只有 ctVal", got)
	}

	if len(changes.TierTables) == 0 {
		t.Fatal("档位表已变，却未被检出")
	}
	found := false
	for _, tc := range changes.TierTables {
		if tc.Kind == Modified && strings.Contains(tc.Detail, "mmr 0.004 → 0.0045") {
			found = true
		}
	}
	if !found {
		t.Errorf("档位表变更未描述出 mmr 的变化: %+v", changes.TierTables)
	}

	t.Logf("变更报告:\n%s", changes)
}

// TestProviderNoChangeWhenDataIdentical 数据未变时不应报出变更，
// 否则变更回调会被无意义地反复触发。
func TestProviderNoChangeWhenDataIdentical(t *testing.T) {
	_, fetcher := newFake(t)
	p := newTestProvider(t, fetcher)

	if _, err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("首次刷新失败: %v", err)
	}
	changes, err := p.Refresh(context.Background())
	if err != nil {
		t.Fatalf("二次刷新失败: %v", err)
	}
	if !changes.IsEmpty() {
		t.Errorf("数据未变却报出 %d 项变更: %s", changes.Count(), changes)
	}
	// 版本时间戳仍应推进，以便区分「刚拉过」与「很久没拉」
	if changes.ToVersion <= changes.FromVersion {
		t.Errorf("版本未推进: %d → %d", changes.FromVersion, changes.ToVersion)
	}
}

// TestProviderAutoRefreshFiresCallback 后台定期刷新在检出变更时应触发回调。
func TestProviderAutoRefreshFiresCallback(t *testing.T) {
	fake, fetcher := newFake(t)

	got := make(chan Changes, 4)
	p := NewProvider(fetcher,
		WithFamilies("BTC-USDT"),
		WithRefreshInterval(20*time.Millisecond),
		WithOnChange(func(c Changes) { got <- c }),
	)
	t.Cleanup(p.Stop)

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if err := p.Start(context.Background()); err == nil {
		t.Error("重复 Start 应当报错")
	}

	fake.mu.Lock()
	fake.tierMMR = "0.0048"
	fake.mu.Unlock()

	select {
	case c := <-got:
		if c.IsEmpty() {
			t.Error("回调收到了空变更")
		}
		t.Logf("回调收到: %s", c)
	case <-time.After(3 * time.Second):
		t.Fatal("等待变更回调超时")
	}
}

func TestProviderStopIsIdempotent(t *testing.T) {
	_, fetcher := newFake(t)

	p := NewProvider(fetcher, WithFamilies("BTC-USDT"),
		WithRefreshInterval(20*time.Millisecond))
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	p.Stop()
	p.Stop() // 重复调用不得阻塞或 panic

	// 未 Start 过的 Provider 调 Stop 也应立即返回
	NewProvider(fetcher).Stop()
}

// TestProviderInitialSnapshotIsFallback 给了初始快照后，首次刷新失败也不应影响可用性。
func TestProviderInitialSnapshotIsFallback(t *testing.T) {
	fake, fetcher := newFake(t)
	fake.mu.Lock()
	fake.fail = true
	fake.mu.Unlock()

	p := NewProvider(fetcher,
		WithInitialSnapshot(refdata.MustEmbedded()),
		WithRefreshInterval(0))
	t.Cleanup(p.Stop)

	if err := p.Start(context.Background()); err == nil {
		t.Error("服务端故障时首次刷新应报错")
	}
	// 但兜底快照必须仍然可用
	if _, err := p.Instrument("BTC-USDT-SWAP"); err != nil {
		t.Errorf("首次刷新失败后兜底快照不可用: %v", err)
	}
}

// TestFamiliesDerivedFromInitialSnapshot 未显式指定品种时，
// 应从初始快照推导出「继续跟踪已有的那些」。
func TestFamiliesDerivedFromInitialSnapshot(t *testing.T) {
	_, fetcher := newFake(t)

	p := NewProvider(fetcher,
		WithInitialSnapshot(refdata.MustEmbedded()),
		WithRefreshInterval(0))
	t.Cleanup(p.Stop)

	if len(p.families) == 0 {
		t.Fatal("未从初始快照推导出要跟踪的品种")
	}
	var hasBTC bool
	for _, f := range p.families {
		if f == "BTC-USDT" {
			hasBTC = true
		}
	}
	if !hasBTC {
		t.Errorf("推导出的品种里没有 BTC-USDT: %v", p.families)
	}
}

func TestDiffHandlesNil(t *testing.T) {
	s := refdata.MustEmbedded()

	if c := Diff(nil, s); !c.IsEmpty() {
		t.Error("与 nil 比较不应报出变更")
	}
	if c := Diff(s, nil); !c.IsEmpty() {
		t.Error("与 nil 比较不应报出变更")
	}
	if c := Diff(nil, nil); !c.IsEmpty() {
		t.Error("两个 nil 比较不应报出变更")
	}
	if c := Diff(s, s); !c.IsEmpty() {
		t.Errorf("自身比较报出了 %d 项变更: %s", c.Count(), c)
	}
}

// TestProviderCarriesForwardFailedTierTable 单张档位表拉取失败时应沿用上一份，
// 而不是让它从快照里消失。
//
// 若直接丢弃，差异报告会显示成「档位表已移除」，与品种真正下架无从区分，
// 下游还会误以为该合约没有档位表而算不出维持保证金——一次网络抖动就足以
// 造成这种后果。
func TestProviderCarriesForwardFailedTierTable(t *testing.T) {
	fake, fetcher := newFake(t)
	p := newTestProvider(t, fetcher)

	if _, err := p.Refresh(context.Background()); err != nil {
		t.Fatalf("首次刷新失败: %v", err)
	}

	// 让 OKX 下发一份违反单调性的档位表（首档 mmr 高于次档），
	// 它会被 NewTierTable 正确拒绝，从而模拟单张表拉取失败
	fake.mu.Lock()
	fake.tierMMR = "0.9"
	fake.mu.Unlock()

	changes, err := p.Refresh(context.Background())
	if err != nil {
		t.Fatalf("二次刷新失败: %v", err)
	}

	for _, tc := range changes.TierTables {
		if tc.Kind == Removed {
			t.Errorf("档位表 %s 被报为移除，应沿用上一份数据", tc.Key)
		}
	}

	inst, err := p.Instrument("BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("查询合约失败: %v", err)
	}
	tbl, err := refdata.TierTableFor(p, inst, types.MgnCross)
	if err != nil {
		t.Fatalf("档位表消失了: %v", err)
	}
	if tbl.Tiers[0].MMR.String() != "0.004" {
		t.Errorf("首档 mmr = %s，期望沿用上一份的 0.004", tbl.Tiers[0].MMR)
	}
}

// TestProviderConcurrentRefreshAndRead 后台自动刷新与手动 Refresh、以及大量并发
// 读取必须能共存。
//
// Fetcher 本身不是并发安全的（内部有可变的限速状态），而 Provider 恰好制造了
// 并发使用它的场景，故 Refresh 内部做了串行化。本机无 C 编译器跑不了 -race，
// 这条测试至少能暴露死锁与显而易见的状态错乱；有 race 检测的环境下它更有价值。
func TestProviderConcurrentRefreshAndRead(t *testing.T) {
	_, fetcher := newFake(t)

	p := NewProvider(fetcher,
		WithFamilies("BTC-USDT"),
		WithRefreshInterval(5*time.Millisecond))
	t.Cleanup(p.Stop)

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	var wg sync.WaitGroup
	deadline := time.Now().Add(300 * time.Millisecond)

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if _, err := p.Refresh(context.Background()); err != nil {
					t.Errorf("并发刷新失败: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				inst, err := p.Instrument("BTC-USDT-SWAP")
				if err != nil {
					t.Errorf("并发读取失败: %v", err)
					return
				}
				if inst.CtVal.String() != "0.01" {
					t.Errorf("读到不一致的 ctVal: %s", inst.CtVal)
					return
				}
				if _, err := refdata.TierTableFor(p, inst, types.MgnCross); err != nil {
					t.Errorf("并发读取档位表失败: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if p.Version() == 0 {
		t.Error("并发结束后 Version 仍为 0")
	}
}

// TestWithSimulatedSetsHeader 模拟盘标识必须落到请求头上。
//
// 这条测试的分量比它看起来重：模拟盘与生产环境的规则数据不同（档位区间、tickSz、
// 杠杆上限均有实测差异），对拍工具若因为漏了这个头而拿到生产数据，
// 每个计算都会偏，且偏差看起来像模拟器自身的缺陷。
func TestWithSimulatedSetsHeader(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("x-simulated-trading"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":"0","msg":"","data":[]}`)
	}))
	t.Cleanup(srv.Close)

	sim := NewFetcher(WithBaseURL(srv.URL), WithMinInterval(0), WithSimulated(true))
	if _, err := sim.Instruments(context.Background(), types.InstSwap); err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(got) != 1 || got[0] != "1" {
		t.Errorf("模拟盘请求的 x-simulated-trading = %v，期望 \"1\"", got)
	}

	prod := NewFetcher(WithBaseURL(srv.URL), WithMinInterval(0))
	if _, err := prod.Instruments(context.Background(), types.InstSwap); err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(got) != 2 || got[1] != "" {
		t.Errorf("生产请求不应带 x-simulated-trading，实际 %v", got)
	}
}

// Package live 从 OKX 公共接口拉取规则数据。
//
// 本包是独立子包，核心包 refdata 不依赖它——这样核心包的依赖树里就不会出现
// net/http，只用回测快照的使用者无需承担网络相关的传递依赖。
//
// 所用接口全部免鉴权：
//
//	GET /api/v5/public/instruments      合约规格
//	GET /api/v5/public/position-tiers   档位表
//	GET /api/v5/market/tickers          行情（仅用于挑选要收录的品种）
//
// 费率不在此列：/api/v5/account/trade-fee 需要鉴权且返回的是「当前账户」的费率，
// 无法作为公共规则拉取。费率请用 refdata.DefaultFeeSchedule 或 FeeSchedule.WithRate。
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// DefaultBaseURL 是 OKX 的公共接口地址。
const DefaultBaseURL = "https://www.okx.com"

// DefaultUserAgent 标识本库发出的请求。
//
// 必须显式设置：实测 OKX 会以 403 拒绝某些客户端库的默认 User-Agent。
const DefaultUserAgent = "okx-position-simulator-go/0.1 (+https://github.com/dream-until-dawn/okx-position-simulator-go)"

// DefaultMinInterval 是相邻两次请求之间的最小间隔。
//
// OKX 对公共接口按 IP 限速，而装配一份完整快照需要为每个品种分别拉取逐仓与全仓
// 两张档位表，请求数量可观。默认取一个保守值，宁可慢也不要触发限速。
const DefaultMinInterval = 120 * time.Millisecond

// Fetcher 从 OKX 公共接口拉取规则数据。
//
// 内置串行限速，可安全地连续调用；但它本身不是并发安全的，
// 多个 goroutine 同时使用请各自持有一个实例。
type Fetcher struct {
	baseURL     string
	userAgent   string
	client      *http.Client
	minInterval time.Duration
	last        time.Time
}

// Option 调整 Fetcher 的行为。
type Option func(*Fetcher)

// WithBaseURL 改用其他接口地址，主要用于测试时指向本地假服务端。
func WithBaseURL(u string) Option { return func(f *Fetcher) { f.baseURL = u } }

// WithHTTPClient 改用自定义的 HTTP 客户端，可用于设置代理或超时。
func WithHTTPClient(c *http.Client) Option { return func(f *Fetcher) { f.client = c } }

// WithUserAgent 改用自定义的 User-Agent。
func WithUserAgent(ua string) Option { return func(f *Fetcher) { f.userAgent = ua } }

// WithMinInterval 调整相邻请求的最小间隔；传入非正值表示不限速。
func WithMinInterval(d time.Duration) Option { return func(f *Fetcher) { f.minInterval = d } }

// NewFetcher 新建拉取器。
func NewFetcher(opts ...Option) *Fetcher {
	f := &Fetcher{
		baseURL:     DefaultBaseURL,
		userAgent:   DefaultUserAgent,
		client:      &http.Client{Timeout: 30 * time.Second},
		minInterval: DefaultMinInterval,
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// throttle 在必要时等待，使相邻请求间隔不小于 minInterval。
func (f *Fetcher) throttle(ctx context.Context) error {
	if f.minInterval <= 0 || f.last.IsZero() {
		f.last = time.Now()
		return nil
	}
	wait := f.minInterval - time.Since(f.last)
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	f.last = time.Now()
	return nil
}

// get 发起一次 GET 请求并返回响应体。
func (f *Fetcher) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if err := f.throttle(ctx); err != nil {
		return nil, err
	}
	u := f.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 的响应失败: %w", path, err)
	}
	// OKX 在业务错误时仍返回 200，错误信息在响应体里；非 200 才是传输层问题。
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("请求 %s 返回 HTTP %d: %s",
			path, resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Instruments 拉取某个产品类型下的全部合约规格。
func (f *Fetcher) Instruments(ctx context.Context, instType types.InstType) ([]refdata.Instrument, error) {
	body, err := f.get(ctx, "/api/v5/public/instruments",
		url.Values{"instType": {instType.String()}})
	if err != nil {
		return nil, err
	}
	return refdata.DecodeResponse[refdata.Instrument](body)
}

// PositionTiers 拉取一个品种在指定保证金模式下的档位表。
func (f *Fetcher) PositionTiers(ctx context.Context, key refdata.TierKey) (*refdata.TierTable, error) {
	body, err := f.get(ctx, "/api/v5/public/position-tiers", url.Values{
		"instType":   {key.InstType.String()},
		"tdMode":     {key.MgnMode.String()},
		"instFamily": {key.Family},
	})
	if err != nil {
		return nil, err
	}
	tiers, err := refdata.DecodeResponse[refdata.PositionTier](body)
	if err != nil {
		return nil, fmt.Errorf("拉取 %s 的档位表失败: %w", key, err)
	}
	return refdata.NewTierTable(key, tiers)
}

// Turnover24h 拉取某个产品类型下各合约的 24 小时成交额，键为 instId。
//
// 成交额 = volCcy24h × last，即按币计的成交量折算成计价币的金额。
//
// 必须折算，不能直接拿 volCcy24h 排序：该字段是**标的币的数量**
// （实测 BTC-USDT-SWAP 的 vol24h × ctVal 恰好等于 volCcy24h），
// 不同币种的单位数量相差若干个数量级——BTC 是几万个，SATS 是几十万亿个。
// 直接比大小会让 SATS、PEPE、SHIB 这类币占满榜首，而 BTC 连前三十都进不去。
//
// 它不属于规则数据，仅用于挑选内置快照要收录哪些品种——全量收录会让嵌入的
// 快照过大，而按成交额取头部能以很小的体积覆盖绝大多数回测场景。
func (f *Fetcher) Turnover24h(ctx context.Context, instType types.InstType) (map[string]decimal.Decimal, error) {
	body, err := f.get(ctx, "/api/v5/market/tickers",
		url.Values{"instType": {instType.String()}})
	if err != nil {
		return nil, err
	}
	type rawTicker struct {
		InstID    string `json:"instId"`
		Last      string `json:"last"`
		VolCcy24h string `json:"volCcy24h"`
	}
	var env struct {
		Code string      `json:"code"`
		Msg  string      `json:"msg"`
		Data []rawTicker `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("解析行情失败: %w", err)
	}
	if env.Code != refdata.CodeOK {
		return nil, fmt.Errorf("拉取行情失败: OKX %s: %s", env.Code, env.Msg)
	}
	out := make(map[string]decimal.Decimal, len(env.Data))
	for _, t := range env.Data {
		if t.VolCcy24h == "" || t.Last == "" {
			continue
		}
		vol, err := decimal.NewFromString(t.VolCcy24h)
		if err != nil {
			continue // 个别合约的字段异常时跳过，不影响整体挑选
		}
		last, err := decimal.NewFromString(t.Last)
		if err != nil {
			continue
		}
		out[t.InstID] = vol.Mul(last)
	}
	return out, nil
}

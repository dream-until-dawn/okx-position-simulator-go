package live

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
)

// DefaultRefreshInterval 是默认的自动刷新周期。
//
// OKX 的规则变动并不频繁——档位表的调整通常以周或月计，合约上下架也不过每日
// 若干起。取一小时是在「及时跟进变更」与「不给接口添无谓压力」之间的折中。
const DefaultRefreshInterval = time.Hour

// ErrNoData 表示 Provider 尚无任何数据：既没有给初始快照，首次刷新也没成功。
var ErrNoData = errors.New("规则数据尚未就绪")

// Provider 实现 refdata.Provider，持有一份快照并可按周期自动拉取 OKX 的规则变更。
//
// 与不可变的 refdata.Snapshot 相对：那个用于回测（规则恒定，结果可复现），
// 这个用于实盘（跟随 OKX 变更，Version 随之推进）。
//
// 并发安全。读取时取一次内部快照指针，由于 Snapshot 不可变，
// 单次读取到的数据必然自洽，不会读到刷新中途的半成品。
type Provider struct {
	fetcher  *Fetcher
	instType types.InstType
	families []string
	interval time.Duration
	onChange func(Changes)
	fees     refdata.FeeSchedule

	// refreshMu 保证同一时刻只有一次刷新在跑。
	//
	// Fetcher 本身不是并发安全的（内部有可变的限速状态），而 Provider 恰好制造了
	// 并发使用它的场景：后台循环在定期刷新，使用者随时可以手动调 Refresh。
	// 串行化还顺带避免了两次刷新同时打接口的浪费。
	refreshMu sync.Mutex

	mu   sync.RWMutex
	snap *refdata.Snapshot

	startMu  sync.Mutex
	started  bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// ProviderOption 调整 Provider 的行为。
type ProviderOption func(*Provider)

// WithInitialSnapshot 设置初始快照，使 Provider 在首次刷新完成前就可用。
//
// 强烈建议传入 refdata.MustEmbedded()：否则首次刷新失败时 Provider 无数据可用，
// 所有查询都会失败。有了兜底快照，网络异常最多让规则数据陈旧，而不会让整个
// 系统不可用。
func WithInitialSnapshot(s *refdata.Snapshot) ProviderOption {
	return func(p *Provider) { p.snap = s }
}

// WithFamilies 指定要跟踪的品种（instFamily）。
//
// 不指定时从初始快照中推导——即「继续跟踪已有的那些」。两者都没有时，
// 首次刷新会拉取该产品类型下的全部合约规格，但不拉任何档位表，
// 因为无从知道该关心哪些品种。
func WithFamilies(families ...string) ProviderOption {
	return func(p *Provider) { p.families = append([]string(nil), families...) }
}

// WithInstType 指定要跟踪的产品类型，默认 SWAP。
func WithInstType(t types.InstType) ProviderOption {
	return func(p *Provider) { p.instType = t }
}

// WithRefreshInterval 设置自动刷新周期；传入非正值表示不自动刷新，只能手动调用 Refresh。
func WithRefreshInterval(d time.Duration) ProviderOption {
	return func(p *Provider) { p.interval = d }
}

// WithOnChange 注册规则变更回调。
//
// 回调在后台刷新的 goroutine 中同步调用，因此不可阻塞，也不应在其中调用
// 本 Provider 的 Stop——请把事件转交给自己的 channel 或日志。
// 只有确实存在变更时才会触发。
func WithOnChange(fn func(Changes)) ProviderOption {
	return func(p *Provider) { p.onChange = fn }
}

// WithFeeSchedule 设置费率表。
//
// 费率不随规则数据刷新——它是账户相关的，无法从免鉴权接口取得。
// 不设置时用 refdata.DefaultFeeSchedule()。
func WithFeeSchedule(s refdata.FeeSchedule) ProviderOption {
	return func(p *Provider) { p.fees = s }
}

// 编译期确认 *Provider 满足 refdata.Provider。
var _ refdata.Provider = (*Provider)(nil)

// NewProvider 新建自动刷新的规则数据源。
//
// 构造时不发起任何网络请求。要么先调用 Refresh 同步拉一次，
// 要么调用 Start 交给后台，两者都不做则只有初始快照可用。
func NewProvider(f *Fetcher, opts ...ProviderOption) *Provider {
	p := &Provider{
		fetcher:  f,
		instType: types.InstSwap,
		interval: DefaultRefreshInterval,
		fees:     refdata.DefaultFeeSchedule(),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	for _, o := range opts {
		o(p)
	}
	if len(p.families) == 0 && p.snap != nil {
		p.families = familiesOf(p.snap)
	}
	return p
}

// familiesOf 从快照中提取已收录的品种。
func familiesOf(s *refdata.Snapshot) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range s.TierKeys() {
		if !seen[k.Family] {
			seen[k.Family] = true
			out = append(out, k.Family)
		}
	}
	return out
}

// Refresh 立即拉取一次规则数据，返回相对上一份数据的变更。
//
// 拉取失败时保留原有数据不动，只返回错误——陈旧的规则远好过没有规则。
//
// 可与后台自动刷新并发调用：内部串行化，同一时刻只有一次刷新在跑，
// 后到的一方会等待前一次完成。
func (p *Provider) Refresh(ctx context.Context) (Changes, error) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	p.mu.RLock()
	prev := p.snap
	p.mu.RUnlock()

	next, err := p.fetch(ctx, prev)
	if err != nil {
		return Changes{}, err
	}

	p.mu.Lock()
	p.snap = next
	p.mu.Unlock()

	return Diff(prev, next), nil
}

// fetch 装配一份新快照，不改动 Provider 的状态。
//
// prev 是上一份快照，用于在单张档位表拉取失败时兜底，可为 nil。
func (p *Provider) fetch(ctx context.Context, prev *refdata.Snapshot) (*refdata.Snapshot, error) {
	insts, err := p.fetcher.Instruments(ctx, p.instType)
	if err != nil {
		return nil, fmt.Errorf("刷新规则数据失败: %w", err)
	}

	// Version 是使用者判断「规则是否刷新过」的唯一依据，因此必须保证每次成功
	// 刷新都推进。毫秒时间戳单靠自身做不到这一点——两次刷新若落在同一毫秒内，
	// 版本号就会原地踏步，变更从外部看便无从察觉。实际刷新间隔以小时计，
	// 撞上的概率极低，但契约不该建立在概率上。
	ts := time.Now().UnixMilli()
	if prev != nil && ts <= prev.Version() {
		ts = prev.Version() + 1
	}

	b := refdata.NewSnapshotBuilder(ts).
		AddInstruments(insts...).
		SetFeeSchedule(p.fees)

	for _, fam := range p.families {
		for _, mode := range []types.MgnMode{types.MgnCross, types.MgnIsolated} {
			key := refdata.TierKey{InstType: p.instType, MgnMode: mode, Family: fam}
			tbl, err := p.fetcher.PositionTiers(ctx, key)
			if err == nil {
				b.AddTierTable(tbl)
				continue
			}
			// 上下文被取消是真的该停下，其余失败不应毁掉整次刷新。
			if ctx.Err() != nil {
				return nil, fmt.Errorf("刷新规则数据失败: %w", ctx.Err())
			}
			// 单张档位表失败时沿用上一份：一次网络抖动或 OKX 下发了不合法的数据，
			// 不该让该品种的档位表凭空消失。若直接丢弃，差异报告里会显示成
			// 「档位表已移除」，与品种真正下架无从区分，下游还会误以为该合约
			// 没有档位表而算不出维持保证金。陈旧的档位远好过没有档位。
			if prev != nil {
				if old, e := prev.TierTable(key); e == nil {
					b.AddTierTable(old)
				}
			}
		}
	}
	return b.Build(), nil
}

// Start 启动后台定期刷新，重复调用返回错误。
//
// 它会立即同步执行一次刷新，使 Provider 尽快就绪；该次刷新的错误会被返回，
// 但后台循环仍会启动——首次失败往往只是一时的网络问题，不该就此放弃跟进规则变更。
// 此后每隔 interval 刷新一次，后续错误不再返回，可通过 Version 是否推进察觉。
//
// 传入的 ctx 被取消时后台循环退出，效果同 Stop。
func (p *Provider) Start(ctx context.Context) error {
	p.startMu.Lock()
	if p.started {
		p.startMu.Unlock()
		return errors.New("Provider 已经启动过")
	}
	p.started = true
	p.startMu.Unlock()

	_, err := p.Refresh(ctx)
	if p.interval <= 0 {
		close(p.done)
		return err
	}
	go p.loop(ctx)
	return err
}

func (p *Provider) loop(ctx context.Context) {
	defer close(p.done)

	t := time.NewTicker(p.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-t.C:
			changes, err := p.Refresh(ctx)
			if err != nil {
				continue // 保留原有数据，等下一个周期再试
			}
			if !changes.IsEmpty() && p.onChange != nil {
				p.onChange(changes)
			}
		}
	}
}

// Stop 停止后台刷新并等待其退出。重复调用无效果；未 Start 过时直接返回。
func (p *Provider) Stop() {
	p.startMu.Lock()
	started := p.started
	p.startMu.Unlock()

	p.stopOnce.Do(func() { close(p.stop) })
	if started {
		<-p.done
	}
}

// Snapshot 返回当前的规则数据快照；尚无数据时返回 nil。
//
// 返回的快照不可变，可长期持有——需要在一段计算内保持规则一致时，
// 取一次快照后一直用它，比反复查询 Provider 更可靠。
func (p *Provider) Snapshot() *refdata.Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.snap
}

// Instrument 实现 refdata.Provider。
func (p *Provider) Instrument(instID string) (refdata.Instrument, error) {
	s := p.Snapshot()
	if s == nil {
		return refdata.Instrument{}, fmt.Errorf("查询合约 %s: %w", instID, ErrNoData)
	}
	return s.Instrument(instID)
}

// TierTable 实现 refdata.Provider。
func (p *Provider) TierTable(key refdata.TierKey) (*refdata.TierTable, error) {
	s := p.Snapshot()
	if s == nil {
		return nil, fmt.Errorf("查询档位表 %s: %w", key, ErrNoData)
	}
	return s.TierTable(key)
}

// FeeSchedule 实现 refdata.Provider。
func (p *Provider) FeeSchedule() refdata.FeeSchedule {
	s := p.Snapshot()
	if s == nil {
		return p.fees
	}
	return s.FeeSchedule()
}

// Version 实现 refdata.Provider；尚无数据时返回 0。
//
// 每次成功刷新都严格推进，即便数据本身没有变化——它回答的是「数据有多新」，
// 而不是「数据是否变了」。后者要看 Refresh 返回的 Changes。
func (p *Provider) Version() int64 {
	s := p.Snapshot()
	if s == nil {
		return 0
	}
	return s.Version()
}

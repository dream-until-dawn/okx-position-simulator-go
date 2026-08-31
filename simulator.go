package okxsim

import (
	"sort"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// Config 是模拟器的配置。
type Config struct {
	// AcctLv 账户模式，默认 acctLv=2 现货合约模式。
	AcctLv types.AcctLv

	// PosMode 持仓方式，默认买卖模式。
	PosMode types.PosMode

	// RefData 规则数据源。回测请用不可变的 refdata.Snapshot，
	// 用会自动刷新的数据源会让结果不可复现。
	RefData refdata.Provider

	// FeeSchedule 覆盖 RefData 自带的费率表。费率是账户相关的，
	// 使用者有实际费率时应在此给出。
	FeeSchedule *refdata.FeeSchedule

	// DefaultLever 未经 SetLeverage 显式设置时使用的杠杆，默认 10。
	DefaultLever decimal.Decimal
}

type positionKey struct {
	instID  string
	posSide types.PosSide
}

type leverageKey struct {
	instID  string
	mgnMode types.MgnMode
	posSide types.PosSide
}

// Simulator 是 OKX 仓位管理的模拟器。
//
// 它只做一件事：把成交事件转换成仓位与账户状态。撮合、行情与订单簿由使用者的
// 回测引擎负责。
//
// 非并发安全。回测是单线程热路径，加锁是纯粹的浪费；确有并发需要时请在外层
// 自行串行化。
type Simulator struct {
	cfg     Config
	fees    refdata.FeeSchedule
	cash    map[string]decimal.Decimal
	pos     map[positionKey]Position
	marks   map[string]decimal.Decimal
	last    map[string]decimal.Decimal
	lever   map[leverageKey]decimal.Decimal
	pending map[string]PendingOrder
}

// New 新建模拟器。RefData 为必填。
func New(cfg Config) (*Simulator, error) {
	if cfg.RefData == nil {
		return nil, okxerr.New(okxerr.CodeParamEmpty, "Config.RefData 不能为空")
	}
	if cfg.AcctLv == "" {
		cfg.AcctLv = types.AcctSpotAndFutures
	}
	if !cfg.AcctLv.Valid() {
		return nil, okxerr.New(okxerr.CodeParamError, "acctLv: 非法的账户模式 %q", cfg.AcctLv)
	}
	if cfg.PosMode == "" {
		cfg.PosMode = types.NetMode
	}
	if !cfg.PosMode.Valid() {
		return nil, okxerr.New(okxerr.CodeParamError, "posMode: 非法的持仓方式 %q", cfg.PosMode)
	}
	if cfg.DefaultLever.IsZero() {
		cfg.DefaultLever = decimal.NewFromInt(10)
	}

	fees := cfg.RefData.FeeSchedule()
	if cfg.FeeSchedule != nil {
		fees = *cfg.FeeSchedule
	}

	return &Simulator{
		cfg:     cfg,
		fees:    fees,
		cash:    make(map[string]decimal.Decimal),
		pos:     make(map[positionKey]Position),
		marks:   make(map[string]decimal.Decimal),
		last:    make(map[string]decimal.Decimal),
		lever:   make(map[leverageKey]decimal.Decimal),
		pending: make(map[string]PendingOrder),
	}, nil
}

// PosMode 返回持仓方式。
func (s *Simulator) PosMode() types.PosMode { return s.cfg.PosMode }

// FeeSchedule 返回当前生效的费率表。
func (s *Simulator) FeeSchedule() refdata.FeeSchedule { return s.fees }

// Deposit 入金。
func (s *Simulator) Deposit(ccy string, amt decimal.Decimal) error {
	if ccy == "" {
		return okxerr.New(okxerr.CodeParamEmpty, "ccy 不能为空")
	}
	if !amt.IsPositive() {
		return okxerr.New(okxerr.CodeParamError, "amt: 入金金额须为正数，实为 %s", amt)
	}
	s.cash[ccy] = s.cash[ccy].Add(amt)
	return nil
}

// Withdraw 出金；可用余额不足时返回错误码 51008。
func (s *Simulator) Withdraw(ccy string, amt decimal.Decimal) error {
	if !amt.IsPositive() {
		return okxerr.New(okxerr.CodeParamError, "amt: 出金金额须为正数，实为 %s", amt)
	}
	if s.cash[ccy].LessThan(amt) {
		return okxerr.New(okxerr.CodeInsufficientBal,
			"%s 可用余额 %s 不足以出金 %s", ccy, s.cash[ccy], amt)
	}
	s.cash[ccy] = s.cash[ccy].Sub(amt)
	return nil
}

// CashBal 返回某币种的现金余额。
func (s *Simulator) CashBal(ccy string) decimal.Decimal { return s.cash[ccy] }

// SetLeverage 设置杠杆，对应 OKX 的 POST /api/v5/account/set-leverage。
//
// 买卖模式下 posSide 传 net 或留空。杠杆不得超过该合约当前档位允许的上限。
func (s *Simulator) SetLeverage(instID string, mgnMode types.MgnMode,
	posSide types.PosSide, lever decimal.Decimal) error {

	inst, err := s.cfg.RefData.Instrument(instID)
	if err != nil {
		return err
	}
	if !lever.IsPositive() {
		return okxerr.New(okxerr.CodeParamError, "lever: 杠杆须为正数，实为 %s", lever)
	}
	tbl, err := refdata.TierTableFor(s.cfg.RefData, inst, mgnMode)
	if err != nil {
		return err
	}
	if max := tbl.MaxLeverage(); lever.GreaterThan(max) {
		return okxerr.New(okxerr.CodeParamError,
			"lever: %s 的杠杆 %s 超过上限 %s", instID, lever, max)
	}
	side, err := s.normalizePosSide(posSide)
	if err != nil {
		return err
	}
	s.lever[leverageKey{instID, mgnMode, side}] = lever
	return nil
}

// Leverage 返回某仓位当前生效的杠杆；未设置过则返回配置的默认值。
func (s *Simulator) Leverage(instID string, mgnMode types.MgnMode, posSide types.PosSide) decimal.Decimal {
	side, err := s.normalizePosSide(posSide)
	if err != nil {
		return s.cfg.DefaultLever
	}
	if v, ok := s.lever[leverageKey{instID, mgnMode, side}]; ok {
		return v
	}
	return s.cfg.DefaultLever
}

// SetMark 更新标记价。
//
// 只更新价格，不触发任何风控——资金费结算与强平检查由 Advance 按时钟推进，
// 这样多个合约在同一时刻的更新顺序就不会影响结果。
func (s *Simulator) SetMark(instID string, px decimal.Decimal) error {
	if !px.IsPositive() {
		return okxerr.New(okxerr.CodeParamError, "标记价须为正数，实为 %s", px)
	}
	s.marks[instID] = px
	return nil
}

// MarkPx 返回某合约当前的标记价；未设置过返回零值。
func (s *Simulator) MarkPx(instID string) decimal.Decimal { return s.marks[instID] }

// normalizePosSide 校验并归一化持仓方向。
//
// 买卖模式下只接受 net 或空值，开平仓模式下只接受 long/short——
// 用错了 OKX 会返回 51000 Parameter posSide error，这里保持一致。
func (s *Simulator) normalizePosSide(side types.PosSide) (types.PosSide, error) {
	if s.cfg.PosMode == types.NetMode {
		if side == "" || side == types.PosNet {
			return types.PosNet, nil
		}
		return "", okxerr.New(okxerr.CodeParamError,
			"posSide: 买卖模式下应为 net，实为 %q", side)
	}
	if side == types.PosLong || side == types.PosShort {
		return side, nil
	}
	return "", okxerr.New(okxerr.CodeParamError,
		"posSide: 开平仓模式下应为 long 或 short，实为 %q", side)
}

// Fill 灌入一笔成交，返回它对仓位与账户的影响。
//
// 资金流转经真实账户标定，各项差值均为 0：
//
//	开仓  现金减少「保证金 + 手续费」，手续费不计入保证金
//	平仓  现金增加「按张数比例释放的保证金 + 已实现盈亏 + 手续费」
//
// 现金不足以支撑本次成交时返回错误码 51008，与 OKX 一致。
func (s *Simulator) Fill(f Fill) (FillResult, error) {
	inst, err := s.cfg.RefData.Instrument(f.InstID)
	if err != nil {
		return FillResult{}, err
	}
	if err := inst.ValidateSize(f.Sz); err != nil {
		return FillResult{}, err
	}
	if !f.Px.IsPositive() {
		return FillResult{}, okxerr.New(okxerr.CodeParamError,
			"px: 成交价须为正数，实为 %s", f.Px)
	}
	if !f.Side.Valid() {
		return FillResult{}, okxerr.New(okxerr.CodeParamError, "side: 非法方向 %q", f.Side)
	}
	mgnMode, ok := f.TdMode.MgnMode()
	if !ok {
		return FillResult{}, okxerr.New(okxerr.CodeParamError,
			"tdMode: 衍生品只支持 isolated 与 cross，实为 %q", f.TdMode)
	}
	if mgnMode != types.MgnIsolated {
		return FillResult{}, okxerr.New(okxerr.CodeParamError,
			"tdMode: 全仓模式将在 v0.3.0 支持，当前只支持 isolated")
	}
	side, err := s.normalizePosSide(f.PosSide)
	if err != nil {
		return FillResult{}, err
	}
	f.PosSide = side

	rate, err := s.fees.Rate(inst)
	if err != nil {
		return FillResult{}, err
	}
	if f.ExecType == "" {
		f.ExecType = types.Taker
	}
	feeRate := rate.Of(f.ExecType)

	key := positionKey{f.InstID, side}
	pos, exists := s.pos[key]
	if !exists {
		pos = Position{
			InstID: f.InstID, MgnMode: mgnMode, PosSide: side,
			Lever: s.Leverage(f.InstID, mgnMode, side),
		}
	}

	res := applyFill(pos, f, inst, feeRate, s.cfg.PosMode)

	// 开仓部分的名义价值按成交价计——保证金是开仓那一刻定下的，
	// 与随后的标记价变动无关。
	openNotional := notional(inst, res.OpenedSz, f.Px)
	md := computeMarginDelta(res, openNotional, pos.Lever)

	delta := res.Pnl.Add(res.Fee).Sub(md.Net())

	// 约束是可用余额而非现金余额：被其他挂单冻结的部分已有归属，不能再拿来
	// 支撑这笔成交。该委托自身的冻结要排除在外——它正在转化为本次成交的保证金，
	// 若把它也算作占用，一笔本该成功的成交会被自己的冻结挡下。
	avail := s.cash[inst.SettleCcy]
	ordMargin, ordFee := s.orderFreeze(inst.SettleCcy)
	avail = avail.Sub(ordMargin).Sub(ordFee)
	if f.OrdID != "" {
		if p, ok := s.pending[f.OrdID]; ok {
			avail = avail.Add(p.Cost.Frozen)
		}
	}
	if avail.Add(delta).IsNegative() {
		return FillResult{}, okxerr.New(okxerr.CodeInsufficientBal,
			"%s 可用余额 %s 不足以支撑本次成交（需 %s）",
			inst.SettleCcy, avail, delta.Neg())
	}

	after := res.After
	after.Margin = pos.Margin.Sub(md.Release).Add(md.Add)
	if after.IsEmpty() {
		// 全平后保证金应当归零；比例释放的舍入残差不该留在账上
		after.Margin = decimal.Zero
	}
	res.After = after

	// 校验通过后再解除冻结。顺序很重要：早于校验会让「其他挂单占用了多少」
	// 算错，晚于落账则会在中途失败时留下已消费却仍冻结的资金。
	if f.OrdID != "" {
		delete(s.pending, f.OrdID)
	}

	s.cash[inst.SettleCcy] = s.cash[inst.SettleCcy].Add(delta)
	if after.IsEmpty() {
		delete(s.pos, key)
	} else {
		s.pos[key] = after
	}
	return res, nil
}

// PositionOf 返回某个仓位；不存在时第二个返回值为 false。
func (s *Simulator) PositionOf(instID string, posSide types.PosSide) (Position, bool) {
	side, err := s.normalizePosSide(posSide)
	if err != nil {
		return Position{}, false
	}
	p, ok := s.pos[positionKey{instID, side}]
	return p, ok
}

// Positions 返回全部仓位，按 instId 与持仓方向排序，使输出稳定可比对。
func (s *Simulator) Positions() []Position {
	out := make([]Position, 0, len(s.pos))
	for _, p := range s.pos {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].InstID != out[j].InstID {
			return out[i].InstID < out[j].InstID
		}
		return out[i].PosSide < out[j].PosSide
	})
	return out
}

// MetricsOf 返回某个仓位在当前标记价下的风险指标。
//
// 未设置过标记价时以开仓均价代替，此时未实现盈亏为零——这比返回错误更实用，
// 使用者刚开完仓还没推送行情是很常见的状态。
func (s *Simulator) MetricsOf(instID string, posSide types.PosSide) (Metrics, error) {
	pos, ok := s.PositionOf(instID, posSide)
	if !ok {
		return Metrics{}, nil
	}
	inst, err := s.cfg.RefData.Instrument(instID)
	if err != nil {
		return Metrics{}, err
	}
	tbl, err := refdata.TierTableFor(s.cfg.RefData, inst, pos.MgnMode)
	if err != nil {
		return Metrics{}, err
	}
	tier, err := tbl.Lookup(pos.AbsPos())
	if err != nil {
		return Metrics{}, err
	}
	rate, err := s.fees.Rate(inst)
	if err != nil {
		return Metrics{}, err
	}
	markPx := s.marks[instID]
	if markPx.IsZero() {
		markPx = pos.AvgPx
	}
	return ComputeMetrics(pos, inst, tier, markPx, rate.Taker), nil
}

// Balance 返回某币种的余额快照。
func (s *Simulator) Balance(ccy string) (Balance, error) {
	b := Balance{Ccy: ccy, CashBal: s.cash[ccy]}

	for _, p := range s.pos {
		inst, err := s.cfg.RefData.Instrument(p.InstID)
		if err != nil {
			return Balance{}, err
		}
		if inst.SettleCcy != ccy {
			continue
		}
		markPx := s.marks[p.InstID]
		if markPx.IsZero() {
			markPx = p.AvgPx
		}
		upl := unrealizedPnl(inst, p.SignedPos(), p.AvgPx, markPx)
		b.Upl = b.Upl.Add(upl)
		b.IsoEq = b.IsoEq.Add(p.Margin).Add(upl)
	}

	// 挂单冻结经实测标定：OKX 的 ordFrozen 只是保证金部分，手续费不在其中，
	// 但可用余额是按两者之和扣减的；frozenBal 则同时含逐仓权益与挂单冻结。
	//
	//	无挂单有持仓  availBal = cashBal，frozenBal = isoEq
	//	有开仓挂单    availBal = cashBal − (保证金 + 手续费)
	//	              frozenBal = isoEq + 保证金 + 手续费
	//
	// 平仓方向的挂单不产生冻结，这一点也已实测确认。
	ordMargin, ordFee := s.orderFreeze(ccy)
	b.OrdFrozen = ordMargin
	b.FrozenBal = b.IsoEq.Add(ordMargin).Add(ordFee)
	b.Eq = b.CashBal.Add(b.IsoEq)
	b.AvailBal = b.CashBal.Sub(ordMargin).Sub(ordFee)
	b.AvailEq = b.AvailBal
	return b, nil
}

// Balances 返回全部有余额或有持仓的币种，按币种名排序。
func (s *Simulator) Balances() ([]Balance, error) {
	seen := map[string]bool{}
	for ccy := range s.cash {
		seen[ccy] = true
	}
	for _, p := range s.pos {
		inst, err := s.cfg.RefData.Instrument(p.InstID)
		if err != nil {
			return nil, err
		}
		seen[inst.SettleCcy] = true
	}
	for _, o := range s.pending {
		seen[o.Cost.Ccy] = true
	}

	ccys := make([]string, 0, len(seen))
	for c := range seen {
		ccys = append(ccys, c)
	}
	sort.Strings(ccys)

	out := make([]Balance, 0, len(ccys))
	for _, c := range ccys {
		b, err := s.Balance(c)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// AdjustMargin 增减逐仓保证金，对应 OKX 的
// POST /api/v5/account/position/margin-balance。
//
// 资金在现金余额与仓位保证金之间一比一划转，经实测确认：追加 200 时现金减少
// 200、保证金增加 200，杠杆设置不变，强平价随之远离（62772 -> 57750）。
//
// 减少的下限是【开仓时的初始保证金】，即 Q × 开仓均价 / 杠杆——等价于不允许
// 有效杠杆超过设定杠杆。越过下限时 OKX 返回 59301，本方法与之一致。
//
// 下限按开仓均价而非标记价计算，这一点由实测确定：减到 624.900524 时恰好等于
// 按开仓均价算出的初始保证金，再减即被拒；若按标记价算下限应为 624.96296，
// 比当时的保证金还高，那样当前状态本身就已违规，与实际不符。
func (s *Simulator) AdjustMargin(instID string, posSide types.PosSide,
	op types.MarginOp, amt decimal.Decimal) error {

	if !op.Valid() {
		return okxerr.New(okxerr.CodeParamError, "type: 非法的调整方向 %q", op)
	}
	if !amt.IsPositive() {
		return okxerr.New(okxerr.CodeParamError, "amt: 调整金额须为正数，实为 %s", amt)
	}
	side, err := s.normalizePosSide(posSide)
	if err != nil {
		return err
	}
	key := positionKey{instID, side}
	pos, ok := s.pos[key]
	if !ok {
		return okxerr.New(okxerr.CodeParamError, "%s 没有 %s 方向的持仓", instID, side)
	}
	if pos.MgnMode != types.MgnIsolated {
		return okxerr.New(okxerr.CodeParamError, "只有逐仓仓位才能调整保证金")
	}
	inst, err := s.cfg.RefData.Instrument(instID)
	if err != nil {
		return err
	}

	if op == types.MarginAdd {
		if s.cash[inst.SettleCcy].LessThan(amt) {
			return okxerr.New(okxerr.CodeInsufficientBal,
				"%s 可用余额 %s 不足以追加保证金 %s", inst.SettleCcy, s.cash[inst.SettleCcy], amt)
		}
		s.cash[inst.SettleCcy] = s.cash[inst.SettleCcy].Sub(amt)
		pos.Margin = pos.Margin.Add(amt)
		s.pos[key] = pos
		return nil
	}

	floor := initialMargin(inst, pos.AbsPos(), pos.AvgPx, pos.Lever)
	if pos.Margin.Sub(amt).LessThan(floor) {
		return okxerr.New(okxerr.CodeMarginAdjustExceeds,
			"%s 减少保证金 %s 后将低于开仓初始保证金 %s（当前 %s）",
			instID, amt, floor, pos.Margin)
	}
	s.cash[inst.SettleCcy] = s.cash[inst.SettleCcy].Add(amt)
	pos.Margin = pos.Margin.Sub(amt)
	s.pos[key] = pos
	return nil
}

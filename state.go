package okxsim

import (
	"sort"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

// State 是模拟器全部可变状态的快照，可 JSON 序列化后原样放回。
//
// 参数扫描的断点续跑、事件重放、把一次回测的中途状态存下来事后复盘，都要它。
//
// **完整性是这个类型存在的理由。** 手写搬运代码时最容易漏掉挂单与算法委托——
// 漏了不会报错，只会让续跑时凭空少了几笔在途的委托，而回测结果照样跑得出来。
// 这里把八项可变状态一次装齐：现金、仓位、三条行情、杠杆设置、挂单、算法委托。
//
// 不含的是构造期的配置（持仓方式、规则数据、费率表）——那些由 New 决定，
// 恢复时须放进一个用同样配置构造出来的模拟器，见 Restore。
type State struct {
	// PosMode 与 RefDataVersion 用于在恢复时确认「放回去的地方对不对」，
	// 见 Restore 的说明。
	PosMode        types.PosMode `json:"posMode"`
	RefDataVersion int64         `json:"refDataVersion"`

	Cash      map[string]decimal.Decimal `json:"cash"`
	Positions []Position                 `json:"positions"`

	// 三条行情各存各的。本库从不拿其中一条顶替另一条，存档也一样。
	MarkPx  map[string]decimal.Decimal `json:"markPx,omitempty"`
	LastPx  map[string]decimal.Decimal `json:"lastPx,omitempty"`
	IndexPx map[string]decimal.Decimal `json:"indexPx,omitempty"`

	Leverage      []LeverageSetting `json:"leverage,omitempty"`
	PendingOrders []PendingOrder    `json:"pendingOrders,omitempty"`
	PendingAlgos  []PendingAlgo     `json:"pendingAlgos,omitempty"`
}

// LeverageSetting 是一条杠杆设置。
//
// 杠杆是按 (合约, 保证金模式, 持仓方向) 分别设定的，且在没有持仓时也有意义
// ——先设杠杆再开仓是常见顺序，存档漏掉它会让恢复后的第一笔成交用错杠杆。
type LeverageSetting struct {
	InstID  string          `json:"instId"`
	MgnMode types.MgnMode   `json:"mgnMode"`
	PosSide types.PosSide   `json:"posSide"`
	Lever   decimal.Decimal `json:"lever"`
}

// State 导出模拟器当前的全部可变状态。
//
// 返回的是副本：此后对模拟器的任何操作都不会改动它。
func (s *Simulator) State() State {
	st := State{
		PosMode:        s.cfg.PosMode,
		RefDataVersion: s.cfg.RefData.Version(),
		Cash:           copyPxMap(s.cash),
		MarkPx:         copyPxMap(s.marks),
		LastPx:         copyPxMap(s.last),
		IndexPx:        copyPxMap(s.index),
		Positions:      s.Positions(),
		PendingOrders:  s.PendingOrders(""),
		PendingAlgos:   s.PendingAlgos(""),
	}
	for k, v := range s.lever {
		st.Leverage = append(st.Leverage, LeverageSetting{
			InstID: k.instID, MgnMode: k.mgnMode, PosSide: k.posSide, Lever: v,
		})
	}
	// 排序使同一状态导出的结果逐字节相同——回测要的是可复现，
	// 而 map 的遍历顺序在 Go 里是随机的。
	sort.Slice(st.Leverage, func(i, j int) bool {
		a, b := st.Leverage[i], st.Leverage[j]
		if a.InstID != b.InstID {
			return a.InstID < b.InstID
		}
		if a.MgnMode != b.MgnMode {
			return a.MgnMode < b.MgnMode
		}
		return a.PosSide < b.PosSide
	})
	return st
}

// Restore 把一份状态放回模拟器，**整体替换**原有状态。
//
// 恢复的目标模拟器须用与导出时相同的配置构造，两处不匹配会直接报错而不是
// 将就着跑：
//
//	持仓方式    买卖模式与开平仓模式对 posSide 的取值要求不同，混用会让仓位错位
//	规则数据版本 档位表变了，同一个仓位的维持保证金率就不一样，风险指标随之全错
//
// 后一条是**有意从严**的。确实要跨规则数据版本恢复时，把 State.RefDataVersion
// 清零即表示「我知道规则可能变了」——这一步必须显式做，才不会在参数扫描里
// 悄悄混进两套规则。
func (s *Simulator) Restore(st State) error {
	if st.PosMode != "" && st.PosMode != s.cfg.PosMode {
		return okxerr.New(okxerr.CodeParamError,
			"存档的持仓方式是 %s，当前模拟器是 %s——两者对 posSide 的取值要求不同，"+
				"请用相同的 PosMode 构造模拟器", st.PosMode, s.cfg.PosMode)
	}
	if v := s.cfg.RefData.Version(); st.RefDataVersion != 0 && st.RefDataVersion != v {
		return okxerr.New(okxerr.CodeParamError,
			"存档的规则数据版本是 %d，当前是 %d——档位表若有变动，同一个仓位的"+
				"维持保证金率就不一样。确实要跨版本恢复，请显式把 State.RefDataVersion 清零",
			st.RefDataVersion, v)
	}

	// 先校验再落地：中途失败不该留下半个状态
	for _, p := range st.Positions {
		if _, err := s.cfg.RefData.Instrument(p.InstID); err != nil {
			return err
		}
	}
	for _, o := range st.PendingOrders {
		if _, err := s.cfg.RefData.Instrument(o.Order.InstID); err != nil {
			return err
		}
	}
	for _, a := range st.PendingAlgos {
		if _, err := s.cfg.RefData.Instrument(a.Order.InstID); err != nil {
			return err
		}
		if len(a.Legs) == 0 {
			return okxerr.New(okxerr.CodeParamError,
				"算法委托 %s 没有触发腿——存档不完整", a.AlgoID)
		}
	}

	s.cash = copyPxMap(st.Cash)
	s.marks = copyPxMap(st.MarkPx)
	s.last = copyPxMap(st.LastPx)
	s.index = copyPxMap(st.IndexPx)
	s.pos = make(map[positionKey]Position, len(st.Positions))
	s.lever = make(map[leverageKey]decimal.Decimal, len(st.Leverage))
	s.pending = make(map[string]PendingOrder, len(st.PendingOrders))
	s.algos = make(map[string]pendingAlgo, len(st.PendingAlgos))

	for _, l := range st.Leverage {
		s.lever[leverageKey{l.InstID, l.MgnMode, l.PosSide}] = l.Lever
	}
	for _, p := range st.Positions {
		if err := s.SetPosition(p); err != nil {
			return err
		}
	}
	for _, o := range st.PendingOrders {
		s.pending[o.OrdID] = o
	}
	for _, a := range st.PendingAlgos {
		legs := make([]algoLeg, 0, len(a.Legs))
		for _, l := range a.Legs {
			legs = append(legs, algoLeg{
				kind: l.Kind, px: l.Px, pxType: l.PxType,
				ordPx: l.OrdPx, above: l.Above, trailing: l.Trailing,
			})
		}
		s.algos[a.AlgoID] = pendingAlgo{Order: a.Order, Legs: legs, Extreme: a.Extreme}
	}
	return nil
}

func copyPxMap(m map[string]decimal.Decimal) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

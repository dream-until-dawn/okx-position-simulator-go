package refdata

import (
	"errors"

	"github.com/dream-until-dawn/okx-position-simulator-go/types"
)

// ErrTierTableNotFound 表示找不到指定品种与保证金模式的档位表。
var ErrTierTableNotFound = errors.New("找不到档位表")

// Provider 提供规则数据查询。
//
// 有两类实现，服务于两种截然不同的需求：
//
//   - Snapshot 是不可变快照，Version 恒定不变。回测必须用它——
//     规则一旦在回测途中改变，结果就不再可复现。
//   - live.Provider 会按设定周期自动拉取 OKX 的规则变更，Version 随之推进。
//     实盘或近实时场景用它。
//
// 实现必须支持并发读取。调用方若需要在一次计算内保持规则一致，
// 应当在计算开始时取一次 Version 并在结束时校验其未变，
// 而不是假设多次查询之间规则不会改变。
type Provider interface {
	// Instrument 返回合约规格；不存在时返回错误码 51001 的错误。
	Instrument(instID string) (Instrument, error)

	// TierTable 返回档位表；不存在时返回包装了 ErrTierTableNotFound 的错误。
	//
	// 键中的 Family 是 instFamily 而非 instId：全仓模式下同一 instFamily 的
	// 多个合约持仓必须合并后再查档。
	TierTable(key TierKey) (*TierTable, error)

	// FeeSchedule 返回费率表。
	FeeSchedule() FeeSchedule

	// Version 返回本份规则数据的生成时刻（毫秒）。
	//
	// 它是判断规则是否已变更的唯一依据：值改变即意味着底层数据被刷新过。
	Version() int64
}

// TierTableFor 是按合约查询档位表的便捷封装，从合约规格中取出 instFamily 与
// 产品类型，避免调用方手工拼装 TierKey 时误用 instId。
func TierTableFor(p Provider, inst Instrument, mgnMode types.MgnMode) (*TierTable, error) {
	return p.TierTable(TierKey{
		InstType: inst.InstType,
		MgnMode:  mgnMode,
		Family:   inst.InstFamily,
	})
}

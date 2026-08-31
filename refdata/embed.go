package refdata

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"
)

//go:embed snapshot/embedded.json.gz
var embeddedGz []byte

var (
	embeddedOnce sync.Once
	embeddedSnap *Snapshot
	embeddedErr  error
)

// Embedded 返回随库分发的内置快照。
//
// 它的存在是为了让本库零配置可用：不联网、不准备任何数据文件就能跑起来，
// 且同一个版本的库永远给出同一份规则数据，回测结果因此可复现。
//
// 收录范围有限——按 24 小时成交额取正向合约的头部品种，加上全部反向合约。
// 全量收录约 18 MB，不适合嵌入二进制。所需品种不在其中时，有三条路：
//
//   - 用 cmd/refdata-sync 自行生成一份更大的快照，经 LoadSnapshot 载入
//   - 用 refdata/live 在运行时拉取
//   - 用 SnapshotBuilder 自行装配
//
// 快照内容随库版本固定。要跟随 OKX 的规则变更，用 refdata/live。
//
// 返回的是同一个共享实例。Snapshot 不可变，可安全地并发使用。
func Embedded() (*Snapshot, error) {
	embeddedOnce.Do(func() {
		embeddedSnap, embeddedErr = LoadSnapshot(bytes.NewReader(embeddedGz))
		if embeddedErr != nil {
			embeddedErr = fmt.Errorf("载入内置快照失败: %w", embeddedErr)
		}
	})
	return embeddedSnap, embeddedErr
}

// MustEmbedded 同 Embedded，但在载入失败时 panic。
//
// 内置快照是随二进制分发的固定数据，其损坏属于构建问题而非运行时状况，
// 因此在初始化场景下直接 panic 比层层传递一个不可能发生的错误更合适。
func MustEmbedded() *Snapshot {
	s, err := Embedded()
	if err != nil {
		panic(err)
	}
	return s
}

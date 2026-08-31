// Command refdata-sync 从 OKX 拉取规则数据并生成快照文件。
//
// 它有两个用途：生成随库分发的内置快照，以及让使用者为自己关心的品种生成
// 一份自用快照（回测需要固定的规则数据才能复现结果）。
//
//	go run ./cmd/refdata-sync -out refdata/snapshot/embedded.json.gz
//
// 内置快照不收录全部品种。实测 459 个永续合约，每个品种的逐仓与全仓档位表各
// 约 20 KB 且互不相同，全量收录约 18 MB，压缩后仍有一两兆，嵌进二进制并不合适。
// 因此按 24 小时成交额取正向合约的头部品种，并收录全部反向合约（仅十余个）。
// 需要更多品种的使用者可以自行调大 -top，或用 refdata/live 在运行时拉取。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dream-until-dawn/okx-position-simulator-go/refdata"
	"github.com/dream-until-dawn/okx-position-simulator-go/refdata/live"
	"github.com/dream-until-dawn/okx-position-simulator-go/types"
	"github.com/shopspring/decimal"
)

func main() {
	var (
		out      = flag.String("out", "refdata/snapshot/embedded.json.gz", "输出文件路径")
		topN     = flag.Int("top", 30, "按 24 小时成交额收录的正向合约品种数")
		plain    = flag.Bool("plain", false, "输出未压缩的 JSON（默认 gzip）")
		all      = flag.Bool("all", false, "收录全部品种，忽略 -top")
		baseURL  = flag.String("base-url", live.DefaultBaseURL, "OKX 接口地址")
		interval = flag.Duration("interval", live.DefaultMinInterval, "相邻请求的最小间隔")
		timeout  = flag.Duration("timeout", 30*time.Minute, "总超时")
	)
	flag.Parse()

	if err := run(*out, *topN, *plain, *all, *baseURL, *interval, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "生成快照失败: %v\n", err)
		os.Exit(1)
	}
}

func run(out string, topN int, plain, all bool, baseURL string, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	f := live.NewFetcher(live.WithBaseURL(baseURL), live.WithMinInterval(interval))

	fmt.Println("拉取合约规格…")
	insts, err := f.Instruments(ctx, types.InstSwap)
	if err != nil {
		return err
	}
	fmt.Printf("  取得 %d 个永续合约\n", len(insts))

	fmt.Println("拉取 24 小时成交额…")
	vol, err := f.Turnover24h(ctx, types.InstSwap)
	if err != nil {
		return err
	}

	families, kept := selectFamilies(insts, vol, topN, all)
	fmt.Printf("  收录 %d 个品种（正向按成交额取头部，反向全收）\n", len(families))

	b := refdata.NewSnapshotBuilder(time.Now().UnixMilli()).
		AddInstruments(kept...).
		SetFeeSchedule(refdata.DefaultFeeSchedule())

	fmt.Printf("拉取档位表（%d 个品种 × 逐仓/全仓 = %d 次请求）…\n",
		len(families), len(families)*2)
	var failed []string
	for i, fam := range families {
		for _, mode := range []types.MgnMode{types.MgnCross, types.MgnIsolated} {
			key := refdata.TierKey{InstType: types.InstSwap, MgnMode: mode, Family: fam}
			tbl, err := f.PositionTiers(ctx, key)
			if err != nil {
				// 个别品种可能刚上线或已下架而没有档位表，跳过而不中断整批
				failed = append(failed, fmt.Sprintf("%s: %v", key, err))
				continue
			}
			b.AddTierTable(tbl)
		}
		if (i+1)%10 == 0 || i+1 == len(families) {
			fmt.Printf("  %d/%d\n", i+1, len(families))
		}
	}
	for _, msg := range failed {
		fmt.Fprintf(os.Stderr, "  跳过 %s\n", msg)
	}

	snap := b.Build()
	nInst, nTier := snap.Counts()
	fmt.Printf("快照装配完成：%d 个合约，%d 张档位表\n", nInst, nTier)

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	fh, err := os.Create(out)
	if err != nil {
		return err
	}
	defer fh.Close()

	if plain {
		err = snap.Encode(fh)
	} else {
		err = snap.EncodeGzip(fh)
	}
	if err != nil {
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}

	st, err := os.Stat(out)
	if err != nil {
		return err
	}
	fmt.Printf("已写入 %s（%.1f KB）\n", out, float64(st.Size())/1024)
	if len(failed) > 0 {
		fmt.Printf("有 %d 张档位表拉取失败，见上方输出\n", len(failed))
	}
	return nil
}

// selectFamilies 挑选要收录的品种，并返回这些品种下的全部合约。
//
// 反向合约（币本位）全部收录：实测仅十余个，体积可以忽略，而它们是 v0.5.0
// 要支持的目标之一，缺了就没法开箱验证。正向合约按 24 小时成交额取头部。
func selectFamilies(insts []refdata.Instrument, turnover map[string]decimal.Decimal,
	topN int, all bool) ([]string, []refdata.Instrument) {

	type famVol struct {
		family string
		vol    decimal.Decimal
	}
	var linear []famVol
	inverse := map[string]bool{}
	seen := map[string]bool{}

	for _, i := range insts {
		if i.InstFamily == "" || seen[i.InstFamily] {
			continue
		}
		seen[i.InstFamily] = true
		if i.IsInverse() {
			inverse[i.InstFamily] = true
			continue
		}
		linear = append(linear, famVol{i.InstFamily, turnover[i.InstID]})
	}

	// 成交额降序；相同则按品种名，保证结果稳定可复现
	sort.Slice(linear, func(a, b int) bool {
		if c := linear[a].vol.Cmp(linear[b].vol); c != 0 {
			return c > 0
		}
		return linear[a].family < linear[b].family
	})
	if !all && topN < len(linear) {
		linear = linear[:topN]
	}

	chosen := make(map[string]bool, len(linear)+len(inverse))
	for f := range inverse {
		chosen[f] = true
	}
	for _, fv := range linear {
		chosen[fv.family] = true
	}

	families := make([]string, 0, len(chosen))
	for f := range chosen {
		families = append(families, f)
	}
	sort.Strings(families)

	kept := make([]refdata.Instrument, 0, len(chosen))
	for _, i := range insts {
		if chosen[i.InstFamily] {
			kept = append(kept, i)
		}
	}
	return families, kept
}

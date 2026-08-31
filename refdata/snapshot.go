package refdata

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/dream-until-dawn/okx-position-simulator-go/okxerr"
)

// Snapshot 是某一时刻规则数据的不可变集合，实现 Provider。
//
// 构造完成后内部状态不再改变，因而天然支持并发读取，且 Version 恒定——
// 这正是回测所需要的：规则在回测途中若会变，结果就不可复现。
//
// 零值不可用，须经 SnapshotBuilder 或 LoadSnapshot 构造。
type Snapshot struct {
	ts          int64
	instruments map[string]Instrument
	tiers       map[TierKey]*TierTable
	fees        FeeSchedule
}

// 编译期确认 *Snapshot 满足 Provider。
var _ Provider = (*Snapshot)(nil)

// Instrument 返回合约规格；不存在时返回错误码 51001 的错误，与 OKX 一致。
func (s *Snapshot) Instrument(instID string) (Instrument, error) {
	i, ok := s.instruments[instID]
	if !ok {
		return Instrument{}, okxerr.New(okxerr.CodeInstNotExist,
			"快照(版本 %d)中没有合约 %s", s.ts, instID)
	}
	return i, nil
}

// TierTable 返回档位表。
func (s *Snapshot) TierTable(key TierKey) (*TierTable, error) {
	t, ok := s.tiers[key]
	if !ok {
		return nil, fmt.Errorf("快照(版本 %d)中没有 %s 的档位表: %w",
			s.ts, key, ErrTierTableNotFound)
	}
	return t, nil
}

// FeeSchedule 返回费率表。
func (s *Snapshot) FeeSchedule() FeeSchedule { return s.fees }

// Version 返回快照生成时刻（毫秒）。
func (s *Snapshot) Version() int64 { return s.ts }

// InstrumentIDs 返回快照中全部合约的 instId，按字典序升序。
func (s *Snapshot) InstrumentIDs() []string {
	ids := make([]string, 0, len(s.instruments))
	for id := range s.instruments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TierKeys 返回快照中全部档位表的键，按字典序升序。
func (s *Snapshot) TierKeys() []TierKey {
	keys := make([]TierKey, 0, len(s.tiers))
	for k := range s.tiers {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

// Counts 返回快照的规模，便于日志与排查。
func (s *Snapshot) Counts() (instruments, tierTables int) {
	return len(s.instruments), len(s.tiers)
}

// SnapshotBuilder 分批装配快照。非并发安全，装配完成后调用 Build 取得不可变结果。
type SnapshotBuilder struct {
	ts          int64
	instruments map[string]Instrument
	tiers       map[TierKey]*TierTable
	fees        FeeSchedule
}

// NewSnapshotBuilder 新建装配器，ts 为快照生成时刻（毫秒）。
func NewSnapshotBuilder(ts int64) *SnapshotBuilder {
	return &SnapshotBuilder{
		ts:          ts,
		instruments: make(map[string]Instrument),
		tiers:       make(map[TierKey]*TierTable),
	}
}

// AddInstruments 加入合约规格，instId 重复时后者覆盖前者。
func (b *SnapshotBuilder) AddInstruments(insts ...Instrument) *SnapshotBuilder {
	for _, i := range insts {
		if i.InstID == "" {
			continue
		}
		b.instruments[i.InstID] = i
	}
	return b
}

// AddTierTable 加入档位表。
func (b *SnapshotBuilder) AddTierTable(t *TierTable) *SnapshotBuilder {
	if t != nil {
		b.tiers[t.Key] = t
	}
	return b
}

// SetFeeSchedule 设置费率表。
func (b *SnapshotBuilder) SetFeeSchedule(s FeeSchedule) *SnapshotBuilder {
	b.fees = s
	return b
}

// Build 产出不可变快照。装配器在此之后不应再被使用。
func (b *SnapshotBuilder) Build() *Snapshot {
	return &Snapshot{
		ts:          b.ts,
		instruments: b.instruments,
		tiers:       b.tiers,
		fees:        b.fees,
	}
}

// snapshotFile 是快照的磁盘格式。
//
// 容器结构是本项目自定义的——一份快照要装下多个接口的结果，OKX 并没有对应的
// 单一响应。但其中每个元素仍保持 OKX 的线格式（数值为字符串等），
// 因此单个合约或档位可以直接与真实 API 响应逐字段比对。
type snapshotFile struct {
	Ts            string                    `json:"ts"`
	Instruments   []Instrument              `json:"instruments"`
	PositionTiers map[string][]PositionTier `json:"positionTiers"`
	TradeFees     []TradeFee                `json:"tradeFees"`
}

// Encode 以 JSON 写出快照。
//
// 合约按 instId、档位表按键排序后写出，使同样的数据产生逐字节相同的文件，
// 快照因而可以纳入版本控制并做有意义的 diff。
//
// 刻意不叫 WriteTo：那个名字会被误认为实现了 io.WriterTo，而本方法不返回写入
// 字节数，语义也不是「把自身作为字节流写出」。
func (s *Snapshot) Encode(w io.Writer) error {
	f := snapshotFile{
		Ts:            formatMillis(s.ts),
		PositionTiers: make(map[string][]PositionTier, len(s.tiers)),
	}
	for _, id := range s.InstrumentIDs() {
		f.Instruments = append(f.Instruments, s.instruments[id])
	}
	for _, k := range s.TierKeys() {
		f.PositionTiers[k.String()] = s.tiers[k].Tiers
	}
	for _, it := range s.fees.instTypesSorted() {
		fee, _ := s.fees.TradeFee(it)
		f.TradeFees = append(f.TradeFees, fee)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}

// EncodeGzip 以 gzip 压缩后写出快照。
//
// 档位表数据高度重复，压缩后体积约为原始的一成，内置快照因此得以嵌入二进制。
func (s *Snapshot) EncodeGzip(w io.Writer) error {
	zw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	if err := s.Encode(zw); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

// LoadSnapshot 读入快照，自动识别输入是否为 gzip 压缩。
func LoadSnapshot(r io.Reader) (*Snapshot, error) {
	br := bufio.NewReader(r)
	if head, err := br.Peek(2); err == nil && head[0] == 0x1f && head[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("解压快照失败: %w", err)
		}
		defer zr.Close()
		return decodeSnapshot(zr)
	}
	return decodeSnapshot(br)
}

func decodeSnapshot(r io.Reader) (*Snapshot, error) {
	var f snapshotFile
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return nil, fmt.Errorf("解析快照失败: %w", err)
	}
	ts, err := parseMillis("ts", f.Ts)
	if err != nil {
		return nil, err
	}

	b := NewSnapshotBuilder(ts).
		AddInstruments(f.Instruments...).
		SetFeeSchedule(NewFeeSchedule(f.TradeFees...))

	for raw, tiers := range f.PositionTiers {
		key, err := ParseTierKey(raw)
		if err != nil {
			return nil, fmt.Errorf("解析快照失败: %w", err)
		}
		tbl, err := NewTierTable(key, tiers)
		if err != nil {
			return nil, fmt.Errorf("解析快照失败: %w", err)
		}
		b.AddTierTable(tbl)
	}
	return b.Build(), nil
}

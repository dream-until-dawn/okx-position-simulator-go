// Package okxsim 用 Go 实现 OKX 的仓位管理内核，供策略回测引擎调用。
//
// 一句话概括职责：「行情与成交 → 仓位/账户状态」的记账机。
//
// 它不预测价格、不产生资金费率、不模拟盘口深度——那些是市场结果或市场数据。
// 它做的是：给定一笔成交或一根 K 线，算出仓位、盈亏、手续费、保证金与风险指标
// 会变成什么样，并且算得与 OKX 逐字段一致。
//
// # 覆盖范围
//
// 正向与币本位 × 逐仓与全仓，四个象限：
//
//	账户模式  acctLv=2 现货合约模式
//	持仓方式  买卖模式（net）与开平仓模式（long/short）
//	合约      永续与交割，正向（linear）与币本位（inverse）
//	保证金    逐仓（isolated）与全仓（cross）
//
// 明确排除在 v1.0 之外：期权、跨币种保证金（acctLv=3）、组合保证金（acctLv=4）、
// ADL 自动减仓。原因与取舍见 docs/roadmap.md。
//
// # 最小用法
//
//	sim, _ := okxsim.New(okxsim.Config{
//		PosMode:      types.NetMode,
//		RefData:      refdata.MustEmbedded(),   // 内置快照，零配置可用
//		DefaultLever: decimal.NewFromInt(5),
//	})
//	sim.Deposit("USDT", decimal.NewFromInt(10000))
//
//	sim.Fill(okxsim.Fill{ /* ... */ })          // 引擎自行撮合时灌成交
//	sim.Advance(okxsim.Bar{ /* ... */ })        // 或用内置撮合推进一根 K 线
//
//	m, _ := sim.MetricsOf("BTC-USDT-SWAP", types.PosNet)
//	b, _ := sim.BalanceOf("USDT")
//
// Advance 一步之内的顺序是确定的：资金费 → 算法委托 → 撮合 → 强平。
//
// # 命名约定
//
//	XxxPx / SetXxxPx   行情读写成对出现：MarkPx、LastPx、IndexPx
//	XxxOf(key)         按键取一个复合结果：BalanceOf、PositionOf、MetricsOf、
//	                   CrossMetricsOf、PendingOrderOf、PendingAlgoOf
//	Xxxs()             取全部：Positions、Balances、PendingOrders、PendingAlgos
//
// 返回标量的访问器不带 Of（MarkPx、CashBal、Leverage）。
//
// # 值的表示
//
// 一切金额与价格用 github.com/shopspring/decimal，不用 float64——OKX 的 avgPx
// 有 16 位小数，浮点在第一次加仓就会开始漂。
//
// 面向 OKX 的视图（PositionView、BalanceView）把数值序列化为**字符串**，并且
// 区分「零」与「无此值」：OKX 用空串表示后者，把它和 "0" 混为一谈，会让
// 「没有强平价」和「强平价是零」分不开。
//
// # 依赖
//
// 核心包的依赖树只有 github.com/shopspring/decimal 一个。net/http 隔离在
// refdata/live 子包，对拍工具是独立的嵌套模块，用不到就不会进入依赖图。
//
// # 规则的来源
//
// 公式与错误码都在 OKX 模拟盘上跑过真实操作核对，包括真实触发一次强平。
// 文档里查不到、只能实测出来的规则有二十余条，逐条记在 docs/okx-rules.md 里
// 并标注了证据等级（实测 / 文档 / 未定）。证据不足的地方在代码与文档中明确标注，
// 并留有守卫测试，在前提悄悄失效时报警。
//
// 「100% 模拟」若不能被自动验证就只是一句口号。cmd/conformance 是那个验证：
// 它把本库的输出与 OKX 的原始 JSON 逐字段比对，每个字段要么值对得上，要么在
// policy.go 里写明为什么不建模。
package okxsim

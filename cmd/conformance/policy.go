package main

// 本文件是「本库有意不建模哪些字段，以及为什么」的唯一声明处。
//
// view 模式拿它当白名单：OKX 响应里的每个字段，要么本库建模了并且值对得上，
// 要么在这里写明理由。两样都没有就判失败。
//
// 这样做的意义在于把「没建模」从隐性遗漏变成显式决定。挑着比字段，永远发现不了
// 「有个字段我压根没建模」；而 OKX 日后新增字段时，这里也会立刻报出来。
//
// 理由要具体到能让人判断该不该改主意，别写「用不上」。

// positionFieldPolicy 声明 GET /api/v5/account/positions 里本库不建模的字段。
var positionFieldPolicy = map[string]string{
	// —— 期权 ——
	// 期权明确排除在 v1.0 之外，见 roadmap.md
	"deltaBS":   "期权希腊字母（BS 口径），期权排除在 v1.0 之外",
	"deltaPA":   "期权希腊字母（PA 口径），同上",
	"gammaBS":   "期权希腊字母，同上",
	"gammaPA":   "期权希腊字母，同上",
	"thetaBS":   "期权希腊字母，同上",
	"thetaPA":   "期权希腊字母，同上",
	"vegaBS":    "期权希腊字母，同上",
	"vegaPA":    "期权希腊字母，同上",
	"optVal":    "期权市值，期权排除在 v1.0 之外",
	"estMaxWin": "期权预估最大盈利，同上",

	// —— 借贷与跨币种 ——
	// 借币计息是 acctLv≥3 的能力，本库只做 acctLv=2
	"liab":                   "负债额，借币是 acctLv≥3 的事",
	"liabCcy":                "负债币种，借币是 acctLv≥3 的事",
	"interest":               "借币计息，是 acctLv≥3 的能力",
	"baseBorrowed":           "交易币已借，同上",
	"baseInterest":           "交易币计息，同上",
	"quoteBorrowed":          "计价币已借，同上",
	"quoteInterest":          "计价币计息，同上",
	"pendingCloseOrdLiabVal": "平仓挂单的负债价值，同上",

	// —— 现货相关 ——
	// 现货对冲需要现货持仓，本库只做衍生品
	"baseBal":         "交易币余额，现货相关",
	"quoteBal":        "计价币余额，现货相关",
	"spotInUseAmt":    "现货对冲占用量，现货相关",
	"spotInUseCcy":    "现货对冲币种，同上",
	"clSpotInUseAmt":  "用户自定义现货占用量，同上",
	"maxSpotInUseAmt": "最大现货对冲占用量，同上",

	// —— 交割与结算 ——
	"settledPnl":     "已结算收益，交割合约到期结算才有值；本库不建模交割结算",
	"nonSettleAvgPx": "未结算均价，同上",

	// —— 其余 ——
	"bizRefId":       "用户自定义业务参考 ID，由下单方给出，与核算无关",
	"bizRefType":     "业务参考类型，同上",
	"ruleType":       "交易规则类型（normal / pre_market），是合约的属性而非仓位状态",
	"closeAllFlag":   "止盈止损是否全部平仓的标志，属算法委托的参数而非仓位状态",
	"closeOrderAlgo": "挂在该仓位上的止盈止损委托。本库的算法委托独立成表（AlgoOrders），不挂回仓位视图——两处表达同一件事只会不同步",
	"hedgedPos":      "组合保证金模式（acctLv=4）下的对冲仓位标记，该模式排除在 v1.0 之外",
}

// balanceFieldPolicy 声明 GET /api/v5/account/balance 的 details 里本库不建模的字段。
var balanceFieldPolicy = map[string]string{
	// —— 借贷与跨币种 ——
	"liab":          "负债额，借币是 acctLv≥3 的事",
	"crossLiab":     "全仓负债额，借币是 acctLv≥3 的事",
	"isoLiab":       "逐仓负债额，借币是 acctLv≥3 的事",
	"interest":      "借币计息，是 acctLv≥3 的能力",
	"maxLoan":       "最大可借额度，借币是 acctLv≥3 的事",
	"borrowFroz":    "借币冻结额度，借币是 acctLv≥3 的事",
	"uplLiab":       "由负债导致的未实现盈亏，同上",
	"eqUsd":         "美元计价权益，同上",
	"notionalLever": "杠杆倍数（账户口径），由跨币种折算得出，同上",

	// —— 现货相关 ——
	"spotBal":        "现货余额，本库只做衍生品",
	"spotUpl":        "现货未实现盈亏，同上",
	"spotUplRatio":   "现货未实现收益率，同上",
	"spotInUseAmt":   "现货对冲占用量，同上",
	"clSpotInUseAmt": "用户自定义现货占用量，同上",
	"maxSpotInUse":   "最大现货对冲占用，同上",
	"spotIsoBal":     "现货逐仓余额，同上",
	"openAvgPx":      "现货开仓均价，同上",
	"accAvgPx":       "现货累计均价，同上",
	"totalPnl":       "现货总盈亏，同上",
	"totalPnlRatio":  "现货总收益率，同上",

	// —— 理财、跟单等增值功能 ——
	"autoLendStatus":        "自动借出状态，理财功能",
	"autoLendAmt":           "自动借出数量，同上",
	"autoLendMtAmt":         "自动借出已匹配数量，同上",
	"autoStakingStatus":     "自动质押状态，同上",
	"rewardBal":             "体验金余额，模拟盘/活动相关",
	"stgyEq":                "策略权益，跟单与策略功能",
	"smtSyncEq":             "跟单同步权益，同上",
	"spotCopyTradingEq":     "现货跟单权益，同上",
	"twap":                  "TWAP 减仓档位，属风控告警而非账务",
	"fixedBal":              "affirmed 稳定资产，理财相关",
	"colRes":                "质押保留额度，抵押借币相关",
	"colBorrAutoConversion": "质押借币自动转换，同上",
	"collateralEnabled":     "是否作为保证金，跨币种折算相关",
	"collateralRestrict":    "是否受质押限制，跨币种折算相关",
	"frpType":               "浮动利率类型，借贷相关",

	// —— 时间戳 ——
	"uTime": "余额更新时间，由交易所侧的写入时刻决定，模拟器无从对应",
}

// blankPolicy 声明本库【建模了但有意恒留空】的字段。
//
// 与 fieldPolicy 的区别：那些是结构体里压根没有的字段，这些是有字段但填不出值。
// 分开是因为两者的含义不同——前者是「不在本库范围内」，后者是「在范围内但模拟器
// 无从得知」。混在一起会掩盖后一类里潜在的真实缺口。
var positionBlankPolicy = map[string]string{
	"posId":       "仓位 ID 由交易所生成，模拟器无从对应",
	"tradeId":     "最后一笔成交的 ID，同上",
	"adl":         "自动减仓等级，排队依赖全市场持仓与收益率，回测环境无法还原（roadmap 已明确排除）",
	"usdPx":       "保证金币种对美元的价格，属行情数据，本库不持有",
	"notionalUsd": "美元计价的名义价值，需要 usdPx 才能折算，同上",
}

var balanceBlankPolicy = map[string]string{
	"disEq":    "美元层面的折算权益，需要该币种对美元的价格，属行情数据",
	"mgnRatio": "没有全仓持仓时 OKX 也返回空串，此为一致行为而非缺口",
}

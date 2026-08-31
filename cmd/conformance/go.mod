// 对拍工具是独立的嵌套模块。
//
// 它需要 OKX SDK 才能发起已签名的请求，而主模块承诺「核心包的依赖树只有
// shopspring/decimal 一个」。若把 SDK 写进主模块的 go.mod，即便使用者从不引用
// 本工具，该依赖仍会出现在他们的模块图里，那条承诺就不成立了。
//
// 代价是根目录的 go build ./... 与 go test ./... 不会包含本目录，需单独进入执行。
module github.com/dream-until-dawn/okx-position-simulator-go/cmd/conformance

go 1.22

replace github.com/dream-until-dawn/okx-position-simulator-go => ../..

require (
	github.com/dream-until-dawn/okx-api-v5-go v0.1.0
	github.com/dream-until-dawn/okx-position-simulator-go v0.0.0-00010101000000-000000000000
	github.com/shopspring/decimal v1.4.0
)

require github.com/gorilla/websocket v1.5.3 // indirect

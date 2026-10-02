module github.com/fouc3/qq-group-management-bot

go 1.27.1

require (
	github.com/fouc3/onebot-ext v0.0.0
	github.com/fouc3/qq-bot-sdk v0.0.0-00010101000000-000000000000
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/redis/go-redis/v9 v9.22.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/sashabaranov/go-openai v1.43.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
	modernc.org/sqlite v1.60.1 // indirect
)

// 本地开发期指向同级的 SDK 源码，便于同步改动。
// 若要改用已发布版本：删掉这行，然后
//   go mod edit -dropreplace=github.com/fouc3/qq-bot-sdk
//   go get github.com/fouc3/qq-bot-sdk@main
replace github.com/fouc3/qq-bot-sdk => ../QQ-Bot-CLI/qq-bot-sdk

// OneBot 扩展是仓库内的独立子模块，见 onebot-ext/。
replace github.com/fouc3/onebot-ext => ./onebot-ext

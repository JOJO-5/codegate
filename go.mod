module github.com/jojo/codegate

// 用 1.24 作为语言版本下限：规范要求 Go 1.24+，声明低一点能让更多贡献者参与构建。
// 本机工具链是 1.27.1，会以 1.24 语言语义编译。若要升到新语言特性，改这一行即可。
go 1.26.0

require (
	github.com/BurntSushi/toml v1.5.0
	github.com/creack/pty v1.1.24
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/klauspost/compress v1.18.0
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
	modernc.org/sqlite v1.59.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

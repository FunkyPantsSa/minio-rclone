// go.mod —— 模块定义与依赖锁定
// 说明：
//   - 独立运行模式（web + rclone），不再依赖 Kubernetes 客户端；
//   - HTTP 层使用 Go 标准库 net/http（含 Go 1.22 ServeMux 路由），无需第三方 Web 框架；
//   - 任务持久化使用纯 Go 的 modernc.org/sqlite（无 CGO 依赖，支持 CGO_ENABLED=0 静态构建）；
//   - 定时调度使用 github.com/robfig/cron/v3。

module github.com/example/minio-rclone-backup-controller

// go 1.22+：路由使用标准库 http.ServeMux 的方法+路径通配符语法（http.PathValue）
go 1.22

require (
	github.com/robfig/cron/v3 v3.0.1
	modernc.org/sqlite v1.34.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.22.0 // indirect
	modernc.org/gc/v3 v3.0.0-20240107210532-573471604cb6 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	modernc.org/strutil v1.2.0 // indirect
	modernc.org/token v1.1.0 // indirect
)

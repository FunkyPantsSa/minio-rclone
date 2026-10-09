package main

// main.go —— 程序入口（独立运行模式：web + rclone）
//
// 启动流程：
//  1. 加载配置（环境变量，见 config.go）
//  2. 初始化 SQLite（任务记录 + 设置项），把上次运行遗留的 running 任务标记为中断
//  3. 创建任务运行器（直接 exec rclone）与内置定时调度器
//  4. 校验备份目标目录可写、凭证是否配置（页面可见）
//  5. 注册 API 路由并启动 HTTP 服务（默认 :8000，可用 PORT 环境变量覆盖）
//
// 部署方式（任选其一）：
//  - 直接运行在任意装有 rclone 的主机/容器上（docker run -v 挂载备份目录与数据目录）；
//  - 或部署到 Kubernetes（deploy/deployment.yaml，仅作为普通容器运行，不创建任何 K8s 资源）。

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	// 1. 加载配置
	cfg := LoadConfig()
	log.Printf("配置加载完成：源=%s，目标目录=%s，数据目录=%s，定时计划=%s，带宽=%s，并发 transfers=%d/checkers=%d",
		cfg.Source(), cfg.BackupDestDir, cfg.DataDir, cfg.CronSchedule, cfg.BWLimit,
		cfg.Transfers, cfg.Checkers)

	// 2. 初始化 SQLite
	db, err := OpenDB(cfg.DataDir)
	if err != nil {
		log.Fatalf("[启动失败] 初始化数据库出错: %v", err)
	}
	defer db.Close()
	if n, err := db.MarkInterruptedOnStartup(); err == nil && n > 0 {
		log.Printf("检测到 %d 个因上次退出未完成的任务，已标记为中断", n)
	} else if err != nil {
		log.Printf("[警告] 标记中断任务失败: %v", err)
	}

	// 3. 创建任务运行器 + 内置定时调度器
	runner, err := NewRunner(cfg, db)
	if err != nil {
		log.Fatalf("[启动失败] 初始化任务运行器出错: %v", err)
	}
	scheduler, err := NewScheduler(runner)
	if err != nil {
		log.Fatalf("[启动失败] 定时计划无效: %v", err)
	}
	scheduler.Start()

	// 4. 目录与凭证自检（页面「当前配置」面板可查看）
	if !dirWritable(cfg.BackupDestDir) {
		log.Printf("[警告] 备份目标目录 %s 不存在或不可写，备份可能失败，请检查目录权限", cfg.BackupDestDir)
	}
	if !cfg.CredentialsReady() {
		log.Printf("[警告] 未配置 AK/SK（AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY）。" +
			"若 rclone.conf 使用 env_auth=true，备份将因无法获取凭证而失败")
	}
	if cfg.LoginPassword == "admin123" {
		log.Printf("[安全提醒] 正在使用默认登录密码（admin/admin123），上线前请务必通过 LOGIN_PASSWORD 修改")
	}

	// 5. 注册路由 + 启动服务
	app := NewApp(cfg, db, runner, scheduler)
	mux := http.NewServeMux()
	app.Routes(mux)

	// 监听地址：默认 :8000，可用 PORT 环境变量覆盖
	addr := ":" + os.Getenv("PORT")
	if addr == ":" {
		addr = ":8000"
	}

	// 中间件链：访问日志 ← 登录认证（/login 与 /api/login 放行）
	server := &http.Server{
		Addr:              addr,
		Handler:           accessLogMiddleware(app.authMiddleware(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("MinIO 备份控制台（web + rclone 独立模式）已启动，监听地址 %s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP 服务异常退出: %v", err)
	}
}

// accessLogMiddleware 简单的访问日志中间件，便于排查 API 调用
func accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start))
	})
}

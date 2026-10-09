package main

// handlers.go —— HTTP API 处理函数
//
// 路由总览（使用 Go 1.22+ 的 ServeMux 方法+路径通配符语法）：
//
//	GET    /login                         登录页面
//	POST   /api/login                     登录（下发会话 Cookie）
//	POST   /api/logout                    退出登录
//	GET    /                              前端 HTML 页面
//	POST   /api/full-backup               启动全量备份（直接 exec rclone）
//	POST   /api/incremental-backup        手动触发一次增量备份
//	POST   /api/cronjob/suspend           暂停/恢复内置定时调度，请求体 {"suspend": true|false}
//	GET    /api/cronjob/status            定时调度详情（计划/上次/下次执行/历史保留）
//	GET    /api/tasks                     任务列表（SQLite 持久化）
//	GET    /api/tasks/{name}/logs?tail=N  静态获取最近 N 行日志
//	GET    /api/tasks/{name}/logs/stream  SSE 流式实时日志
//	GET    /api/tasks/{name}/progress     实时备份进度
//	DELETE /api/tasks/{name}              删除任务记录与日志
//	GET    /api/config                    当前生效配置（敏感字段不返回）
//
// 所有接口返回 JSON；错误统一返回 {"error": "..."}。

import (
	_ "embed" // go:embed 编译期嵌入 templates/index.html 与 login.html
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/index.html
var indexHTML string

//go:embed templates/login.html
var loginHTML string

// App 持有依赖（配置 / 数据库 / 任务运行器 / 调度器 / 认证器）
type App struct {
	cfg       *Config
	db        *DB
	runner    *Runner
	scheduler *Scheduler
	auth      *Auth
}

// NewApp 创建 App 实例
func NewApp(cfg *Config, db *DB, runner *Runner, scheduler *Scheduler) *App {
	return &App{cfg: cfg, db: db, runner: runner, scheduler: scheduler, auth: NewAuth(cfg)}
}

// Routes 注册全部 API 路由
// 认证说明：/login 与 /api/login 放行，其余路由由 authMiddleware 统一校验会话
func (a *App) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", a.handleLoginPage)
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	mux.HandleFunc("GET /", a.handleIndex)
	mux.HandleFunc("POST /api/full-backup", a.handleFullBackup)
	mux.HandleFunc("POST /api/incremental-backup", a.handleIncrementalBackup)
	mux.HandleFunc("POST /api/cronjob/suspend", a.handleCronJobSuspend)
	mux.HandleFunc("GET /api/cronjob/status", a.handleCronJobStatus)
	mux.HandleFunc("GET /api/tasks", a.handleListTasks)
	mux.HandleFunc("GET /api/tasks/{name}/logs", a.handleTaskLogs)
	mux.HandleFunc("GET /api/tasks/{name}/logs/stream", a.handleTaskLogStream)
	mux.HandleFunc("GET /api/tasks/{name}/progress", a.handleTaskProgress)
	mux.HandleFunc("DELETE /api/tasks/{name}", a.handleDeleteTask)
	mux.HandleFunc("GET /api/config", a.handleConfig)
}

// ---------------------------------------------------------------------------
// JSON 响应辅助
// ---------------------------------------------------------------------------

// writeJSON 统一 JSON 响应
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 统一错误响应 {"error": "..."}
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---------------------------------------------------------------------------
// 页面
// ---------------------------------------------------------------------------

// handleIndex GET / —— 返回内嵌的前端 HTML 页面
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

// ---------------------------------------------------------------------------
// 登录认证
// ---------------------------------------------------------------------------

// authMiddleware 登录认证中间件：
//   - GET /login 与 POST /api/login 放行；
//   - 其余路由要求有效会话 Cookie；
//   - API 未登录返回 401 JSON；页面请求重定向到 /login。
func (a *App) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || r.URL.Path == "/api/login" {
			next.ServeHTTP(w, r)
			return
		}
		if a.isAuthed(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

// isAuthed 校验请求携带的会话 Cookie 是否有效
func (a *App) isAuthed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	_, ok := a.auth.verify(c.Value)
	return ok
}

// handleLoginPage GET /login —— 登录页面（已登录则直接跳回首页）
func (a *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if a.isAuthed(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(loginHTML))
}

// handleLogin POST /api/login —— 登录校验，成功后下发会话 Cookie
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体必须是 JSON，例如 {\"username\":\"admin\",\"password\":\"...\"}")
		return
	}
	if !a.auth.check(req.Username, req.Password) {
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    a.auth.issue(req.Username),
		Path:     "/",
		HttpOnly: true, // 禁止 JS 读取，防 XSS 窃取会话
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "登录成功"})
}

// handleLogout POST /api/logout —— 退出登录
// 说明：会话是无状态的 HMAC Cookie，退出仅清除客户端 Cookie。
func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:   cookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "已退出登录"})
}

// ---------------------------------------------------------------------------
// 配置展示
// ---------------------------------------------------------------------------

// ConfigView 返回给前端的配置视图（登录密码 / 签名密钥 / AK-SK 等敏感字段已剔除）
type ConfigView struct {
	*Config
	CredentialsReady bool `json:"credentialsReady"` // AK/SK 是否已配置
	DestDirReady     bool `json:"destDirReady"`     // 备份目标目录是否可写
}

// handleConfig GET /api/config —— 返回当前生效的全部配置（登录后可见）
func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ConfigView{
		Config:           a.cfg,
		CredentialsReady: a.cfg.CredentialsReady(),
		DestDirReady:     dirWritable(a.cfg.BackupDestDir),
	})
}

// dirWritable 探测目录是否可写（创建临时文件后删除）
func dirWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".bc-write-test")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

// ---------------------------------------------------------------------------
// 备份任务触发
// ---------------------------------------------------------------------------

// handleFullBackup POST /api/full-backup —— 启动一次性全量备份
//
// 规则：
//   - 同一时间只允许一个备份任务运行，否则返回 409；
//   - 触发全量前若定时调度处于运行状态，先暂停调度器（避免定时增量与全量并发），
//     全量结束后由后台 goroutine 自动恢复；若触发前用户本就暂停，则保持暂停。
func (a *App) handleFullBackup(w http.ResponseWriter, r *http.Request) {
	if id, name := a.runner.CurrentRunning(); id != "" {
		writeError(w, http.StatusConflict, "已有备份任务 "+name+" 运行中，请等待其完成后再触发全量备份")
		return
	}
	wasSuspended := a.runner.Suspended()
	if !wasSuspended {
		if err := a.runner.SetSuspended(true); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("暂停定时调度失败: %v", err))
			return
		}
	}

	rec, err := a.runner.Start(TaskTypeFull)
	if err != nil {
		if !wasSuspended {
			_ = a.runner.SetSuspended(false) // 创建失败：恢复触发前状态
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("启动全量备份失败: %v", err))
		return
	}

	// 全量结束后自动恢复定时调度（触发前在运行才恢复）
	if !wasSuspended {
		go a.resumeSchedulerAfterTask(rec.ID)
	}

	msg := "全量备份任务已启动"
	if !wasSuspended {
		msg += "，定时增量调度已暂停（全量完成后自动恢复）"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message": msg,
		"name":    rec.Name,
	})
}

// handleIncrementalBackup POST /api/incremental-backup —— 手动触发一次增量备份
// 复用同一份 rclone 参数（--max-duration 25m），与定时增量等价，仅记录类型不同。
func (a *App) handleIncrementalBackup(w http.ResponseWriter, r *http.Request) {
	if id, name := a.runner.CurrentRunning(); id != "" {
		writeError(w, http.StatusConflict, "已有备份任务 "+name+" 运行中，请等待其完成后再触发增量备份")
		return
	}
	rec, err := a.runner.Start(TaskTypeManual)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("触发增量备份失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "增量备份任务已启动",
		"name":    rec.Name,
	})
}

// resumeSchedulerAfterTask 等待任务结束后自动恢复定时调度。
// 说明：触发全量时调度器被自动暂停；本函数在任务结束时无条件恢复。
// 若触发前用户本就手动暂停（wasSuspended=true），则不会挂载本函数，保持暂停。
func (a *App) resumeSchedulerAfterTask(id string) {
	done, ok := a.runner.DoneChan(id)
	if !ok {
		return
	}
	<-done
	_ = a.runner.SetSuspended(false)
	log.Printf("全量备份结束，已自动恢复定时增量调度")
}

// ---------------------------------------------------------------------------
// 定时调度 暂停 / 状态
// ---------------------------------------------------------------------------

// handleCronJobSuspend POST /api/cronjob/suspend —— 暂停/恢复内置定时调度
func (a *App) handleCronJobSuspend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suspend bool `json:"suspend"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体必须是 JSON，例如 {\"suspend\": true}")
		return
	}
	if err := a.runner.SetSuspended(req.Suspend); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	state := "已恢复"
	if req.Suspend {
		state = "已暂停"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "定时增量备份" + state,
		"suspend": fmt.Sprintf("%v", req.Suspend),
	})
}

// handleCronJobStatus GET /api/cronjob/status —— 定时调度完整状态
func (a *App) handleCronJobStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.scheduler.Status())
}

// ---------------------------------------------------------------------------
// 任务列表 / 日志 / 进度 / 删除
// ---------------------------------------------------------------------------

// TaskView 返回给前端任务表格的摘要
type TaskView struct {
	Name           string   `json:"name"`           // 任务名（= ID）
	Type           string   `json:"type"`           // full / incremental / manual-incremental
	TypeLabel      string   `json:"typeLabel"`      // 全量备份 / 定时增量 / 手动增量
	Status         string   `json:"status"`         // 运行中 / 成功 / 失败 / 中断
	StatusLabel    string   `json:"statusLabel"`    // 与 status 相同（兼容旧前端）
	StartTime      string   `json:"startTime"`      // 开始时间（RFC3339）
	CompletionTime *string  `json:"completionTime"` // 结束时间
	CreatedAt      string   `json:"createdAt"`      // 开始时间（兼容旧前端）
	ExitCode       *int     `json:"exitCode"`       // 退出码
	Error          string   `json:"error"`          // 失败原因
	Progress       Progress `json:"progress"`       // 最终/实时进度快照
}

// taskTypeLabel 类型中文名
func taskTypeLabel(t string) string {
	switch t {
	case TaskTypeFull:
		return "全量备份"
	case TaskTypeIncremental:
		return "定时增量"
	case TaskTypeManual:
		return "手动增量"
	}
	return t
}

// taskStatusLabel 状态中文名
func taskStatusLabel(s string) string {
	switch s {
	case StatusRunning:
		return "运行中"
	case StatusSuccess:
		return "成功"
	case StatusFailed:
		return "失败"
	case StatusInterrupted:
		return "中断"
	}
	return s
}

// taskView 把持久化记录转换为前端视图
func taskView(rec *TaskRecord) TaskView {
	v := TaskView{
		Name:        rec.Name,
		Type:        rec.Type,
		TypeLabel:   taskTypeLabel(rec.Type),
		Status:      taskStatusLabel(rec.Status),
		StatusLabel: taskStatusLabel(rec.Status),
		StartTime:   rec.StartTime.Format(time.RFC3339),
		CreatedAt:   rec.StartTime.Format(time.RFC3339),
		ExitCode:    rec.ExitCode,
		Error:       rec.Error,
		Progress:    progressFromRecord(rec),
	}
	if rec.EndTime != nil {
		s := rec.EndTime.Format(time.RFC3339)
		v.CompletionTime = &s
	}
	return v
}

// handleListTasks GET /api/tasks —— 列出所有任务（含状态与最终进度）
func (a *App) handleListTasks(w http.ResponseWriter, r *http.Request) {
	recs, err := a.db.ListTasks(200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]TaskView, 0, len(recs))
	for i := range recs {
		views = append(views, taskView(&recs[i]))
	}
	writeJSON(w, http.StatusOK, views)
}

// parseTail 解析 tail 查询参数：默认 200 行，限制在 1~10000
func parseTail(r *http.Request) int64 {
	tail := int64(200)
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			tail = n
		}
	}
	if tail > 10000 {
		tail = 10000
	}
	return tail
}

// taskLogPath 返回任务日志文件路径
func (a *App) taskLogPath(name string) string {
	return filepath.Join(a.runner.LogsDir(), name+".log")
}

// handleTaskLogs GET /api/tasks/{name}/logs?tail=200 —— 静态获取最近 N 行日志
func (a *App) handleTaskLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path := a.taskLogPath(name)
	if !fileExists(path) {
		writeError(w, http.StatusNotFound, "任务 "+name+" 不存在或无日志")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取日志失败")
		return
	}
	lines := strings.Split(string(data), "\n")
	tail := int(parseTail(r))
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(strings.Join(lines, "\n")))
}

// handleTaskLogStream GET /api/tasks/{name}/logs/stream —— SSE 流式实时日志
//
// 协议：text/event-stream，轮询日志文件新增字节并输出 data: 事件；
// 任务结束且日志不再增长时发送 done 事件并关闭连接。
func (a *App) handleTaskLogStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path := a.taskLogPath(name)
	if !fileExists(path) {
		writeError(w, http.StatusNotFound, "任务 "+name+" 不存在或无日志")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前响应不支持流式输出")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关闭 Nginx 等反向代理对 SSE 的缓冲

	// 支持 ?since=<字节偏移>：只从指定偏移开始增量输出
	offset := int64(0)
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			offset = n
		}
	}

	writeSSE := func(s string) {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.TrimSpace(line) != "" {
				fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\x00", ""))
			}
		}
		flusher.Flush()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	idle := 0
	for {
		select {
		case <-r.Context().Done():
			return // 客户端断开
		case <-ticker.C:
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			st, _ := f.Stat()
			size := st.Size()
			if size > offset {
				if _, err := f.Seek(offset, 0); err == nil {
					buf := make([]byte, 64*1024)
					n, _ := f.Read(buf)
					writeSSE(string(buf[:n]))
					offset += int64(n)
					idle = 0
				}
			} else {
				idle++
			}
			f.Close()

			// 任务已结束且连续 2 个周期无新日志 → 输出 done 事件并关闭
			if !a.runner.IsRunning(name) && idle >= 2 {
				fmt.Fprint(w, "event: done\ndata: {}\n\n")
				flusher.Flush()
				return
			}
		}
	}
}

// handleTaskProgress GET /api/tasks/{name}/progress —— 实时/最终备份进度
func (a *App) handleTaskProgress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, running, err := a.runner.Progress(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// 附加当前状态供前端展示
	writeJSON(w, http.StatusOK, struct {
		Progress
		Running bool `json:"running"`
	}{Progress: p, Running: running})
}

// handleDeleteTask DELETE /api/tasks/{name} —— 删除任务记录与日志文件
func (a *App) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if a.runner.IsRunning(name) {
		writeError(w, http.StatusConflict, "任务 "+name+" 正在运行，无法删除")
		return
	}
	if err := a.db.DeleteTask(name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	_ = os.Remove(a.taskLogPath(name))
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务 " + name + " 已删除"})
}

package main

// runner.go —— 备份任务运行器 + 内置定时调度器
//
// 架构（独立运行模式，不再经过 Kubernetes）：
//   - Web 应用直接 exec rclone 进程执行备份（全量 / 定时增量 / 手动增量共用 rclone copy）；
//   - 同一时间只允许运行一个备份任务（全量/增量互斥，等价于原 CronJob 的 Forbid 语义：
//     重叠的定时调度直接跳过，不排队、不并发）；
//   - 任务日志写入 DATA_DIR/logs/<任务名>.log；rclone --use-json-log 的 stats 行
//     被实时解析并回写 SQLite，供页面展示实时进度（修复了原来进度一直为空的问题）；
//   - 定时调度由内置 robfig/cron 驱动，暂停/恢复状态持久化在 SQLite settings 表，
//     应用重启后仍保持用户设置的暂停状态。

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Runner 管理 rclone 进程与任务生命周期
type Runner struct {
	cfg *Config
	db  *DB

	mu        sync.Mutex
	running   map[string]*RunningTask // id -> 运行中的任务
	activeID  string                  // 当前运行中的任务 id（同一时间最多一个）
	suspended bool                    // 定时调度暂停标志（持久化在 settings 表）
	logsDir   string
}

// RunningTask 描述一个正在运行的 rclone 进程
type RunningTask struct {
	ID        string
	Name      string
	Type      string
	StartTime time.Time
	cmd       *exec.Cmd
	logFile   *os.File
	done      chan struct{} // 进程结束（wait goroutine）后关闭
	mu        sync.Mutex
	progress  Progress // 最近一次 stats 快照
}

// NewRunner 创建运行器：初始化日志目录，并从数据库恢复调度暂停状态
func NewRunner(cfg *Config, db *DB) (*Runner, error) {
	logsDir := filepath.Join(cfg.DataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	suspended := cfg.CronSuspend
	if v, ok := db.GetSetting(settingSuspend); ok {
		suspended = v == "true"
	}
	return &Runner{
		cfg:       cfg,
		db:        db,
		running:   map[string]*RunningTask{},
		suspended: suspended,
		logsDir:   logsDir,
	}, nil
}

// LogsDir 返回任务日志目录（供日志接口使用）
func (r *Runner) LogsDir() string { return r.logsDir }

// fileExists 判断文件是否存在
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// buildRcloneArgs 构造 rclone copy 命令参数（exec 形式，不经过 shell）
// 增量任务追加 --max-duration，保证单次运行自行结束。
func (r *Runner) buildRcloneArgs(incremental bool) []string {
	cfg := r.cfg
	args := []string{"copy", cfg.Source(), cfg.BackupDestDir}
	if cfg.RcloneConfigPath != "" && fileExists(cfg.RcloneConfigPath) {
		args = append(args, "--config", cfg.RcloneConfigPath)
	}
	args = append(args,
		"--log-level", cfg.LogLevel,
		"--bwlimit", cfg.BWLimit,
		"--transfers", strconv.Itoa(cfg.Transfers),
		"--checkers", strconv.Itoa(cfg.Checkers),
		"--retries", strconv.Itoa(cfg.Retries),
		"--retries-sleep", cfg.RetriesSleep,
		"--stats", cfg.StatsInterval,
	)
	if cfg.FastList {
		args = append(args, "--fast-list")
	}
	if cfg.UseServerModtime {
		args = append(args, "--use-server-modtime")
	}
	if cfg.UseJSONLog {
		args = append(args, "--use-json-log")
	}
	if incremental {
		maxDur := cfg.IncrementalMaxDuration
		if maxDur != "" {
			args = append(args, "--max-duration", maxDur)
		}
	}
	return args
}

// taskName 生成带时间戳 + 随机后缀的任务名（也是任务 ID 与日志文件名）
// 随机后缀避免同一秒内先后启动的任务名称冲突（DB 主键 / 日志文件名唯一）
func taskName(taskType string) string {
	prefix := map[string]string{
		TaskTypeFull:        "rclone-full-backup",
		TaskTypeIncremental: "rclone-incr",
		TaskTypeManual:      "rclone-manual",
	}[taskType]
	if prefix == "" {
		prefix = "rclone-task"
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s", prefix, time.Now().Format("20060102-150405"), hex.EncodeToString(b))
}

// CurrentRunning 返回当前运行中任务（ID, 名称）；无则返回空串
func (r *Runner) CurrentRunning() (id, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.running[r.activeID]; ok {
		return t.ID, t.Name
	}
	return "", ""
}

// IsRunning 判断指定任务是否仍在运行
func (r *Runner) IsRunning(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.running[id]
	return ok
}

// DoneChan 返回任务结束信号通道（用于“全量结束后恢复定时调度”）
func (r *Runner) DoneChan(id string) (<-chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.running[id]
	if !ok {
		return nil, false
	}
	return t.done, true
}

// Suspended 返回定时调度是否暂停
func (r *Runner) Suspended() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.suspended
}

// SetSuspended 设置定时调度暂停状态并持久化
func (r *Runner) SetSuspended(v bool) error {
	r.mu.Lock()
	r.suspended = v
	r.mu.Unlock()
	value := "false"
	if v {
		value = "true"
	}
	if err := r.db.SetSetting(settingSuspend, value); err != nil {
		return err
	}
	return nil
}

// Start 启动一个备份任务（taskType: full / incremental / manual-incremental）。
// 同一时间只允许一个任务运行，否则返回错误。
func (r *Runner) Start(taskType string) (*TaskRecord, error) {
	r.mu.Lock()
	if r.activeID != "" {
		name := r.running[r.activeID].Name
		r.mu.Unlock()
		return nil, fmt.Errorf("已有备份任务 %s 运行中，请等待其完成后再触发", name)
	}
	r.mu.Unlock()

	// 校验 rclone 可执行文件
	bin := r.cfg.RcloneBin
	if !strings.Contains(bin, "/") {
		p, err := exec.LookPath(bin)
		if err != nil {
			return nil, fmt.Errorf("未找到 rclone 可执行文件（%s），请安装 rclone 或配置 RCLONE_BIN", bin)
		}
		bin = p
	}

	incremental := taskType != TaskTypeFull
	name := taskName(taskType)
	logPath := filepath.Join(r.logsDir, name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("创建日志文件失败: %w", err)
	}

	cmd := exec.Command(bin, r.buildRcloneArgs(incremental)...)
	// 凭证：rclone.conf 使用 env_auth=true 时，通过环境变量注入 AK/SK（不落盘、不进命令行）
	if r.cfg.AccessKeyID != "" && r.cfg.SecretAccessKey != "" {
		cmd.Env = append(os.Environ(),
			"AWS_ACCESS_KEY_ID="+r.cfg.AccessKeyID,
			"AWS_SECRET_ACCESS_KEY="+r.cfg.SecretAccessKey,
		)
	}

	// stdout 直接落日志文件；stderr 由 consumeStderr 逐行处理（写日志 + 解析进度）
	cmd.Stdout = &lockedWriter{w: logFile}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("创建 stderr 管道失败: %w", err)
	}

	rec := &TaskRecord{ID: name, Name: name, Type: taskType, StartTime: time.Now()}
	rt := &RunningTask{
		ID: name, Name: name, Type: taskType,
		StartTime: rec.StartTime, cmd: cmd, logFile: logFile,
		done: make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("启动 rclone 失败: %w", err)
	}

	// 注册为运行中
	r.mu.Lock()
	r.running[name] = rt
	r.activeID = name
	r.mu.Unlock()

	if err := r.db.InsertTask(rec); err != nil {
		log.Printf("[警告] 写入任务记录失败: %v", err)
	}

	// stderr 解析 + 进程等待（两个后台 goroutine）
	go r.consumeStderr(rt, stderr)
	go r.wait(rt)

	log.Printf("备份任务已启动：%s（类型=%s，日志=%s）", name, taskType, logPath)
	return rec, nil
}

// consumeStderr 读取 rclone stderr：原始行写入日志文件；JSON stats 行解析为实时进度
func (r *Runner) consumeStderr(rt *RunningTask, rdr io.Reader) {
	scanner := bufio.NewScanner(rdr)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	w := &lockedWriter{w: rt.logFile}
	for scanner.Scan() {
		line := scanner.Text()
		_, _ = io.WriteString(w, line+"\n")
		if p, ok := parseStatsLine(line); ok {
			rt.mu.Lock()
			rt.progress = p
			rt.mu.Unlock()
			if err := r.db.UpdateProgress(rt.ID, p); err != nil {
				log.Printf("[警告] 更新进度失败（%s）: %v", rt.ID, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[警告] 读取 rclone 日志出错（%s）: %v", rt.ID, err)
	}
}

// wait 等待进程结束并落最终状态
func (r *Runner) wait(rt *RunningTask) {
	defer close(rt.done)

	status := StatusSuccess
	var exitCode *int
	errMsg := ""
	err := rt.cmd.Wait()
	rt.logFile.Close()
	if err != nil {
		status = StatusFailed
		if ee, ok := err.(*exec.ExitError); ok {
			c := ee.ExitCode()
			exitCode = &c
		}
		errMsg = err.Error()
	}

	rt.mu.Lock()
	final := rt.progress
	rt.mu.Unlock()

	r.mu.Lock()
	delete(r.running, rt.ID)
	if r.activeID == rt.ID {
		r.activeID = ""
	}
	r.mu.Unlock()

	if err := r.db.FinishTask(rt.ID, status, exitCode, errMsg, final); err != nil {
		log.Printf("[警告] 结束任务记录失败（%s）: %v", rt.ID, err)
	}
	log.Printf("备份任务结束：%s → %s（耗时 %s）", rt.Name, status, time.Since(rt.StartTime).Round(time.Second))

	// 历史保留清理（顺带删除过期任务的日志文件）
	if ids, err := r.db.CleanupHistory(r.cfg.SuccessfulHistoryLimit, r.cfg.FailedHistoryLimit); err == nil {
		for _, id := range ids {
			_ = os.Remove(filepath.Join(r.logsDir, id+".log"))
		}
	} else {
		log.Printf("[警告] 清理历史任务失败: %v", err)
	}
}

// Progress 返回任务进度：运行中返回实时快照；已结束返回数据库中的最终快照。
// 返回 (进度, 是否运行中, 错误)。
func (r *Runner) Progress(id string) (Progress, bool, error) {
	r.mu.Lock()
	rt, ok := r.running[id]
	r.mu.Unlock()
	if ok {
		rt.mu.Lock()
		p := rt.progress
		rt.mu.Unlock()
		return p, true, nil
	}
	rec, err := r.db.GetTask(id)
	if err != nil {
		return Progress{}, false, err
	}
	return progressFromRecord(rec), false, nil
}

// progressFromRecord 从任务记录还原进度（用于已结束任务的进度展示）
func progressFromRecord(rec *TaskRecord) Progress {
	p := Progress{
		Bytes:          rec.BytesDone,
		TotalBytes:     rec.BytesTotal,
		Transfers:      rec.TransfersDone,
		TotalTransfers: rec.TransfersTotal,
		ElapsedTime:    rec.Elapsed,
		Speed:          rec.Speed,
		Percent:        rec.Percent,
	}
	p.SpeedHuman = humanSpeed(rec.Speed)
	return p
}

// lockedWriter 串行化多个 goroutine 对同一日志文件的写入
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// ---------------------------------------------------------------------------
// 内置定时调度器（等价于原 CronJob；Forbid 语义）
// ---------------------------------------------------------------------------

// Scheduler 内置定时增量调度
type Scheduler struct {
	runner *Runner
	sched  cron.Schedule
}

// NewScheduler 解析 cron 表达式并创建调度器
func NewScheduler(r *Runner) (*Scheduler, error) {
	sched, err := cron.ParseStandard(r.cfg.CronSchedule)
	if err != nil {
		return nil, fmt.Errorf("定时计划解析失败（%s）: %w", r.cfg.CronSchedule, err)
	}
	return &Scheduler{runner: r, sched: sched}, nil
}

// Start 启动定时调度
func (s *Scheduler) Start() {
	c := cron.New()
	if _, err := c.AddFunc(s.runner.cfg.CronSchedule, s.tick); err != nil {
		log.Printf("[警告] 注册定时任务失败: %v", err)
		return
	}
	c.Start()
	log.Printf("内置定时增量调度已启动，计划 %s（暂停时跳过；已有任务运行中时本次调度跳过）",
		s.runner.cfg.CronSchedule)
}

// tick 定时触发：暂停则跳过；已有任务运行则跳过（Forbid）；否则启动定时增量
func (s *Scheduler) tick() {
	if s.runner.Suspended() {
		log.Printf("定时调度已暂停，跳过本次触发")
		return
	}
	if id, name := s.runner.CurrentRunning(); id != "" {
		log.Printf("已有任务 %s 运行中，跳过本次定时增量（Forbid）", name)
		return
	}
	rec, err := s.runner.Start(TaskTypeIncremental)
	if err != nil {
		log.Printf("[警告] 定时增量启动失败: %v", err)
		return
	}
	log.Printf("定时增量备份已启动：%s", rec.Name)
}

// CronJobStatus 返回给前端“定时任务”面板的状态（字段名保持与原 CronJob 版本一致）
type CronJobStatus struct {
	Exists                     bool    `json:"exists"`
	Schedule                   string  `json:"schedule"`
	Suspend                    bool    `json:"suspend"`
	LastScheduleTime           *string `json:"lastScheduleTime"`
	NextScheduleTime           string  `json:"nextScheduleTime"`
	LastSuccessfulTime         *string `json:"lastSuccessfulTime"`
	ConcurrencyPolicy          string  `json:"concurrencyPolicy"`
	ActiveJobs                 int     `json:"activeJobs"`
	SuccessfulJobsHistoryLimit int32   `json:"successfulJobsHistoryLimit"`
	FailedJobsHistoryLimit     int32   `json:"failedJobsHistoryLimit"`
}

// Status 汇总定时调度状态
func (s *Scheduler) Status() CronJobStatus {
	cfg := s.runner.cfg
	st := CronJobStatus{
		Exists:                     true,
		Schedule:                   cfg.CronSchedule,
		Suspend:                    s.runner.Suspended(),
		NextScheduleTime:           s.sched.Next(time.Now()).Format(time.RFC3339),
		ConcurrencyPolicy:          "Forbid",
		SuccessfulJobsHistoryLimit: cfg.SuccessfulHistoryLimit,
		FailedJobsHistoryLimit:     cfg.FailedHistoryLimit,
	}
	if t, err := s.runner.db.LatestIncrementalStart(); err == nil && t != nil {
		v := t.Format(time.RFC3339)
		st.LastScheduleTime = &v
	}
	if t, err := s.runner.db.LatestIncrementalSuccess(); err == nil && t != nil {
		v := t.Format(time.RFC3339)
		st.LastSuccessfulTime = &v
	}
	if n, err := s.runner.db.CountRunning(); err == nil {
		st.ActiveJobs = n
	}
	return st
}

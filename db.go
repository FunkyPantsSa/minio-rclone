package main

// db.go —— SQLite 持久化（任务记录 + 设置项）
//
// 架构：独立运行模式（web + rclone），不再查询 Kubernetes；
// 所有备份任务的元数据与最终进度写入 SQLite，日志内容以文件形式存放在
// DATA_DIR/logs/ 下。数据库文件位于 DATA_DIR/backup-controller.db。
//
// 说明：
//   - 使用纯 Go 的 modernc.org/sqlite 驱动（无 CGO 依赖，适合 CGO_ENABLED=0 静态构建）；
//   - SQLite 为单写模型，此处限制 MaxOpenConns=1 并启用 WAL 提高并发读；
//   - 任务 ID 即任务名称（带时间戳，全局唯一），日志文件名 = 任务名 + ".log"。

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// 任务类型
const (
	TaskTypeFull        = "full"              // 全量备份
	TaskTypeIncremental = "incremental"       // 定时增量（内置调度器触发）
	TaskTypeManual      = "manual-incremental" // 手动增量
)

// 任务状态
const (
	StatusRunning     = "running"     // 运行中
	StatusSuccess     = "success"     // 成功
	StatusFailed      = "failed"      // 失败
	StatusInterrupted = "interrupted" // 中断（应用重启导致进程终止）
)

// settings 表键
const settingSuspend = "scheduler_suspend" // 定时调度暂停标志："true" / "false"

// TaskRecord 持久化到 SQLite 的任务记录
type TaskRecord struct {
	ID             string
	Name           string
	Type           string
	Status         string
	StartTime      time.Time
	EndTime        *time.Time
	ExitCode       *int
	Error          string
	BytesTotal     int64
	BytesDone      int64
	TransfersTotal int64
	TransfersDone  int64
	Speed          float64
	Elapsed        float64
	Percent        float64
}

// DB 封装 SQLite 访问
type DB struct {
	db *sql.DB
}

// OpenDB 打开（必要时创建）数据库并初始化表结构
func OpenDB(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	path := filepath.Join(dataDir, "backup-controller.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	conn.SetMaxOpenConns(1) // SQLite 单写者模型
	_, _ = conn.Exec("PRAGMA journal_mode=WAL;")
	_, _ = conn.Exec("PRAGMA busy_timeout=5000;")

	schema := `
CREATE TABLE IF NOT EXISTS tasks (
	id              TEXT PRIMARY KEY,
	name            TEXT NOT NULL,
	type            TEXT NOT NULL,
	status          TEXT NOT NULL,
	start_time      TEXT NOT NULL,
	end_time        TEXT,
	exit_code       INTEGER,
	error           TEXT,
	bytes_total     INTEGER DEFAULT 0,
	bytes_done      INTEGER DEFAULT 0,
	transfers_total INTEGER DEFAULT 0,
	transfers_done  INTEGER DEFAULT 0,
	speed           REAL,
	elapsed         REAL,
	percent         REAL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("初始化数据库失败: %w", err)
	}
	return &DB{db: conn}, nil
}

// Close 关闭数据库
func (d *DB) Close() error { return d.db.Close() }

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// InsertTask 新建任务记录（初始状态 running）
func (d *DB) InsertTask(rec *TaskRecord) error {
	_, err := d.db.Exec(`INSERT INTO tasks
		(id, name, type, status, start_time, bytes_total, bytes_done, transfers_total, transfers_done, speed, elapsed, percent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Name, rec.Type, StatusRunning, fmtTime(rec.StartTime),
		rec.BytesTotal, rec.BytesDone, rec.TransfersTotal, rec.TransfersDone,
		rec.Speed, rec.Elapsed, rec.Percent)
	if err != nil {
		return fmt.Errorf("写入任务记录失败: %w", err)
	}
	return nil
}

// UpdateProgress 运行中更新进度快照（供页面实时展示与任务结束回写）
func (d *DB) UpdateProgress(id string, p Progress) error {
	_, err := d.db.Exec(`UPDATE tasks SET
		bytes_total=?, bytes_done=?, transfers_total=?, transfers_done=?, speed=?, elapsed=?, percent=?
		WHERE id=?`,
		p.TotalBytes, p.Bytes, p.TotalTransfers, p.Transfers, p.Speed, p.ElapsedTime, p.Percent, id)
	if err != nil {
		return fmt.Errorf("更新任务进度失败: %w", err)
	}
	return nil
}

// FinishTask 任务结束时写入最终状态与进度
func (d *DB) FinishTask(id, status string, exitCode *int, errMsg string, p Progress) error {
	now := time.Now()
	_, err := d.db.Exec(`UPDATE tasks SET
		status=?, end_time=?, exit_code=?, error=?,
		bytes_total=?, bytes_done=?, transfers_total=?, transfers_done=?, speed=?, elapsed=?, percent=?
		WHERE id=?`,
		status, fmtTime(now), exitCode, errMsg,
		p.TotalBytes, p.Bytes, p.TotalTransfers, p.Transfers, p.Speed, p.ElapsedTime, p.Percent, id)
	if err != nil {
		return fmt.Errorf("结束任务记录失败: %w", err)
	}
	return nil
}

// MarkInterruptedOnStartup 应用启动时，把遗留的 running 记录标记为 interrupted
// （应用重启后 rclone 子进程必然已终止，这些任务不可能继续运行）
func (d *DB) MarkInterruptedOnStartup() (int, error) {
	res, err := d.db.Exec(`UPDATE tasks SET status=?, end_time=?, error=? WHERE status=?`,
		StatusInterrupted, fmtTime(time.Now()), "应用重启，备份进程已终止", StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("标记中断任务失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ListTasks 列出最近 limit 条任务（新→旧）
func (d *DB) ListTasks(limit int) ([]TaskRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.db.Query(`SELECT id,name,type,status,start_time,end_time,exit_code,error,
		bytes_total,bytes_done,transfers_total,transfers_done,speed,elapsed,percent
		FROM tasks ORDER BY start_time DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询任务列表失败: %w", err)
	}
	defer rows.Close()

	var out []TaskRecord
	for rows.Next() {
		var r TaskRecord
		var start, end, errText sql.NullString // error 列在任务运行中为 NULL
		var exit sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Name, &r.Type, &r.Status, &start, &end, &exit,
			&errText, &r.BytesTotal, &r.BytesDone, &r.TransfersTotal, &r.TransfersDone,
			&r.Speed, &r.Elapsed, &r.Percent); err != nil {
			return nil, fmt.Errorf("读取任务记录失败: %w", err)
		}
		r.Error = errText.String
		r.StartTime = parseTime(start.String)
		if end.Valid && end.String != "" {
			t := parseTime(end.String)
			r.EndTime = &t
		}
		if exit.Valid {
			v := int(exit.Int64)
			r.ExitCode = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetTask 按 ID 查询单条任务
func (d *DB) GetTask(id string) (*TaskRecord, error) {
	recs, err := d.ListTasks(-1)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if recs[i].ID == id {
			return &recs[i], nil
		}
	}
	return nil, fmt.Errorf("任务 %s 不存在", id)
}

// DeleteTask 删除任务记录
func (d *DB) DeleteTask(id string) error {
	res, err := d.db.Exec(`DELETE FROM tasks WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("删除任务记录失败: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("任务 %s 不存在", id)
	}
	return nil
}

// GetSetting 读取设置项
func (d *DB) GetSetting(key string) (string, bool) {
	var v string
	err := d.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

// SetSetting 写入设置项
func (d *DB) SetSetting(key, value string) error {
	_, err := d.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("写入设置失败: %w", err)
	}
	return nil
}

// CleanupHistory 按保留数量清理历史任务记录，返回被删除的任务 ID（调用方据此清理日志文件）。
// 规则：成功的任务最多保留 successfulLimit 条，失败/中断最多保留 failedLimit 条。
func (d *DB) CleanupHistory(successfulLimit, failedLimit int32) ([]string, error) {
	var removed []string
	clean := func(status string, keep int32) error {
		if keep < 0 {
			return nil
		}
		rows, err := d.db.Query(`SELECT id FROM tasks WHERE status=? ORDER BY start_time DESC LIMIT -1 OFFSET ?`, status, keep)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if _, err := d.db.Exec(`DELETE FROM tasks WHERE id=?`, id); err != nil {
				return err
			}
			removed = append(removed, id)
		}
		return rows.Err()
	}
	if err := clean(StatusSuccess, successfulLimit); err != nil {
		return nil, fmt.Errorf("清理成功历史失败: %w", err)
	}
	if err := clean(StatusFailed, failedLimit); err != nil {
		return nil, fmt.Errorf("清理失败历史失败: %w", err)
	}
	if err := clean(StatusInterrupted, failedLimit); err != nil {
		return nil, fmt.Errorf("清理中断历史失败: %w", err)
	}
	return removed, nil
}

// LatestIncrementalStart / LatestIncrementalSuccess / CountRunning 供定时任务状态面板使用
func (d *DB) LatestIncrementalStart() (*time.Time, error) {
	var s string
	err := d.db.QueryRow(`SELECT start_time FROM tasks
		WHERE type IN (?, ?) ORDER BY start_time DESC LIMIT 1`, TaskTypeIncremental, TaskTypeManual).Scan(&s)
	if err != nil {
		return nil, nil // 无记录
	}
	t := parseTime(s)
	return &t, nil
}

func (d *DB) LatestIncrementalSuccess() (*time.Time, error) {
	var s string
	err := d.db.QueryRow(`SELECT start_time FROM tasks
		WHERE type IN (?, ?) AND status=? ORDER BY start_time DESC LIMIT 1`,
		TaskTypeIncremental, TaskTypeManual, StatusSuccess).Scan(&s)
	if err != nil {
		return nil, nil
	}
	t := parseTime(s)
	return &t, nil
}

func (d *DB) CountRunning() (int, error) {
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status=?`, StatusRunning).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

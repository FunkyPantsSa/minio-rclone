package main

// db_test.go —— SQLite 持久化的单元测试（使用临时数据目录）

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDBTaskLifecycle(t *testing.T) {
	db := newTestDB(t)

	rec := &TaskRecord{ID: "rclone-full-backup-20261008-100000", Name: "rclone-full-backup-20261008-100000", Type: TaskTypeFull, StartTime: time.Now()}
	if err := db.InsertTask(rec); err != nil {
		t.Fatalf("插入任务失败: %v", err)
	}

	// 进度更新
	p := Progress{Bytes: 10, TotalBytes: 100, Transfers: 1, TotalTransfers: 5, Percent: 10, Speed: 1024, ElapsedTime: 1.5}
	if err := db.UpdateProgress(rec.ID, p); err != nil {
		t.Fatalf("更新进度失败: %v", err)
	}

	// 结束
	if err := db.FinishTask(rec.ID, StatusSuccess, nil, "", p); err != nil {
		t.Fatalf("结束任务失败: %v", err)
	}

	// 查询
	got, err := db.GetTask(rec.ID)
	if err != nil {
		t.Fatalf("查询任务失败: %v", err)
	}
	if got.Status != StatusSuccess {
		t.Errorf("状态应为 success，实际 %s", got.Status)
	}
	if got.BytesDone != 10 || got.BytesTotal != 100 {
		t.Errorf("进度未正确持久化: %+v", got)
	}
	if got.EndTime == nil {
		t.Error("EndTime 不应为空")
	}

	// 列表（新→旧）
	recs, err := db.ListTasks(10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("列表应为 1 条，实际 %d（err=%v）", len(recs), err)
	}

	// 删除
	if err := db.DeleteTask(rec.ID); err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}
	if _, err := db.GetTask(rec.ID); err == nil {
		t.Error("删除后不应再查到该任务")
	}
}

func TestDBMarkInterrupted(t *testing.T) {
	db := newTestDB(t)
	rec := &TaskRecord{ID: "t1", Name: "t1", Type: TaskTypeIncremental, StartTime: time.Now()}
	_ = db.InsertTask(rec)
	n, err := db.MarkInterruptedOnStartup()
	if err != nil || n != 1 {
		t.Fatalf("应标记 1 条任务为中断，实际 %d（err=%v）", n, err)
	}
	got, _ := db.GetTask("t1")
	if got.Status != StatusInterrupted {
		t.Errorf("状态应为 interrupted，实际 %s", got.Status)
	}
}

func TestDBSettings(t *testing.T) {
	db := newTestDB(t)
	if _, ok := db.GetSetting(settingSuspend); ok {
		t.Error("初始不应有该设置")
	}
	if err := db.SetSetting(settingSuspend, "true"); err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}
	if v, ok := db.GetSetting(settingSuspend); !ok || v != "true" {
		t.Errorf("读取设置失败: %v %v", v, ok)
	}
}

func TestDBCleanupHistory(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().Add(-time.Hour)
	// 4 条成功 + 3 条失败
	for i := 0; i < 4; i++ {
		id := "ok-" + string(rune('a'+i))
		rec := &TaskRecord{ID: id, Name: id, Type: TaskTypeIncremental, StartTime: base.Add(time.Duration(i) * time.Minute)}
		_ = db.InsertTask(rec)
		_ = db.FinishTask(id, StatusSuccess, nil, "", Progress{})
	}
	for i := 0; i < 3; i++ {
		id := "fail-" + string(rune('a'+i))
		rec := &TaskRecord{ID: id, Name: id, Type: TaskTypeIncremental, StartTime: base.Add(time.Duration(i) * time.Minute)}
		_ = db.InsertTask(rec)
		_ = db.FinishTask(id, StatusFailed, nil, "boom", Progress{})
	}
	removed, err := db.CleanupHistory(2, 2)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	// 应删除 2 条成功 + 1 条失败（各保留 2 条）
	if len(removed) != 3 {
		t.Errorf("应删除 3 条，实际 %d（%v）", len(removed), removed)
	}
	recs, _ := db.ListTasks(100)
	success, failed := 0, 0
	for _, r := range recs {
		switch r.Status {
		case StatusSuccess:
			success++
		case StatusFailed:
			failed++
		}
	}
	if success != 2 || failed != 2 {
		t.Errorf("保留数错误：成功=%d 失败=%d", success, failed)
	}
}

func TestDBListWithNullError(t *testing.T) {
	db := newTestDB(t)
	// 任务刚启动、尚未结束时，error 列是 NULL —— 列表读取不能因此报错
	rec := &TaskRecord{ID: "running-1", Name: "running-1", Type: TaskTypeIncremental, StartTime: time.Now()}
	if err := db.InsertTask(rec); err != nil {
		t.Fatalf("插入任务失败: %v", err)
	}
	recs, err := db.ListTasks(10)
	if err != nil {
		t.Fatalf("读取含 NULL error 的任务列表失败: %v", err)
	}
	if len(recs) != 1 || recs[0].Error != "" {
		t.Errorf("列表结果异常: %+v", recs)
	}
	if recs[0].Status != StatusRunning {
		t.Errorf("状态应为 running，实际 %s", recs[0].Status)
	}
}

func TestParseStatsLine(t *testing.T) {
	// 单行 stats 解析
	p, ok := parseStatsLine(`{"bytes":5242880,"totalBytes":104857600,"transfers":1,"totalTransfers":20,"errors":0,"elapsedTime":10.5,"speed":524288,"transferring":[]}`)
	if !ok {
		t.Fatal("stats 行应解析成功")
	}
	if p.Percent < 4.9 || p.Percent > 5.1 {
		t.Errorf("Percent 应约为 5，实际 %f", p.Percent)
	}
	// 普通日志行 / 非 JSON 行应失败
	if _, ok := parseStatsLine(`{"level":"info","msg":"hi"}`); ok {
		t.Error("普通日志行不应被识别为 stats")
	}
	if _, ok := parseStatsLine("not json"); ok {
		t.Error("非 JSON 行不应被识别为 stats")
	}
}

func TestTaskNameUnique(t *testing.T) {
	a := taskName(TaskTypeFull)
	b := taskName(TaskTypeFull)
	if a == b {
		t.Error("任务名应包含时间戳以保证唯一")
	}
	if !filepath.HasPrefix(a, "rclone-full-backup-") {
		t.Errorf("全量任务名前缀不符: %s", a)
	}
}

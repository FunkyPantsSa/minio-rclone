package main

// progress_test.go —— rclone JSON 进度解析的单元测试

import (
	"strings"
	"testing"
)

// 模拟一段 rclone --use-json-log 日志：
// 包含普通日志行（level/msg）、一条中间 stats 行、一条最新 stats 行，以及非 JSON 行
const sampleLog = `2024-06-01 10:00:00 container starting...
{"level":"info","msg":"S3 bucket bucket1: reading root directory","time":"2024-06-01T10:00:01Z"}
{"bytes":5242880,"totalBytes":104857600,"transfers":1,"totalTransfers":20,"errors":0,"elapsedTime":10.5,"speed":524288,"transferring":[]}
{"level":"info","msg":"Copied: file1.bin","time":"2024-06-01T10:00:02Z"}
not a json line at all
{"bytes":20971520,"totalBytes":104857600,"transfers":4,"totalTransfers":20,"errors":1,"elapsedTime":42.0,"speed":524288,"transferring":[]}
`

func TestParseProgressLines(t *testing.T) {
	p, found := parseProgressLines(sampleLog)
	if !found {
		t.Fatal("期望找到进度记录，实际 found=false")
	}
	// 应取最后一条有效 stats 行
	if p.Bytes != 20971520 {
		t.Errorf("Bytes 应为 20971520，实际 %d", p.Bytes)
	}
	if p.TotalBytes != 104857600 {
		t.Errorf("TotalBytes 应为 104857600，实际 %d", p.TotalBytes)
	}
	if p.Transfers != 4 {
		t.Errorf("Transfers 应为 4，实际 %d", p.Transfers)
	}
	if p.Errors != 1 {
		t.Errorf("Errors 应为 1，实际 %d", p.Errors)
	}
	// percent = bytes / totalBytes * 100 = 20
	if p.Percent < 19.9 || p.Percent > 20.1 {
		t.Errorf("Percent 应约为 20，实际 %f", p.Percent)
	}
	if !strings.Contains(p.SpeedHuman, "KiB/s") {
		t.Errorf("SpeedHuman 应包含 KiB/s，实际 %q", p.SpeedHuman)
	}
}

func TestParseProgressLinesEmpty(t *testing.T) {
	// 空日志 / 仅普通日志行时不应返回 found
	if _, found := parseProgressLines(""); found {
		t.Error("空日志不应返回进度记录")
	}
	if _, found := parseProgressLines(`{"level":"info","msg":"hello"}`); found {
		t.Error("普通日志行不应被识别为进度记录")
	}
}

func TestParseProgressLinesZeroTotal(t *testing.T) {
	// totalBytes 为 0（尚未开始统计）时百分比应为 0，且不应 panic
	p, found := parseProgressLines(`{"bytes":0,"totalBytes":0,"transfers":0,"totalTransfers":0,"errors":0,"elapsedTime":0.5,"speed":0,"transferring":[]}`)
	if !found {
		t.Fatal("期望找到进度记录")
	}
	if p.Percent != 0 {
		t.Errorf("Percent 应为 0，实际 %f", p.Percent)
	}
}

func TestHumanSpeed(t *testing.T) {
	cases := []struct {
		bps  float64
		want string
	}{
		{500, "500 B/s"},
		{512000, "500.0 KiB/s"},
		{5242880, "5.0 MiB/s"},
		{1073741824, "1.0 GiB/s"},
	}
	for _, c := range cases {
		if got := humanSpeed(c.bps); got != c.want {
			t.Errorf("humanSpeed(%v) = %q, 期望 %q", c.bps, got, c.want)
		}
	}
}

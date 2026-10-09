package main

// progress.go —— rclone JSON 日志进度解析
//
// rclone 使用 --use-json-log + --stats 5s 时，会周期性输出一行 JSON 格式的统计快照，形如：
//
//	{"bytes":123456,"checking":{...},"elapsedTime":30.5,"errors":0,"eta":120,
//	 "speed":512000,"totalBytes":99999999,"totalChecks":0,"totalTransfers":1024,
//	 "transferring":[...],"transfers":512}
//
// 同时普通日志行同样是 JSON：{"level":"info","msg":"...","time":"..."}。
//
// 解析策略：
//   - 逐行尝试 json.Unmarshal
//   - 进度行必然包含 totalBytes / bytes 字段，普通日志行没有，以此区分
//   - 非 JSON 行或解析失败直接跳过（容错：日志中可能混杂其它输出）
//   - 返回最后一条有效进度记录

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"
)

// rcloneStats 对应 rclone 统计快照中的核心字段（只需解析前端关心的部分）
type rcloneStats struct {
	Bytes          int64            `json:"bytes"`
	TotalBytes     int64            `json:"totalBytes"`
	Transfers      int64            `json:"transfers"`
	TotalTransfers int64            `json:"totalTransfers"`
	Errors         int64            `json:"errors"`
	ElapsedTime    float64          `json:"elapsedTime"`
	Speed          float64          `json:"speed"`
	Transferring   []rcloneTransfer `json:"transferring"`
	// ETA 为剩余秒数；用 *float64 以兼容整数/浮点/null
	ETA *float64 `json:"eta"`
}

// rcloneTransfer 描述单个正在传输的文件
type rcloneTransfer struct {
	Bytes      int64   `json:"bytes"`
	ETA        *float64 `json:"eta"`
	Group      string  `json:"group"`
	Name       string  `json:"name"`
	Percentage int     `json:"percentage"`
	Size       int64   `json:"size"`
	Speed      float64 `json:"speed"`
	SpeedAvg   float64 `json:"speedAvg"`
}

// Progress 是返回给前端的进度信息
type Progress struct {
	Percent        float64          `json:"percent"`         // 已完成百分比 0~100
	Bytes          int64            `json:"bytes"`           // 已传输字节
	TotalBytes     int64            `json:"totalBytes"`      // 总字节
	Transfers      int64            `json:"transfers"`       // 已完成文件数
	TotalTransfers int64            `json:"totalTransfers"`  // 总文件数
	Errors         int64            `json:"errors"`          // 错误数
	ElapsedTime    float64          `json:"elapsedTime"`     // 已耗时（秒）
	Speed          float64          `json:"speed"`           // 当前速度（字节/秒）
	SpeedHuman     string           `json:"speedHuman"`      // 可读速度文本
	Eta            *float64         `json:"eta"`             // 预计剩余秒数（null 表示未知）
	Transferring   []rcloneTransfer `json:"transferring"`    // 正在传输的文件列表
	Raw            string           `json:"-"`               // 原始 JSON 行（调试用，不返回前端）
}

// parseProgressLines 从日志文本中提取最后一条有效的 rclone 进度记录。
// 返回 (进度, 是否找到)。日志尚未产出任何 stats 行时 found=false。
func parseProgressLines(logText string) (Progress, bool) {
	scanner := bufio.NewScanner(strings.NewReader(logText))
	// 单行可能很长（transferring 数组），放宽扫描器限制
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var last Progress
	found := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if p, ok := parseStatsLine(line); ok {
			last = p
			found = true
		}
	}
	return last, found
}

// parseStatsLine 解析单行 rclone JSON 日志：是 stats 进度行则返回进度。
// 普通日志行（如 {"level":"info","msg":...}）或非 JSON 行直接返回 ok=false。
func parseStatsLine(line string) (Progress, bool) {
	// 第一步：宽松解析，确认该行是 stats 行而非普通日志行
	var probe map[string]interface{}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return Progress{}, false // 非 JSON 行（例如容器启动信息），跳过
	}
	// 进度行必然包含 totalBytes 字段（普通日志行没有），以此区分
	if _, ok := probe["totalBytes"]; !ok {
		return Progress{}, false
	}
	// 第二步：严格解析为统计结构
	var s rcloneStats
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		return Progress{}, false
	}
	p := Progress{
		Bytes:          s.Bytes,
		TotalBytes:     s.TotalBytes,
		Transfers:      s.Transfers,
		TotalTransfers: s.TotalTransfers,
		Errors:         s.Errors,
		ElapsedTime:    s.ElapsedTime,
		Speed:          s.Speed,
		Eta:            s.ETA,
		Transferring:   s.Transferring,
		Raw:            line,
	}
	// 计算百分比：totalBytes 为 0（空桶或尚未开始统计）时百分比为 0
	if s.TotalBytes > 0 {
		p.Percent = float64(s.Bytes) / float64(s.TotalBytes) * 100
	}
	p.SpeedHuman = humanSpeed(s.Speed)
	return p, true
}

// humanSpeed 将字节/秒格式化为可读文本（KiB/s、MiB/s……，风格与 rclone --human-readable 一致）
func humanSpeed(bps float64) string {
	const unit = 1024.0
	if bps < unit {
		return fmt.Sprintf("%.0f B/s", bps)
	}
	div, exp := unit, 0
	for n := bps / div; n >= unit && exp < len("KMGTPE")-1; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB/s", bps/div, "KMGTPE"[exp])
}

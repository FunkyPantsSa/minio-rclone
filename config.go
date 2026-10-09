package main

// config.go —— 应用配置（独立运行模式：web + rclone 单进程组合）
//
// 本版本不再依赖 Kubernetes（不创建 Job/CronJob），由 Web 应用直接 exec rclone
// 并在进程内调度定时备份。所有可配置项均来自环境变量（生产环境可通过 ConfigMap
// 注入、或 docker run -e 传入、或 systemd 环境文件），无需改代码。
//
// 带 json 标签的字段会通过 GET /api/config 展示在控制台「当前配置」面板；
// 敏感字段（登录密码、签名密钥、AK/SK）标记 json:"-" 不会返回前端。

import (
	"os"
	"strconv"
)

// Config 汇总备份控制台的全部可配置项
type Config struct {
	// ---------- rclone 与备份路径 ----------
	RcloneBin        string `json:"rcloneBin"`        // rclone 可执行文件路径（RCLONE_BIN，默认 PATH 中的 rclone）
	RcloneConfigPath string `json:"rcloneConfigPath"` // rclone.conf 路径（RCLONE_CONFIG_PATH，文件不存在时自动跳过 --config）
	RcloneRemote     string `json:"rcloneRemote"`     // 远程名，对应 rclone.conf 的 [remote_s3]（RCLONE_REMOTE）
	RcloneBucket     string `json:"rcloneBucket"`     // 备份桶名（RCLONE_BUCKET）
	SourceSubdir     string `json:"sourceSubdir"`     // 桶内子目录，空表示整个桶（RCLONE_SOURCE_SUBDIR）
	BackupDestDir    string `json:"backupDestDir"`    // 备份目标目录（BACKUP_DEST_DIR，本地路径）
	DataDir          string `json:"dataDir"`          // 运行数据目录（DATA_DIR）：SQLite 数据库与任务日志存放于此

	// ---------- rclone 命令参数 ----------
	BWLimit          string `json:"bwLimit"`          // 带宽上限（RCLONE_BW_LIMIT）
	Transfers        int    `json:"transfers"`        // 并发传输数（RCLONE_TRANSFERS）
	Checkers         int    `json:"checkers"`         // 并发检查数（RCLONE_CHECKERS）
	Retries          int    `json:"retries"`          // 失败重试次数（RCLONE_RETRIES）
	RetriesSleep     string `json:"retriesSleep"`     // 重试间隔（RCLONE_RETRIES_SLEEP）
	StatsInterval    string `json:"statsInterval"`    // 进度输出间隔（RCLONE_STATS_INTERVAL）
	LogLevel         string `json:"logLevel"`         // 日志级别（RCLONE_LOG_LEVEL）
	FastList         bool   `json:"fastList"`         // 启用 --fast-list（RCLONE_FAST_LIST）
	UseServerModtime bool   `json:"useServerModtime"` // 启用 --use-server-modtime（RCLONE_USE_SERVER_MODTIME）
	UseJSONLog       bool   `json:"useJsonLog"`       // 启用 --use-json-log（RCLONE_USE_JSON_LOG）

	// ---------- 定时增量调度（应用内置调度器，等价于原 CronJob） ----------
	CronSchedule               string `json:"cronSchedule"`               // cron 表达式（CRON_SCHEDULE）
	CronSuspend                bool   `json:"cronSuspend"`                // 初始暂停状态（CRON_SUSPEND；页面可随时暂停/恢复，状态持久化到 SQLite）
	IncrementalMaxDuration     string `json:"incrementalMaxDuration"`     // 单次增量 rclone 最大时长（INCREMENTAL_MAX_DURATION）
	SuccessfulHistoryLimit     int32  `json:"successfulHistoryLimit"`     // 成功任务历史保留数（SUCCESSFUL_HISTORY_LIMIT）
	FailedHistoryLimit         int32  `json:"failedHistoryLimit"`         // 失败任务历史保留数（FAILED_HISTORY_LIMIT）
	ManualIncrementalMaxDuration string `json:"manualIncrementalMaxDuration"` // 手动增量最大时长（MANUAL_INCREMENTAL_MAX_DURATION，空则同增量）

	// ---------- 凭证（rclone.conf 使用 env_auth=true 时生效） ----------
	// 以下为敏感字段，仅传给 rclone 子进程，不通过 /api/config 返回
	AccessKeyID     string `json:"-"` // MinIO AK（AWS_ACCESS_KEY_ID）
	SecretAccessKey string `json:"-"` // MinIO SK（AWS_SECRET_ACCESS_KEY）

	// ---------- 登录认证 ----------
	LoginUsername string `json:"loginUsername"` // 控制台登录用户名（LOGIN_USERNAME）
	// 以下为敏感字段，仅用于校验，不通过 /api/config 返回
	LoginPassword string `json:"-"` // 控制台登录密码（LOGIN_PASSWORD，上线前必须修改）
	AuthSecret    string `json:"-"` // 会话签名密钥（AUTH_SECRET，留空则启动时随机生成）
}

// LoadConfig 从环境变量加载配置，未设置时使用默认值。
func LoadConfig() *Config {
	return &Config{
		RcloneBin:        getEnv("RCLONE_BIN", "rclone"),
		RcloneConfigPath: getEnv("RCLONE_CONFIG_PATH", "/config/rclone.conf"),
		RcloneRemote:     getEnv("RCLONE_REMOTE", "remote_s3"),
		RcloneBucket:     getEnv("RCLONE_BUCKET", "YOUR_BUCKET"),
		SourceSubdir:     getEnv("RCLONE_SOURCE_SUBDIR", ""),
		BackupDestDir:    getEnv("BACKUP_DEST_DIR", "/data"),
		DataDir:          getEnv("DATA_DIR", "/data/bc"),

		BWLimit:          getEnv("RCLONE_BW_LIMIT", "5M"),
		Transfers:        getEnvInt("RCLONE_TRANSFERS", 4),
		Checkers:         getEnvInt("RCLONE_CHECKERS", 8),
		Retries:          getEnvInt("RCLONE_RETRIES", 3),
		RetriesSleep:     getEnv("RCLONE_RETRIES_SLEEP", "10s"),
		StatsInterval:    getEnv("RCLONE_STATS_INTERVAL", "5s"),
		LogLevel:         getEnv("RCLONE_LOG_LEVEL", "INFO"),
		FastList:         getEnvBool("RCLONE_FAST_LIST", true),
		UseServerModtime: getEnvBool("RCLONE_USE_SERVER_MODTIME", true),
		UseJSONLog:       getEnvBool("RCLONE_USE_JSON_LOG", true),

		CronSchedule:               getEnv("CRON_SCHEDULE", "*/10 * * * *"),
		CronSuspend:                getEnvBool("CRON_SUSPEND", false),
		IncrementalMaxDuration:     getEnv("INCREMENTAL_MAX_DURATION", "25m"),
		SuccessfulHistoryLimit:     getEnvInt32("SUCCESSFUL_HISTORY_LIMIT", 10),
		FailedHistoryLimit:         getEnvInt32("FAILED_HISTORY_LIMIT", 10),
		ManualIncrementalMaxDuration: getEnv("MANUAL_INCREMENTAL_MAX_DURATION", ""),

		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),

		LoginUsername: getEnv("LOGIN_USERNAME", "admin"),
		LoginPassword: getEnv("LOGIN_PASSWORD", "admin123"),
		AuthSecret:    os.Getenv("AUTH_SECRET"),
	}
}

// Source 拼接 rclone 源路径：remote:bucket[/subdir]
func (c *Config) Source() string {
	s := c.RcloneRemote + ":" + c.RcloneBucket
	if c.SourceSubdir != "" {
		s += "/" + c.SourceSubdir
	}
	return s
}

// CredentialsReady 报告 AK/SK 是否都已配置（配置面板显示用）
func (c *Config) CredentialsReady() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// firstNonEmpty 返回第一个非空字符串
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// getEnv 读取环境变量，为空时返回默认值
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getEnvBool 读取布尔环境变量（true/1/yes 视为 true）
func getEnvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getEnvInt32(key string, def int32) int32 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return def
	}
	return int32(n)
}

func getEnvInt64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

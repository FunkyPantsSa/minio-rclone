package main

// auth.go —— 登录认证
//
// 方案：HMAC 签名的会话 Cookie（无状态，无需 Redis/DB）。
//   - 登录成功：签发 token = base64url(payload) + "." + HMAC-SHA256 签名，
//     payload 为 "过期时间戳.用户名"，以 HttpOnly Cookie 下发；
//   - 校验：恒定时间比较签名、检查过期时间；
//   - 密码与签名密钥来自配置（LOGIN_USERNAME / LOGIN_PASSWORD / AUTH_SECRET）。
//
// 说明：
//   - 密码为固定值（当前按需求写死，上线前务必修改 LOGIN_PASSWORD）；
//   - AUTH_SECRET 未配置时启动随机生成，应用重启后所有会话失效（需重新登录）。

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cookieName 会话 Cookie 名称
const cookieName = "backup_controller_session"

// sessionTTL 会话有效期（12 小时）
const sessionTTL = 12 * time.Hour

// Auth 处理登录认证
type Auth struct {
	username string
	password string
	secret   []byte
	ttl      time.Duration
}

// NewAuth 创建认证器：未配置 AUTH_SECRET 时生成随机密钥
func NewAuth(cfg *Config) *Auth {
	secret := []byte(cfg.AuthSecret)
	if len(secret) == 0 {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic(fmt.Sprintf("生成会话签名密钥失败: %v", err))
		}
		secret = b
	}
	return &Auth{
		username: cfg.LoginUsername,
		password: cfg.LoginPassword,
		secret:   secret,
		ttl:      sessionTTL,
	}
}

// check 校验用户名与密码（恒定时间比较，避免时序攻击）
func (a *Auth) check(user, pass string) bool {
	uOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.username)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(pass), []byte(a.password)) == 1
	return uOK && pOK
}

// issue 签发会话 token：base64url(payload).base64url(hmac)
func (a *Auth) issue(username string) string {
	payload := fmt.Sprintf("%d.%s", time.Now().Add(a.ttl).Unix(), username)
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return p + "." + a.sign(p)
}

// sign 计算 payload 的 HMAC-SHA256 签名（base64url）
func (a *Auth) sign(p string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(p))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify 校验 token，成功返回用户名
func (a *Auth) verify(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	// 签名对象是 base64url 编码后的 payload 字符串（与 issue 保持一致），
	// 恒定时间比较，避免时序攻击
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	// 解析 payload（base64url("过期时间戳.用户名")）
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	s := string(payload)
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return "", false
	}
	expiry, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return "", false
	}
	return s[i+1:], true
}

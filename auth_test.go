package main

// auth_test.go —— 登录认证的单元测试

import (
	"encoding/base64"
	"strings"
	"testing"
)

func newTestAuth() *Auth {
	return NewAuth(&Config{
		LoginUsername: "admin",
		LoginPassword: "secret123",
		AuthSecret:    "fixed-test-secret-for-tests",
	})
}

func TestAuthCheck(t *testing.T) {
	a := newTestAuth()
	if !a.check("admin", "secret123") {
		t.Error("正确的用户名密码应校验通过")
	}
	if a.check("admin", "wrong") {
		t.Error("错误密码不应通过")
	}
	if a.check("nobody", "secret123") {
		t.Error("错误用户名不应通过")
	}
}

// TestAuthIssueVerifyRoundTrip 签发→校验的完整往返
func TestAuthIssueVerifyRoundTrip(t *testing.T) {
	a := newTestAuth()
	token := a.issue("admin")
	user, ok := a.verify(token)
	if !ok {
		t.Fatalf("正常签发的 token 应校验通过: %s", token)
	}
	if user != "admin" {
		t.Errorf("校验应返回用户名 admin，实际 %s", user)
	}
}

func TestAuthVerifyTampered(t *testing.T) {
	a := newTestAuth()

	// 篡改 payload（base64 前缀改为别的字符）
	token := a.issue("admin")
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		t.Fatalf("token 格式异常: %s", token)
	}
	tampered := "A" + parts[0][1:] + "." + parts[1]
	if _, ok := a.verify(tampered); ok {
		t.Error("篡改 payload 的 token 不应通过校验")
	}

	// 伪造签名
	if _, ok := a.verify("abc.def"); ok {
		t.Error("伪造签名的 token 不应通过校验")
	}
	// 空 token
	if _, ok := a.verify(""); ok {
		t.Error("空 token 不应通过校验")
	}
	// 缺少分隔符
	if _, ok := a.verify("no-dot-here"); ok {
		t.Error("缺少 '.' 的 token 不应通过校验")
	}

	// 已过期的 token：用固定密钥构造过去的时间戳
	expiredPayload := base64.RawURLEncoding.EncodeToString([]byte("1600000000.admin"))
	expired := expiredPayload + "." + a.sign(expiredPayload)
	if _, ok := a.verify(expired); ok {
		t.Error("已过期的 token 不应通过校验")
	}
}

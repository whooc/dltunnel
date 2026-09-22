package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// TokenPayload 是主服务器签发给从节点的下载授权凭据.
// 从节点只需持有同一个 secret 就能离线校验, 不需要回连主服务器.
type TokenPayload struct {
	URL string `json:"u"` // 真实下载地址
	Exp int64  `json:"e"` // 过期时间 (unix 秒), 0 表示不过期
	Node string `json:"n"` // 目标节点 ID
	// Priv 由主服务器随令牌下发, 保证主从对"是否允许内网地址"的判断一致.
	// 默认 false: 从节点即使被单独部署也不会成为内网跳板.
	Priv bool `json:"p,omitempty"`
}

func b64e(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func b64d(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("无法读取随机数: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func newID() string { return randHex(6) }

func hmacSum(key string, msg string) []byte {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// signToken 生成 "payload.signature" 形式的下载令牌.
func signToken(secret string, p TokenPayload) (string, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	payload := b64e(body)
	return payload + "." + b64e(hmacSum(secret, payload)), nil
}

// verifyToken 校验令牌签名与有效期.
func verifyToken(secret, token string) (TokenPayload, error) {
	var p TokenPayload
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return p, errors.New("令牌格式错误")
	}
	expect := b64e(hmacSum(secret, parts[0]))
	if !hmac.Equal([]byte(parts[1]), []byte(expect)) {
		return p, errors.New("签名校验失败")
	}
	body, err := b64d(parts[0])
	if err != nil {
		return p, errors.New("令牌内容无法解析")
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, errors.New("令牌内容无法解析")
	}
	if p.Exp > 0 && time.Now().Unix() > p.Exp {
		return p, errors.New("链接已过期")
	}
	if p.URL == "" {
		return p, errors.New("令牌缺少目标地址")
	}
	return p, nil
}

// ---- 管理面板会话 ----

func makeSession(key, user string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	body := b64e([]byte(user + "|" + strconv.FormatInt(exp, 10)))
	return body + "." + b64e(hmacSum(key, body))
}

func checkSession(key, val string) (string, bool) {
	parts := strings.Split(val, ".")
	if len(parts) != 2 {
		return "", false
	}
	expect := b64e(hmacSum(key, parts[0]))
	if !hmac.Equal([]byte(parts[1]), []byte(expect)) {
		return "", false
	}
	raw, err := b64d(parts[0])
	if err != nil {
		return "", false
	}
	segs := strings.Split(string(raw), "|")
	if len(segs) != 2 {
		return "", false
	}
	exp, err := strconv.ParseInt(segs[1], 10, 64)
	if err != nil {
		return "", false
	}
	if time.Now().Unix() >= exp {
		return "", false
	}
	return segs[0], true
}

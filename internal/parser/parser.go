package parser

import (
	"strconv"
	"strings"
	"time"
)

// Params 表示从用户名中解析出的代理参数。
type Params struct {
	Username   string
	Country    string
	SessionID  string
	SessionTTL time.Duration
}

// IsSticky 判断当前请求是否要求 sticky session。
func (p Params) IsSticky() bool {
	return p.SessionID != ""
}

// Parse 解析 DataImpulse 风格的用户名参数。
func Parse(username, password string) Params {
	_ = password

	parts := strings.SplitN(username, "__", 2)
	params := Params{
		Username: parts[0],
	}

	if len(parts) != 2 || parts[1] == "" {
		return params
	}

	for _, item := range strings.Split(parts[1], ";") {
		keyValue := strings.SplitN(item, ".", 2)
		if len(keyValue) != 2 {
			continue
		}

		key := strings.ToLower(strings.TrimSpace(keyValue[0]))
		value := strings.TrimSpace(keyValue[1])
		if value == "" {
			continue
		}

		switch key {
		case "cr":
			params.Country = strings.ToLower(value)
		case "sessid":
			params.SessionID = value
		case "sessttl":
			minutes, err := strconv.Atoi(value)
			if err == nil && minutes > 0 {
				params.SessionTTL = time.Duration(minutes) * time.Minute
			}
		}
	}

	return params
}

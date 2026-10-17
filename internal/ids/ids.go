// Package ids 提供进程内 ID 生成工具，供各层统一使用。
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

// New 生成一个随机 ID；crypto/rand 失败时回退到纳秒时间戳。
func New() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

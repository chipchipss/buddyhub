package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// errStreamClosedWithoutFinish 上游流正常读完但没有 finish_reason（不能当完整回复）。
var errStreamClosedWithoutFinish = errors.New("上游流正常结束，但没有 finish_reason，不能当成完整回复")

// jsonUnmarshalInto json.Unmarshal 薄封装（测试替换点）。
func jsonUnmarshalInto(data string, v any) error { return json.Unmarshal([]byte(data), v) }

// jsonMarshal json.Marshal 薄封装。
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// timeNowUnix 当前 Unix 秒。
func timeNowUnix() int64 { return time.Now().Unix() }

// randRead crypto/rand.Read 别名（compactUUID 用）。
func randRead(b []byte) (int, error) { return rand.Read(b) }

// hexEncodeBytes hex 编码。
func hexEncodeBytes(b []byte) string { return hex.EncodeToString(b) }

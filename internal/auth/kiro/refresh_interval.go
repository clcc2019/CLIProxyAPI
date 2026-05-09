package kiro

import (
	"crypto/rand"
	"math/big"
	"time"
)

const (
	DefaultRefreshIntervalMinSeconds = 60
	DefaultRefreshIntervalMaxSeconds = 60
)

func RandomRefreshIntervalSeconds() int {
	minSeconds := DefaultRefreshIntervalMinSeconds
	maxSeconds := DefaultRefreshIntervalMaxSeconds
	if maxSeconds < minSeconds {
		return minSeconds
	}
	width := int64(maxSeconds - minSeconds + 1)
	n, err := rand.Int(rand.Reader, big.NewInt(width))
	if err != nil {
		return minSeconds + int(time.Now().UnixNano()%width)
	}
	return minSeconds + int(n.Int64())
}

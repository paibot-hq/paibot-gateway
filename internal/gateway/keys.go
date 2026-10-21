package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math"
	"sync"
	"time"
)

const (
	keyPrefix  = "pg_"
	hashPrefix = "sha256:"
)

// GenerateKey returns a new random virtual key with 256 bits of entropy.
// Because keys are random rather than chosen by people, a plain SHA-256
// hash is enough; a slow password hash would add nothing.
func GenerateKey() string {
	b := make([]byte, 32)
	rand.Read(b) // never returns an error; panics if the OS source fails
	return keyPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hashPrefix + hex.EncodeToString(sum[:])
}

// limiter is a token bucket allowing rpm requests per minute with a burst
// of the same size. A nil limiter allows everything.
type limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newLimiter(rpm int) *limiter {
	if rpm <= 0 {
		return nil
	}
	return &limiter{rate: float64(rpm) / 60, burst: float64(rpm), tokens: float64(rpm)}
}

// allow takes one token. When none is left it reports how long until one is.
func (l *limiter) allow(now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() {
		l.tokens = math.Min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	}
	l.last = now
	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}
	wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
	return false, wait
}

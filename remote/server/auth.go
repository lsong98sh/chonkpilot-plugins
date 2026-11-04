package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookieName = "rk_sess"
	sessionTTL        = 7 * 24 * time.Hour
)

// randomHex returns n cryptographically random bytes encoded as hex.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken is the storage form of any bearer token (cookie or device token).
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func bcryptHash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func bcryptCheck(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// loginLimiter counts failed logins per "ip|username" within a sliding window.
type loginLimiter struct {
	mu     sync.Mutex
	fails  map[string][]time.Time
	max    int
	window time.Duration
	lastGC time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{fails: map[string][]time.Time{}, max: max, window: window}
}

func (l *loginLimiter) prune(key string, now time.Time) []time.Time {
	entries := l.fails[key]
	cutoff := now.Add(-l.window)
	kept := entries[:0]
	for _, t := range entries {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = kept
	return kept
}

// blocked reports whether key already exhausted its attempts.
func (l *loginLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.gcLocked(now)
	return len(l.prune(key, now)) >= l.max
}

// recordFailure records one failed attempt.
func (l *loginLimiter) recordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	entries := l.prune(key, now)
	l.fails[key] = append(entries, now)
}

func (l *loginLimiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < l.window {
		return
	}
	l.lastGC = now
	cutoff := now.Add(-l.window)
	for key, entries := range l.fails {
		alive := entries[:0]
		for _, t := range entries {
			if t.After(cutoff) {
				alive = append(alive, t)
			}
		}
		if len(alive) == 0 {
			delete(l.fails, key)
		} else {
			l.fails[key] = alive
		}
	}
}

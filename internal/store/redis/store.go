package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"apod-server/internal/model"
)

type RedisStore struct {
	client     *redis.Client
	enabled    bool
	mu         sync.Mutex
	failCount  int
	openUntil  time.Time
	openWindow time.Duration
	logger    *zap.Logger
	prefix    string
	lastDate  string
	ttl      time.Duration
	failMax  int
}

var (
	redisClientLogMu         sync.Mutex
	redisClientLogLast       time.Time
	redisClientLogSuppressed int
)

type redisZapLogger struct {
	logger *zap.Logger
}

func (r redisZapLogger) Printf(ctx context.Context, format string, v ...interface{}) {
	detail := sanitizeRedisLogDetail(fmt.Sprintf(format, v...))
	if detail == "" {
		return
	}
	emit, suppressed := allowRedisClientLog()
	if !emit {
		return
	}

	l := zap.L().With(zap.String("component", "redis"))
	fields := []zap.Field{zap.String("detail", detail)}
	if suppressed > 0 {
		fields = append(fields, zap.Int("suppressed", suppressed))
	}

	if isRedisConnectivityNoise(detail) {
		l.Warn("redis client log", fields...)
		return
	}
	l.Error("redis client log", fields...)
}

func sanitizeRedisLogDetail(raw string) string {
	detail := strings.TrimSpace(raw)
	if detail == "" {
		return ""
	}
	lower := strings.ToLower(detail)
	if idx := strings.Index(lower, "dial tcp "); idx >= 0 {
		return strings.TrimSpace(detail[idx:])
	}

	prefixes := []string{
		"redis: connection pool: failed to dial after 5 attempts:",
		"redis: connection pool:",
		"redis:",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(strings.ToLower(detail), p) {
			trimmed := strings.TrimSpace(detail[len(p):])
			if trimmed != "" {
				return trimmed
			}
			break
		}
	}
	return detail
}

func isRedisConnectivityNoise(detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "dial tcp") || strings.Contains(lower, "connection refused") || strings.Contains(lower, "timeout") || strings.Contains(lower, "failed to dial")
}

func allowRedisClientLog() (bool, int) {
	const minInterval = 15 * time.Second

	redisClientLogMu.Lock()
	defer redisClientLogMu.Unlock()

	now := time.Now()
	if redisClientLogLast.IsZero() || now.Sub(redisClientLogLast) >= minInterval {
		suppressed := redisClientLogSuppressed
		redisClientLogSuppressed = 0
		redisClientLogLast = now
		return true, suppressed
	}

	redisClientLogSuppressed++
	return false, 0
}

func NewRedisStore(addr, password string, db int, logger *zap.Logger) *RedisStore {
	failWindow := 5 * time.Second
	failMax := 3
	redis.SetLogger(redisZapLogger{logger: logger})
	client := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("redis disabled", zap.Error(err), zap.String("addr", addr))
		return &RedisStore{enabled: false, openWindow: failWindow, logger: logger}
	}
	logger.Info("redis connected", zap.String("addr", addr), zap.Int("db", db))
	return &RedisStore{
		client:     client,
		enabled:   true,
		openWindow: failWindow,
		logger:    logger,
		prefix:   "apod:data:",
		lastDate: "apod:last_date",
		ttl:     30 * 24 * time.Hour,
		failMax:  failMax,
	}
}

func ParseAddr(s string) (addr, password string, db int) {
	addr = s
	if idx := strings.Index(s, "@"); idx >= 0 {
		addr = s[idx+1:]
		if part := s[:idx]; part != "" {
			if at := strings.LastIndex(part, ":"); at >= 0 {
				password = part[at+1:]
				addr = part[:at] + addr
			}
		}
	}
	if idx := strings.LastIndex(addr, "/"); idx >= 0 {
		if n, err := strconv.Atoi(addr[idx+1:]); err == nil {
			db = n
			addr = addr[:idx]
		}
	}
	return
}

func (r *RedisStore) allowAccess() bool {
	if !r.enabled {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !time.Now().Before(r.openUntil)
}

func (r *RedisStore) onSuccess() {
	r.mu.Lock()
	r.failCount = 0
	r.openUntil = time.Time{}
	r.mu.Unlock()
}

func (r *RedisStore) onFailure(op string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failCount++
	if r.failCount < r.failMax {
		r.logger.Warn("redis access failed", zap.String("op", op), zap.Int("fail_count", r.failCount), zap.Error(err))
		return
	}
	r.openUntil = time.Now().Add(r.openWindow)
	r.failCount = 0
	r.logger.Warn("redis circuit open", zap.String("op", op), zap.Duration("open_window", r.openWindow), zap.Error(err))
}

func (r *RedisStore) Get(date string) *model.APOD {
	if !r.allowAccess() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	val, err := r.client.Get(ctx, r.prefix+date).Result()
	if err != nil {
		if err != redis.Nil {
			r.onFailure("get", err)
			return nil
		}
		r.onSuccess()
		return nil
	}
	r.onSuccess()

	var apod model.APOD
	if err := json.Unmarshal([]byte(val), &apod); err != nil {
		r.logger.Warn("redis unmarshal failed", zap.Error(err), zap.String("date", date))
		return nil
	}
	if apod.OriginImage == "" && apod.MediaType == "image" {
		apod.OriginImage = apod.ImageURL
	}
	apod.Cached = true
	return &apod
}

func (r *RedisStore) Set(date string, apod *model.APOD) {
	if !r.allowAccess() || apod == nil {
		return
	}
	body, err := json.Marshal(apod)
	if err != nil {
		r.logger.Warn("redis marshal failed", zap.Error(err), zap.String("date", date))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ttl := r.ttlForDate(date)
	pipe := r.client.TxPipeline()
	pipe.Set(ctx, r.prefix+date, string(body), ttl)
	pipe.Set(ctx, r.lastDate, date, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		r.onFailure("set", err)
		return
	}
	r.onSuccess()
}

func (r *RedisStore) ttlForDate(date string) time.Duration {
	today := time.Now().Format("2006-01-02")
	if date == today {
		return r.ttl
	}
	return 0
}

func (r *RedisStore) GetLast() *model.APOD {
	if !r.allowAccess() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	lastDate, err := r.client.Get(ctx, r.lastDate).Result()
	if err != nil {
		if err != redis.Nil {
			r.onFailure("get_last", err)
			return nil
		}
		r.onSuccess()
		return nil
	}
	r.onSuccess()
	return r.Get(lastDate)
}

func (r *RedisStore) Ready(ctx context.Context) error {
	if !r.enabled {
		return fmt.Errorf("redis disabled")
	}
	if !r.allowAccess() {
		return fmt.Errorf("redis circuit open")
	}
	return r.client.Ping(ctx).Err()
}
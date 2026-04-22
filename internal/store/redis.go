package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"apod-server/internal/httputil"
	"apod-server/internal/model"
)

// RedisStoreConfig holds configuration for NewRedisStore.
type RedisStoreConfig struct {
	Addr        string
	Password    string
	DB          int
	Prefix      string
	LastDateKey string
	TTL         time.Duration
	TodayTTL    time.Duration
	FailWindow  time.Duration
	FailMax     int
	Logger      *zap.Logger
}

// RedisStore is a Redis-backed persistent store implementing KVStore.
type RedisStore struct {
	client      *redis.Client
	enabled     bool
	mu          sync.Mutex
	failCount   int
	openUntil   time.Time
	openWindow  time.Duration
	failMax     int
	prefix      string
	lastDateKey string
	ttl         time.Duration
	todayTTL    time.Duration
	logger      *zap.Logger
}

// --- redis client log suppression ---

var (
	redisClientLogMu         sync.Mutex
	redisClientLogLast       time.Time
	redisClientLogSuppressed int
)

type redisZapLogger struct {
	logger *zap.Logger
}

func (r redisZapLogger) Printf(_ context.Context, format string, v ...interface{}) {
	detail := sanitizeRedisLogDetail(fmt.Sprintf(format, v...))
	if detail == "" {
		return
	}
	emit, suppressed := allowRedisClientLog()
	if !emit {
		return
	}
	l := r.logger.With(zap.String("component", "redis"))
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

// --- constructor ---

// NewRedisStore creates a RedisStore. Returns a disabled store if connection fails.
func NewRedisStore(cfg RedisStoreConfig) *RedisStore {
	if cfg.Prefix == "" {
		cfg.Prefix = "apod:data:"
	}
	if cfg.LastDateKey == "" {
		cfg.LastDateKey = "apod:last_date"
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * 24 * time.Hour
	}
	if cfg.TodayTTL <= 0 {
		cfg.TodayTTL = 48 * time.Hour
	}
	if cfg.FailWindow <= 0 {
		cfg.FailWindow = 5 * time.Second
	}
	if cfg.FailMax <= 0 {
		cfg.FailMax = 3
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}

	redis.SetLogger(redisZapLogger{logger: cfg.Logger})
	client := redis.NewClient(&redis.Options{Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		cfg.Logger.Warn("redis disabled", zap.Error(err), zap.String("addr", cfg.Addr))
		return &RedisStore{enabled: false, openWindow: cfg.FailWindow, logger: cfg.Logger}
	}
	cfg.Logger.Info("redis connected", zap.String("addr", cfg.Addr), zap.Int("db", cfg.DB))
	return &RedisStore{
		client:      client,
		enabled:     true,
		openWindow:  cfg.FailWindow,
		failMax:     cfg.FailMax,
		prefix:      cfg.Prefix,
		lastDateKey: cfg.LastDateKey,
		ttl:         cfg.TTL,
		todayTTL:    cfg.TodayTTL,
		logger:      cfg.Logger,
	}
}

// --- circuit breaker ---

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

// --- KVStore interface ---

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
	pipe.Set(ctx, r.lastDateKey, date, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		r.onFailure("set", err)
		return
	}
	r.onSuccess()
}

func (r *RedisStore) ttlForDate(date string) time.Duration {
	if httputil.IsToday(date) {
		return r.todayTTL
	}
	return r.ttl
}

func (r *RedisStore) GetLast() *model.APOD {
	if !r.allowAccess() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lastDate, err := r.client.Get(ctx, r.lastDateKey).Result()
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

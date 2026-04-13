package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

type RedisStore struct {
	client     *redis.Client
	enabled    bool
	mu         sync.Mutex
	failCount  int
	openUntil  time.Time
	openWindow time.Duration
}

func NewRedisStore() *RedisStore {
	addr := getenv("REDIS_ADDR", "127.0.0.1:6379")
	password := getenv("REDIS_PASSWORD", "")
	db, _ := strconv.Atoi(getenv("REDIS_DB", "0"))
	client := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("redis disabled", zap.Error(err), zap.String("addr", addr))
		return &RedisStore{enabled: false, openWindow: redisFailWindow}
	}
	logger.Info("redis connected", zap.String("addr", addr), zap.Int("db", db))
	return &RedisStore{client: client, enabled: true, openWindow: redisFailWindow}
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
	if r.failCount < redisFailMax {
		logger.Warn("redis access failed", zap.String("op", op), zap.Int("fail_count", r.failCount), zap.Error(err))
		return
	}
	r.openUntil = time.Now().Add(r.openWindow)
	r.failCount = 0
	logger.Warn("redis circuit open", zap.String("op", op), zap.Duration("open_window", r.openWindow), zap.Error(err))
}

func (r *RedisStore) Get(date string) *APOD {
	if !r.allowAccess() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	val, err := r.client.Get(ctx, redisAPODPrefix+date).Result()
	if err != nil {
		if err != redis.Nil {
			r.onFailure("get", err)
			return nil
		}
		r.onSuccess()
		return nil
	}
	r.onSuccess()

	var apod APOD
	if err := json.Unmarshal([]byte(val), &apod); err != nil {
		logger.Warn("redis unmarshal failed", zap.Error(err), zap.String("date", date))
		return nil
	}
	if apod.OriginImage == "" && apod.MediaType == "image" {
		apod.OriginImage = apod.ImageURL
	}
	apod.Cached = true
	return &apod
}

func (r *RedisStore) Set(date string, apod *APOD) {
	if !r.allowAccess() || apod == nil {
		return
	}
	body, err := json.Marshal(apod)
	if err != nil {
		logger.Warn("redis marshal failed", zap.Error(err), zap.String("date", date))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ttl := r.ttlForDate(date)
	pipe := r.client.TxPipeline()
	pipe.Set(ctx, redisAPODPrefix+date, string(body), ttl)
	pipe.Set(ctx, redisLastDate, date, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		r.onFailure("set", err)
		return
	}
	r.onSuccess()
}

func (r *RedisStore) ttlForDate(date string) time.Duration {
	if isToday(date) {
		return redisTTL
	}
	return 0
}

func (r *RedisStore) GetLast() *APOD {
	if !r.allowAccess() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	lastDate, err := r.client.Get(ctx, redisLastDate).Result()
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

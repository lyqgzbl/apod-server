package store

import (
	"context"

	"apod-server/internal/model"
)

// Cache is the interface for in-memory hot cache.
type Cache interface {
	Get(key string) *model.APOD
	Set(key string, val *model.APOD)
	GetLast() *model.APOD
	Cleanup()
}

// KVStore is the interface for persistent key-value storage (e.g. Redis).
type KVStore interface {
	Get(date string) *model.APOD
	Set(date string, val *model.APOD)
	GetLast() *model.APOD
	Ready(ctx context.Context) error
}

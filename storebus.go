package storebus

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/gopatchy/bus"
	"github.com/gopatchy/jsrest"
	"github.com/gopatchy/metadata"
	"github.com/gopatchy/store"
)

type StoreBus struct {
	store *store.Store
	bus   *bus.Bus

	// This lock ensures that no writes interleave with read/subscribe pairs
	orderMu sync.RWMutex

	chanMap   map[<-chan []any]<-chan any
	chanMapMu sync.Mutex
}

func NewStoreBus(dbname string) (*StoreBus, error) {
	st, err := store.NewStore(dbname)
	if err != nil {
		return nil, err
	}

	return &StoreBus{
		store:   st,
		bus:     bus.NewBus(),
		chanMap: map[<-chan []any]<-chan any{},
	}, nil
}

func (sb *StoreBus) Close() {
	sb.store.Close()
}

func (sb *StoreBus) Write(ctx context.Context, t string, obj any) error {
	sb.orderMu.Lock()
	defer sb.orderMu.Unlock()

	if err := updateHash(obj); err != nil {
		return jsrest.Errorf(jsrest.ErrInternalServerError, "hash update failed (%w)", err)
	}

	if err := sb.store.Write(ctx, t, obj); err != nil {
		return jsrest.Errorf(jsrest.ErrInternalServerError, "write failed (%w)", err)
	}

	sb.bus.Announce(t, obj)

	return nil
}

func (sb *StoreBus) Delete(ctx context.Context, t, id string) error {
	sb.orderMu.Lock()
	defer sb.orderMu.Unlock()

	if err := sb.store.Delete(ctx, t, id); err != nil {
		return jsrest.Errorf(jsrest.ErrInternalServerError, "delete failed (%w)", err)
	}

	sb.bus.Delete(t, id)

	return nil
}

func (sb *StoreBus) Read(ctx context.Context, t, id string, factory func() any) (any, error) {
	return sb.store.Read(ctx, t, id, factory)
}

func (sb *StoreBus) ReadStream(ctx context.Context, t, id string, factory func() any) (<-chan any, error) {
	sb.orderMu.RLock()
	defer sb.orderMu.RUnlock()

	initial, err := sb.store.Read(ctx, t, id, factory)
	if err != nil {
		return nil, jsrest.Errorf(jsrest.ErrInternalServerError, "read failed (%w)", err)
	}

	c := sb.bus.SubscribeKey(t, id, initial)

	return c, nil
}

func (sb *StoreBus) CloseReadStream(t, id string, c <-chan any) {
	sb.bus.UnsubscribeKey(t, id, c)
}

func (sb *StoreBus) List(ctx context.Context, t string, factory func() any) ([]any, error) {
	return sb.store.List(ctx, t, factory)
}

func (sb *StoreBus) ListStream(ctx context.Context, t string, factory func() any) (<-chan []any, error) {
	sb.orderMu.RLock()
	defer sb.orderMu.RUnlock()

	initial, err := sb.store.List(ctx, t, factory)
	if err != nil {
		return nil, jsrest.Errorf(jsrest.ErrInternalServerError, "list failed (%w)", err)
	}

	c := sb.bus.SubscribeType(t, initial)

	ret := make(chan []any, 100)

	sb.registerChan(c, ret)

	go func() {
		defer close(ret)

		for range c {
			// List() results are always at least (but not exactly) as new as the write that triggered it
			l, err := sb.store.List(ctx, t, factory)
			if err != nil {
				break
			}

			select {
			case ret <- l:
			default:
				break
			}
		}
	}()

	return ret, nil
}

func (sb *StoreBus) CloseListStream(t string, c <-chan []any) {
	sb.chanMapMu.Lock()
	defer sb.chanMapMu.Unlock()

	sb.bus.UnsubscribeType(t, sb.chanMap[c])

	delete(sb.chanMap, c)
}

func (sb *StoreBus) registerChan(in <-chan any, out <-chan []any) {
	sb.chanMapMu.Lock()
	defer sb.chanMapMu.Unlock()

	sb.chanMap[out] = in
}

func updateHash(obj any) error {
	m := *metadata.GetMetadata(obj)
	metadata.ClearMetadata(obj)

	defer metadata.SetMetadata(obj, &m)

	hash := sha256.New()
	enc := json.NewEncoder(hash)

	if err := enc.Encode(obj); err != nil {
		return jsrest.Errorf(jsrest.ErrInternalServerError, "JSON encode failed (%w)", err)
	}

	m.ETag = fmt.Sprintf("etag:%x", hash.Sum(nil))

	return nil
}

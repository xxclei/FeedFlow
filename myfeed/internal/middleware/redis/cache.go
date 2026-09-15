package redis

import (
	"context"
	"errors"
	"time"
)

func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	if c == nil || c.rdb == nil {
		return nil, errors.New("redis client not initialized")
	}
	return c.rdb.Get(ctx, key).Bytes()
}

func (c *Client) SetBytes(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return errors.New("redis client not initialized")
	}
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

func (c *Client) Del(ctx context.Context, key string) error {
	if c == nil || c.rdb == nil {
		return errors.New("redis client not initialized")
	}
	return c.rdb.Del(ctx, key).Err()
}

func (c *Client) DelByPattern(ctx context.Context, pattern string) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	iter := c.rdb.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		_ = c.rdb.Del(ctx, iter.Val())
	}
	return iter.Err()
}

// DelMany 一次删多个 key。
//
// 用 pipeline 把 N 条 DEL 压成**一次往返**，而不是循环调 N 次 Del。
// 批量删除场景 N 可能是几十上百，逐条 Del 就是 N 次 RTT ——
// 在 50ms 的超时预算内，N=100 时几乎必然全部超时失败。
//
// 注意 pipeline **不是事务**：不保证原子性，中间某条失败其余照常执行。
// 失效缓存这个场景要的正是这个语义（尽力而为，TTL 兜底），
// 用 MULTI/EXEC 反而多两次往返、且没必要。
func (c *Client) DelMany(ctx context.Context, keys []string) error {
	if c == nil || c.rdb == nil || len(keys) == 0 {
		return nil
	}
	pipe := c.rdb.Pipeline()
	for _, k := range keys {
		pipe.Del(ctx, k)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (c *Client) MGet(ctx context.Context, cacheKeys ...string) ([]interface{}, error) {
	if c == nil || c.rdb == nil {
		return nil, errors.New("redis client not initialized")
	}
	return c.rdb.MGet(ctx, cacheKeys...).Result()
}

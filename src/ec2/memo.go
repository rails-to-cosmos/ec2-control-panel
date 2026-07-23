package ec2

import "sync"

// Memo returns the cached value for KEY, computing and storing it on a miss.
// Errors are not cached, so a transient AWS failure retries on the next call.
// Centralizing this keeps the type assertion in one place instead of one per
// cache site.
func Memo[V any](c *sync.Map, key any, compute func() (V, error)) (V, error) {
	if v, ok := c.Load(key); ok {
		if typed, ok := v.(V); ok {
			return typed, nil
		}
	}
	v, err := compute()
	if err != nil {
		var zero V
		return zero, err
	}
	c.Store(key, v)
	return v, nil
}

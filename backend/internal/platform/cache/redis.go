package cache

import redis "github.com/redis/go-redis/v9"

// Cache operations must honor their context deadline, including pool waits,
// socket I/O and connection setup. Broker/coordination clients have other budgets.
func NewRedisClient(addr, password string, db int) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: addr, Password: password, DB: db,
		ContextTimeoutEnabled: true, MaxRetries: -1,
		DialTimeout: redisTimeout, ReadTimeout: redisTimeout,
		WriteTimeout: redisTimeout, PoolTimeout: redisTimeout,
	})
}

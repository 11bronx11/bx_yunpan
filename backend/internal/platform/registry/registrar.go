// Package registry 提供基于 etcd 的服务注册与 gRPC 服务发现。
//
// 注册侧用 lease + KeepAlive：实例进程消失后租约到期自动摘除，不需要
// 额外的健康检查回路。优雅停机时主动 Revoke，让摘除立即生效而不必等
// 一个 TTL。
//
// 发现侧实现 gRPC 原生的 resolver.Builder 接口而不是在业务层轮询地址：
// 这样地址变化由 gRPC 自己交给 balancer（round_robin）处理，连接的建立
// 与摘除、请求的重新分配都走框架既有逻辑。
package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/config"
)

// Open 按配置连接 etcd。未启用时返回 nil client，调用方需判空。
func Open(cfg config.Registry) (*clientv3.Client, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.Endpoints,
		Username:    cfg.Username,
		Password:    cfg.Password,
		DialTimeout: cfg.DialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("connect etcd: %w", err)
	}
	return client, nil
}

// Registrar 把本实例注册到 etcd 并维持租约。
type Registrar struct {
	client *clientv3.Client
	logger *slog.Logger
	key    string
	value  string
	ttl    time.Duration
	lease  clientv3.LeaseID
}

// NewRegistrar 构造注册器。key 为 {prefix}/{instanceID}，value 为
// 对端可 dial 的地址。
func NewRegistrar(client *clientv3.Client, logger *slog.Logger, key, value string, ttl time.Duration) *Registrar {
	return &Registrar{client: client, logger: logger, key: key, value: value, ttl: ttl}
}

// Register 申请租约并写入实例键，随后在后台续租。
//
// KeepAlive 的 channel 被 etcd 关闭意味着租约已失效（网络长时间中断或
// etcd 重启）。此时不能沉默：实例还活着但注册中心里已经没有它，必须
// 重新注册，否则流量永远不会再回来。
func (r *Registrar) Register(ctx context.Context) error {
	grant, err := r.client.Grant(ctx, int64(r.ttl.Seconds()))
	if err != nil {
		return fmt.Errorf("grant etcd lease: %w", err)
	}
	if _, err := r.client.Put(ctx, r.key, r.value, clientv3.WithLease(grant.ID)); err != nil {
		return fmt.Errorf("register instance: %w", err)
	}
	r.lease = grant.ID

	keepAlive, err := r.client.KeepAlive(ctx, grant.ID)
	if err != nil {
		return fmt.Errorf("keep etcd lease alive: %w", err)
	}
	go r.keepAlive(ctx, keepAlive)
	r.logger.InfoContext(ctx, "registered to etcd", "key", r.key, "address", r.value, "lease_ttl", r.ttl)
	return nil
}

func (r *Registrar) keepAlive(ctx context.Context, keepAlive <-chan *clientv3.LeaseKeepAliveResponse) {
	for {
		select {
		case <-ctx.Done():
			return
		case response, ok := <-keepAlive:
			if ok && response != nil {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			r.logger.WarnContext(ctx, "etcd lease lost, re-registering", "key", r.key)
			if err := r.reregister(ctx); err != nil {
				r.logger.ErrorContext(ctx, "re-register to etcd failed", "key", r.key, "error", err)
				return
			}
			return
		}
	}
}

// reregister 带退避重试地重新注册，直到成功或 ctx 结束。
func (r *Registrar) reregister(ctx context.Context) error {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if err := r.Register(ctx); err == nil {
			return nil
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// Deregister 主动撤销租约，让实例立即从注册中心消失而不必等 TTL 到期。
// 停机流程里应在停止接收新请求之后调用。
func (r *Registrar) Deregister(ctx context.Context) error {
	if r.lease == 0 {
		return nil
	}
	lease := r.lease
	r.lease = 0
	if _, err := r.client.Revoke(ctx, lease); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("revoke etcd lease: %w", err)
	}
	r.logger.InfoContext(ctx, "deregistered from etcd", "key", r.key)
	return nil
}

package registry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/resolver"
)

// Scheme 是本 resolver 注册到 gRPC 的 scheme。dial 目标形如
// yunpan-etcd:///services/aisvc。
const Scheme = "yunpan-etcd"

// retryInterval 是 etcd 不可用后重新对齐快照的间隔。
const retryInterval = 3 * time.Second

var errWatchClosed = errors.New("registry: etcd watch channel closed")

// timeAfter 便于测试替换计时。
var timeAfter = time.After

// Metrics 暴露服务发现的健康度。注册中心故障是一种「业务还活着但看不见
// 彼此」的状态，必须有指标能报警，否则只能靠日志事后发现。
type Metrics struct {
	Instances prometheus.Gauge
	WatchErrs prometheus.Counter
}

// NewMetrics 注册服务发现指标。
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	metrics := &Metrics{
		Instances: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "yunpan_registry_instances",
			Help: "Number of aisvc instances currently known to the resolver.",
		}),
		WatchErrs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "yunpan_registry_watch_errors_total",
			Help: "Total number of etcd watch failures observed by the resolver.",
		}),
	}
	registerer.MustRegister(metrics.Instances, metrics.WatchErrs)
	return metrics
}

// Builder 是 gRPC resolver.Builder 的 etcd 实现。
type Builder struct {
	client  *clientv3.Client
	logger  *slog.Logger
	metrics *Metrics
}

// NewBuilder 构造 resolver builder。用 resolver.Register 注册后，
// grpc.NewClient("yunpan-etcd:///services/aisvc") 即可走它解析。
func NewBuilder(client *clientv3.Client, logger *slog.Logger, metrics *Metrics) *Builder {
	return &Builder{client: client, logger: logger, metrics: metrics}
}

func (b *Builder) Scheme() string { return Scheme }

// Build 先做一次全量拉取拿到初始地址，再起 watch 增量更新。
//
// 初始全量失败时不返回错误：注册中心故障不应该让 API 起不来。此时地址
// 列表为空，gRPC 会保持在 TRANSIENT_FAILURE 并持续重连，watch 一旦恢复
// 就自动补上地址。
func (b *Builder) Build(target resolver.Target, clientConn resolver.ClientConn, _ resolver.BuildOptions) (resolver.Resolver, error) {
	prefix := targetPrefix(target)
	ctx, cancel := context.WithCancel(context.Background())
	instance := &etcdResolver{
		source:     etcdSource{client: b.client},
		logger:     b.logger,
		metrics:    b.metrics,
		clientConn: clientConn,
		prefix:     prefix,
		addresses:  map[string]string{},
		cancel:     cancel,
	}
	instance.wait.Add(1)
	go instance.watch(ctx)
	return instance, nil
}

func targetPrefix(target resolver.Target) string {
	// Endpoint() strips the leading slash from targets such as
	// yunpan-etcd:///services/aisvc. etcd keys are absolute, so prefer the raw
	// URL path or the resolver would watch services/aisvc and never see
	// /services/aisvc/* registrations.
	prefix := target.URL.Path
	if prefix == "" {
		prefix = target.Endpoint()
	}
	return prefix
}

// keyValueSource 抽出 resolver 真正用到的 etcd 能力，便于单测注入故障。
type keyValueSource interface {
	// list 返回当前全量地址与对应的 revision。
	list(ctx context.Context, prefix string) (map[string]string, int64, error)
	// watch 从 revision 起推送增量，channel 关闭表示连接断开。
	watch(ctx context.Context, prefix string, revision int64) <-chan watchResponse
}

// watchResponse 是一批增量事件，err 非 nil 表示 watch 出错需要重新对齐。
type watchResponse struct {
	events []watchEvent
	err    error
}

type etcdResolver struct {
	source     keyValueSource
	logger     *slog.Logger
	metrics    *Metrics
	clientConn resolver.ClientConn
	prefix     string

	mutex     sync.Mutex
	addresses map[string]string
	cancel    context.CancelFunc
	wait      sync.WaitGroup
}

// ResolveNow 是 gRPC 的主动刷新钩子。这里是空实现：watch 已经在推增量，
// 没有需要额外触发的拉取。
func (r *etcdResolver) ResolveNow(resolver.ResolveNowOptions) {}

func (r *etcdResolver) Close() {
	r.cancel()
	r.wait.Wait()
}

// watch 先全量后增量。etcd 的 revision 保证了两者之间不会漏事件：
// 从全量返回的 revision+1 开始 watch，正是那次快照之后的第一个变更。
func (r *etcdResolver) watch(ctx context.Context) {
	defer r.wait.Done()
	for {
		revision, err := r.loadAll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// 保留上一次已知地址列表继续服务：注册中心故障不能直接打挂业务。
			r.reportFailure(ctx, "etcd list failed, keeping last known addresses", err)
			if !r.pause(ctx) {
				return
			}
			continue
		}
		if err := r.consume(ctx, revision+1); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.reportFailure(ctx, "etcd watch interrupted, keeping last known addresses", err)
			if !r.pause(ctx) {
				return
			}
		}
	}
}

// reportFailure 打指标与告警日志。地址列表刻意不动：这正是"注册中心故障时
// 用最后一次已知地址继续服务"的实现点。
func (r *etcdResolver) reportFailure(ctx context.Context, message string, err error) {
	r.metrics.WatchErrs.Inc()
	if r.logger != nil {
		r.logger.WarnContext(ctx, message, "prefix", r.prefix, "known", r.count(), "error", err)
	}
}

// pause 返回 false 表示 ctx 已结束，调用方应退出。
func (r *etcdResolver) pause(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-timeAfter(retryInterval):
		return true
	}
}

func (r *etcdResolver) loadAll(ctx context.Context) (int64, error) {
	addresses, revision, err := r.source.list(ctx, r.prefix)
	if err != nil {
		return 0, err
	}
	r.replace(addresses)
	return revision, nil
}

// consume 消费增量事件。watch channel 被关闭或带 error 时返回，由 watch
// 重新走一遍全量+增量——不能只重连 watch，因为中断期间的事件已经丢了，
// 必须重新对齐快照，否则地址列表会停在过期状态。
func (r *etcdResolver) consume(ctx context.Context, revision int64) error {
	for response := range r.source.watch(ctx, r.prefix, revision) {
		if response.err != nil {
			return response.err
		}
		r.applyEvents(response.events)
	}
	return errWatchClosed
}

// etcdSource 是 keyValueSource 的 etcd 实现。
type etcdSource struct{ client *clientv3.Client }

func (s etcdSource) list(ctx context.Context, prefix string) (map[string]string, int64, error) {
	response, err := s.client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, 0, err
	}
	addresses := make(map[string]string, len(response.Kvs))
	for _, kv := range response.Kvs {
		addresses[string(kv.Key)] = string(kv.Value)
	}
	return addresses, response.Header.Revision, nil
}

func (s etcdSource) watch(ctx context.Context, prefix string, revision int64) <-chan watchResponse {
	out := make(chan watchResponse)
	go func() {
		defer close(out)
		for response := range s.client.Watch(ctx, prefix, clientv3.WithPrefix(), clientv3.WithRev(revision)) {
			converted := watchResponse{err: response.Err()}
			if converted.err == nil {
				converted.events = make([]watchEvent, 0, len(response.Events))
				for _, event := range response.Events {
					converted.events = append(converted.events, watchEvent{
						key:     string(event.Kv.Key),
						value:   string(event.Kv.Value),
						deleted: event.Type == clientv3.EventTypeDelete,
					})
				}
			}
			select {
			case out <- converted:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// watchEvent 是与 etcd 类型解耦的地址变更事件，便于单测直接驱动。
type watchEvent struct {
	key     string
	value   string
	deleted bool
}

func (r *etcdResolver) applyEvents(events []watchEvent) {
	r.mutex.Lock()
	for _, event := range events {
		if event.deleted {
			delete(r.addresses, event.key)
			continue
		}
		r.addresses[event.key] = event.value
	}
	state := r.stateLocked()
	r.mutex.Unlock()
	r.push(state)
}

func (r *etcdResolver) replace(addresses map[string]string) {
	r.mutex.Lock()
	r.addresses = addresses
	state := r.stateLocked()
	r.mutex.Unlock()
	r.push(state)
}

func (r *etcdResolver) stateLocked() resolver.State {
	endpoints := make([]resolver.Endpoint, 0, len(r.addresses))
	for _, address := range r.addresses {
		endpoints = append(endpoints, resolver.Endpoint{Addresses: []resolver.Address{{Addr: address}}})
	}
	return resolver.State{Endpoints: endpoints}
}

func (r *etcdResolver) push(state resolver.State) {
	r.metrics.Instances.Set(float64(len(state.Endpoints)))
	if err := r.clientConn.UpdateState(state); err != nil && r.logger != nil {
		r.logger.Warn("update resolver state", "prefix", r.prefix, "error", err)
	}
}

func (r *etcdResolver) count() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return len(r.addresses)
}

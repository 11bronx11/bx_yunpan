package registry

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

// stateRecorder 记录 resolver 推给 gRPC 的每一次地址列表。
type stateRecorder struct {
	mutex  sync.Mutex
	states []resolver.State
}

func (r *stateRecorder) UpdateState(state resolver.State) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.states = append(r.states, state)
	return nil
}

func (r *stateRecorder) ReportError(error)             {}
func (r *stateRecorder) NewAddress([]resolver.Address) {}

func (r *stateRecorder) ParseServiceConfig(string) *serviceconfig.ParseResult {
	return nil
}

func (r *stateRecorder) last() resolver.State {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if len(r.states) == 0 {
		return resolver.State{}
	}
	return r.states[len(r.states)-1]
}

func addressesOf(state resolver.State) []string {
	result := make([]string, 0, len(state.Endpoints))
	for _, endpoint := range state.Endpoints {
		for _, address := range endpoint.Addresses {
			result = append(result, address.Addr)
		}
	}
	return result
}

func newTestResolver(t *testing.T, recorder *stateRecorder) *etcdResolver {
	t.Helper()
	return &etcdResolver{
		logger:     nil,
		metrics:    NewMetrics(prometheus.NewRegistry()),
		clientConn: recorder,
		prefix:     "/services/aisvc",
		addresses:  map[string]string{},
		cancel:     func() {},
	}
}

func TestApplyPutAndDeleteUpdatesAddresses(t *testing.T) {
	recorder := &stateRecorder{}
	instance := newTestResolver(t, recorder)

	instance.replace(map[string]string{
		"/services/aisvc/a": "host-a:8082",
		"/services/aisvc/b": "host-b:8082",
	})
	if got := len(addressesOf(recorder.last())); got != 2 {
		t.Fatalf("addresses after full load = %d, want 2", got)
	}

	instance.applyEvents([]watchEvent{{key: "/services/aisvc/b", deleted: true}})
	addresses := addressesOf(recorder.last())
	if len(addresses) != 1 || addresses[0] != "host-a:8082" {
		t.Fatalf("addresses after delete = %v, want [host-a:8082]", addresses)
	}

	instance.applyEvents([]watchEvent{{key: "/services/aisvc/c", value: "host-c:8082"}})
	if got := len(addressesOf(recorder.last())); got != 2 {
		t.Fatalf("addresses after put = %d, want 2", got)
	}
}

// 注册中心故障时必须保留最后一次已知地址列表：etcd 挂了不能顺带打挂业务。
// 这里让 source 先成功返回一次地址，随后一直失败，验证 resolver 既不清空
// 地址、也不推空列表给 gRPC。
func TestKeepsLastKnownAddressesWhenEtcdFails(t *testing.T) {
	recorder := &stateRecorder{}
	registerer := prometheus.NewRegistry()
	source := &flakySource{addresses: map[string]string{"/services/aisvc/a": "host-a:8082"}}

	// 缩短重试间隔，避免测试等 3s。
	original := timeAfter
	timeAfter = func(time.Duration) <-chan time.Time {
		channel := make(chan time.Time, 1)
		channel <- time.Now()
		return channel
	}
	t.Cleanup(func() { timeAfter = original })

	ctx, cancel := context.WithCancel(context.Background())
	instance := &etcdResolver{
		source:     source,
		metrics:    NewMetrics(registerer),
		clientConn: recorder,
		prefix:     "/services/aisvc",
		addresses:  map[string]string{},
		cancel:     cancel,
	}
	instance.wait.Add(1)
	go instance.watch(ctx)

	// 等到至少发生过几次失败重试，确认 resolver 没有在失败路径上清空地址。
	deadline := time.Now().Add(3 * time.Second)
	for source.failures() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	instance.Close()

	if source.failures() < 3 {
		t.Fatalf("list failures = %d, want at least 3", source.failures())
	}
	addresses := addressesOf(recorder.last())
	if len(addresses) != 1 || addresses[0] != "host-a:8082" {
		t.Fatalf("addresses after failures = %v, want [host-a:8082]", addresses)
	}
	if instance.count() != 1 {
		t.Fatalf("known addresses = %d, want 1", instance.count())
	}
	if got := counterValue(t, registerer, "yunpan_registry_watch_errors_total"); got < 3 {
		t.Fatalf("watch errors counter = %v, want at least 3", got)
	}
}

// flakySource 第一次 list 成功，之后一直失败，模拟 etcd 掉线。
type flakySource struct {
	addresses map[string]string

	mutex sync.Mutex
	calls int
	fails int
}

func (s *flakySource) list(context.Context, string) (map[string]string, int64, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.calls++
	if s.calls == 1 {
		return s.addresses, 7, nil
	}
	s.fails++
	return nil, 0, errors.New("etcd unavailable")
}

func (s *flakySource) watch(ctx context.Context, _ string, _ int64) <-chan watchResponse {
	channel := make(chan watchResponse, 1)
	channel <- watchResponse{err: errors.New("watch broken")}
	close(channel)
	return channel
}

func (s *flakySource) failures() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.fails
}

func TestBuildDerivesPrefixFromTarget(t *testing.T) {
	builder := NewBuilder(nil, nil, NewMetrics(prometheus.NewRegistry()))
	if builder.Scheme() != Scheme {
		t.Fatalf("scheme = %q, want %q", builder.Scheme(), Scheme)
	}
	targetURL, err := url.Parse("yunpan-etcd:///services/aisvc")
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	if got := targetPrefix(resolver.Target{URL: *targetURL}); got != "/services/aisvc" {
		t.Fatalf("prefix = %q, want /services/aisvc", got)
	}
}

func TestInstanceGaugeTracksAddressCount(t *testing.T) {
	registerer := prometheus.NewRegistry()
	recorder := &stateRecorder{}
	instance := &etcdResolver{
		metrics:    NewMetrics(registerer),
		clientConn: recorder,
		prefix:     "/services/aisvc",
		addresses:  map[string]string{},
		cancel:     func() {},
	}
	instance.replace(map[string]string{
		"/services/aisvc/a": "host-a:8082",
		"/services/aisvc/b": "host-b:8082",
		"/services/aisvc/c": "host-c:8082",
	})
	if got := gaugeValue(t, registerer, "yunpan_registry_instances"); got != 3 {
		t.Fatalf("instances gauge = %v, want 3", got)
	}
	instance.replace(map[string]string{"/services/aisvc/a": "host-a:8082"})
	if got := gaugeValue(t, registerer, "yunpan_registry_instances"); got != 1 {
		t.Fatalf("instances gauge = %v, want 1", got)
	}
}

func gaugeValue(t *testing.T, registerer *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registerer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			return metric.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func counterValue(t *testing.T, registerer *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registerer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			return metric.GetCounter().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsMiddlewareRecordsSuccessAndFailure(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry, RoleObject, map[string]int{QueueObject: 1}, nil)
	success := metrics.Middleware()(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil }))
	if err := success.ProcessTask(context.Background(), asynq.NewTask("object.verify.v1", nil)); err != nil {
		t.Fatal(err)
	}
	failure := metrics.Middleware()(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return errors.New("injected") }))
	if err := failure.ProcessTask(context.Background(), asynq.NewTask("object.verify.v1", nil)); err == nil {
		t.Fatal("failed task unexpectedly succeeded")
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var successCount, failureCount float64
	for _, family := range families {
		if family.GetName() != "yunpan_worker_tasks_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			result := ""
			for _, label := range metric.GetLabel() {
				if label.GetName() == "result" {
					result = label.GetValue()
				}
			}
			switch result {
			case "success":
				successCount = metric.GetCounter().GetValue()
			case "error":
				failureCount = metric.GetCounter().GetValue()
			}
		}
	}
	if successCount != 1 || failureCount != 1 {
		t.Fatalf("task metrics success=%v failure=%v", successCount, failureCount)
	}
}

func TestMetricsRegistersQueueStateWithoutBroker(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry, RoleMedia, map[string]int{QueueMedia: 4}, nil)
	metrics.queueSize.WithLabelValues(string(RoleMedia), QueueMedia).Set(0)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, family := range families {
		if family.GetName() == "yunpan_worker_queue_size" {
			found = true
		}
	}
	if !found {
		t.Fatal("worker queue metrics were not registered")
	}
}

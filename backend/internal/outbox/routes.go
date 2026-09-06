package outbox

import (
	"strings"

	"github.com/11bronx11/bx_yunpan/backend/internal/eventcontract"
	"github.com/11bronx11/bx_yunpan/backend/internal/eventstream"
)

// KafkaRoutes describes long-lived domain-event deliveries. Commands stay on
// Asynq; Kafka receives only replayable facts with a stable aggregate key.
func KafkaRoutes(eventType string, enabled bool) []DeliveryTarget {
	if !enabled || strings.TrimSpace(eventType) == "" {
		return nil
	}
	// Commands are intentionally excluded from the durable event stream.
	definition, ok := eventcontract.Lookup(eventType)
	if !ok || definition.MessageKind != eventcontract.MessageKindDomainEvent {
		return nil
	}
	topic := eventstream.TopicForEvent(eventType)
	if topic == "" {
		return nil
	}
	return []DeliveryTarget{{Transport: "kafka", Destination: topic}}
}

const (
	TaskObjectVerifyV1   = "object.verify.v1"
	TaskMediaThumbnailV1 = "media.thumbnail.v1"
	TaskObjectGCV1       = "object.gc.v1"
	TaskAIIndexV1        = "ai.index.v1"
	TaskAIReprocessV1    = "ai.reprocess.v1"
)

// DeliveryTarget is the transport-owned routing decision for one outbox row.
// Callers can choose an event type, but cannot choose an arbitrary queue.
type DeliveryTarget struct {
	Transport   string
	Destination string
	TaskType    string
	Queue       string
}

func target(transport, taskType, queue string) DeliveryTarget {
	return DeliveryTarget{
		Transport:   transport,
		TaskType:    taskType,
		Queue:       queue,
		Destination: taskType + "@" + queue,
	}
}

func SearchRoutes(eventType string, enabled bool) []DeliveryTarget {
	if !enabled {
		return nil
	}
	switch eventType {
	case EventFileCreated, eventcontract.EventFileUpdatedV1, eventcontract.EventFileDeletedV1,
		eventcontract.EventFolderCreatedV1, eventcontract.EventFolderRenamedV1,
		eventcontract.EventFolderMovedV1, eventcontract.EventFolderDeletedV1,
		eventcontract.EventShareCreatedV1, eventcontract.EventShareRevokedV1,
		EventShareImported, EventObjectReady, EventObjectGCRequested,
		eventcontract.EventAIIndexRequestedV1:
		return []DeliveryTarget{target("asynq", TaskSearchProjectV1, "search")}
	default:
		return nil
	}
}

// Routes is the explicit registry for enabled Asynq destinations. Unknown
// event types return nil and are kept as observable unrouted outbox rows.
func Routes(eventType string, aiEnabled bool) []DeliveryTarget {
	switch eventType {
	case EventFileCreated:
		return []DeliveryTarget{target("asynq", EventFileCreated, "maintenance")}
	case EventShareImported:
		return []DeliveryTarget{target("asynq", EventShareImported, "maintenance")}
	case EventObjectVerifyRequested:
		return []DeliveryTarget{target("asynq", TaskObjectVerifyV1, "object")}
	case EventObjectReady:
		targets := []DeliveryTarget{target("asynq", TaskMediaThumbnailV1, "media")}
		if aiEnabled {
			targets = append(targets, target("asynq", TaskAIIndexV1, "ai"))
		}
		return targets
	case EventObjectGCRequested:
		return []DeliveryTarget{target("asynq", TaskObjectGCV1, "maintenance")}
	case EventAIReprocessRequested:
		if aiEnabled {
			return []DeliveryTarget{target("asynq", TaskAIReprocessV1, "ai")}
		}
		return nil
	case EventAIIndexRequested:
		if aiEnabled {
			return []DeliveryTarget{target("asynq", TaskAIIndexV1, "ai")}
		}
		return nil
	case EventStorageOperationRecover:
		return []DeliveryTarget{target("asynq", EventStorageOperationRecover, "maintenance")}
	case EventMediaThumbnailRequested:
		return []DeliveryTarget{target("asynq", EventMediaThumbnailRequested, "media")}
	default:
		return nil
	}
}

func targetFromDelivery(delivery Delivery) (DeliveryTarget, bool) {
	if delivery.Transport != "asynq" {
		return DeliveryTarget{}, false
	}
	separator := strings.LastIndexByte(delivery.Destination, '@')
	if separator <= 0 || separator == len(delivery.Destination)-1 {
		return DeliveryTarget{}, false
	}
	taskType, queue := canonicalTaskType(delivery.Destination[:separator], delivery.Destination[separator+1:])
	return target(delivery.Transport, taskType, queue), true
}

// canonicalTaskType keeps deliveries created by the pre-P2 relay executable
// during the compatibility window while dispatching them to the versioned
// handler names.
func canonicalTaskType(taskType, queue string) (string, string) {
	switch {
	case taskType == EventObjectVerifyRequested && queue == "object":
		return TaskObjectVerifyV1, queue
	case taskType == EventObjectReady && queue == "media":
		return TaskMediaThumbnailV1, queue
	case taskType == EventObjectGCRequested && queue == "maintenance":
		return TaskObjectGCV1, queue
	case taskType == EventAIIndexRequested && queue == "ai":
		return TaskAIIndexV1, queue
	case taskType == EventAIReprocessRequested && queue == "ai":
		return TaskAIReprocessV1, queue
	default:
		return taskType, queue
	}
}

func targetAllowedForEvent(eventType string, aiEnabled bool, delivery Delivery) (DeliveryTarget, bool) {
	return targetAllowedForEventWithSearch(eventType, aiEnabled, false, delivery)
}

func targetAllowedForEventWithSearch(eventType string, aiEnabled, searchEnabled bool, delivery Delivery) (DeliveryTarget, bool) {
	target, ok := targetFromDelivery(delivery)
	if !ok {
		return DeliveryTarget{}, false
	}
	for _, allowed := range Routes(eventType, aiEnabled) {
		if allowed.Transport == target.Transport && allowed.Destination == target.Destination {
			return target, true
		}
	}
	for _, allowed := range SearchRoutes(eventType, searchEnabled) {
		if allowed.Transport == target.Transport && allowed.Destination == target.Destination {
			return target, true
		}
	}
	return DeliveryTarget{}, false
}

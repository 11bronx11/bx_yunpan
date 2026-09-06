package eventstream

import "strings"

// TopicForEvent keeps Kafka topic ownership at the event-stream boundary. A
// stable aggregate key is supplied separately by the caller to preserve order
// within one aggregate without assuming global ordering.
func TopicForEvent(eventType string) string {
	if strings.HasPrefix(eventType, "file.") || strings.HasPrefix(eventType, "folder.") {
		return "yunpan.file-events"
	}
	if strings.HasPrefix(eventType, "share.") {
		return "yunpan.share-events"
	}
	if strings.HasPrefix(eventType, "object.") {
		return "yunpan.object-events"
	}
	if strings.HasPrefix(eventType, "ai.") {
		return "yunpan.ai-events"
	}
	return ""
}

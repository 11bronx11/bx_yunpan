package taskqueue

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// DecodeLegacyTask is used only by the offline cutover utility. It decodes the
// stable Asynq TaskMessage wire fields without retaining the Asynq runtime.
func DecodeLegacyTask(data []byte) (Task, error) {
	var task Task
	for len(data) > 0 {
		field, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return task, protowire.ParseError(n)
		}
		data = data[n:]
		switch typ {
		case protowire.BytesType:
			value, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return task, protowire.ParseError(n)
			}
			data = data[n:]
			switch field {
			case 1:
				task.Kind = string(value)
			case 2:
				task.Body = append([]byte(nil), value...)
			case 3:
				task.ID = string(value)
			case 4:
				task.Queue = string(value)
			case 10, 14:
				if len(value) > 0 {
					return task, errors.New("legacy grouped/unique tasks require manual reconciliation")
				}
			}
		case protowire.VarintType:
			value, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return task, protowire.ParseError(n)
			}
			data = data[n:]
			switch field {
			case 5:
				task.MaxRetry = int(value)
			case 6:
				task.RetryCount = int(value)
			case 8:
				task.Timeout = time.Duration(value) * time.Second
			case 9:
				if value != 0 {
					return task, errors.New("legacy absolute deadline requires manual reconciliation")
				}
			}
		default:
			n := protowire.ConsumeFieldValue(field, typ, data)
			if n < 0 {
				return task, protowire.ParseError(n)
			}
			data = data[n:]
		}
	}
	if task.ID == "" || task.Kind == "" || !validQueue(task.Queue) || task.RetryCount < 0 || task.MaxRetry < task.RetryCount {
		return task, fmt.Errorf("invalid legacy task metadata")
	}
	return task, nil
}

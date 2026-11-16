package aiprotocol

import (
	"github.com/google/uuid"
)

type JobFinish struct {
	JobID    uuid.UUID         `json:"job_id"`
	OwnerID  uuid.UUID         `json:"owner_id"`
	ObjectID uuid.UUID         `json:"object_id"`
	Outcomes map[string]string `json:"outcomes"`
	Status   string            `json:"status"`
}

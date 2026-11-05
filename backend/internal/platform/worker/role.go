package worker

// Role names are also used by deployment and metrics. A role owns one queue
// (except the relay, which never consumes a queue) so it can be scaled and
// rolled back independently.
type Role string

const (
	RoleCombined    Role = "combined"
	RoleRelay       Role = "outbox-relay"
	RoleObject      Role = "object-worker"
	RoleMedia       Role = "media-worker"
	RoleMaintenance Role = "maintenance-worker"
	RoleUpload      Role = "upload-worker"
	RoleSearch      Role = "search-indexer"
)

const (
	QueueObject      = "object"
	QueueMedia       = "media"
	QueueMaintenance = "maintenance"
	QueueUpload      = "upload"
	QueueSearch      = "search"
)

func QueuesForRole(role Role, configured map[string]int) map[string]int {
	if role == RoleCombined {
		result := make(map[string]int, len(configured))
		for name, weight := range configured {
			if name != QueueAI {
				result[name] = weight
			}
		}
		return result
	}
	var queue string
	switch role {
	case RoleObject:
		queue = QueueObject
	case RoleMedia:
		queue = QueueMedia
	case RoleMaintenance:
		queue = QueueMaintenance
	case RoleUpload:
		queue = QueueUpload
	case RoleSearch:
		queue = QueueSearch
	default:
		return map[string]int{}
	}
	weight := configured[queue]
	if weight <= 0 {
		weight = 1
	}
	return map[string]int{queue: weight}
}

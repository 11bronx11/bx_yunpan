package worker

import "testing"

func TestQueuesForRoleOwnsOnlyItsQueue(t *testing.T) {
	configured := map[string]int{QueueAI: 4, QueueObject: 2, QueueMedia: 3, QueueMaintenance: 1, QueueUpload: 2}
	for _, test := range []struct {
		role   Role
		queue  string
		weight int
	}{
		{RoleObject, QueueObject, 2},
		{RoleMedia, QueueMedia, 3},
		{RoleMaintenance, QueueMaintenance, 1},
		{RoleUpload, QueueUpload, 2},
	} {
		queues := QueuesForRole(test.role, configured)
		if len(queues) != 1 || queues[test.queue] != test.weight {
			t.Fatalf("%s queues = %#v", test.role, queues)
		}
	}
	if queues := QueuesForRole(RoleRelay, configured); len(queues) != 0 {
		t.Fatalf("relay unexpectedly owns queues: %#v", queues)
	}
}

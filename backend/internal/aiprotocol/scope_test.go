package aiprotocol

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// D09: empty selected must fail closed; normalization cannot broaden access.
func TestSelectedScopeBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 49, 50, 51} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s := Scope{Mode: "selected"}
			for range n {
				s.FileIDs = append(s.FileIDs, uuid.New())
			}
			err := s.Normalize()
			if (err == nil) != (n > 0 && n <= 50) || s.Mode != "selected" {
				t.Fatal("selected scope broadened or wrong boundary")
			}
		})
	}
	id := uuid.New()
	s := Scope{Mode: "selected", FileIDs: []uuid.UUID{id, id}}
	if s.Normalize() != nil || len(s.FileIDs) != 1 {
		t.Fatal("duplicate IDs are not canonical")
	}
	s = Scope{Mode: "selected", FileIDs: []uuid.UUID{uuid.Nil}}
	if s.Normalize() == nil {
		t.Fatal("nil ID accepted")
	}
	s = Scope{Mode: "all", FileIDs: []uuid.UUID{id}}
	if s.Normalize() == nil {
		t.Fatal("ambiguous all/selected accepted")
	}
}

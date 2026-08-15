package drive

import (
	"errors"
	"testing"
)

func TestDeleteFolderRequiresAnEmptyFolder(t *testing.T) {
	fixture := newMoveFixture(t)

	if err := fixture.service.Delete(fixture.ownerID, fixture.source.ID, fixture.source.Version); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("delete folder containing a file: error = %v, want ErrNotEmpty", err)
	}

	child, err := fixture.service.Create(fixture.ownerID, fixture.target.ID, "child")
	if err != nil {
		t.Fatalf("create child folder: %v", err)
	}
	if err := fixture.service.Delete(fixture.ownerID, fixture.target.ID, fixture.target.Version); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("delete folder containing a directory: error = %v, want ErrNotEmpty", err)
	}
	if err := fixture.service.Delete(fixture.ownerID, child.ID, child.Version); err != nil {
		t.Fatalf("delete empty child folder: %v", err)
	}
	if err := fixture.service.Delete(fixture.ownerID, fixture.target.ID, fixture.target.Version); err != nil {
		t.Fatalf("delete empty target folder: %v", err)
	}
	if _, err := fixture.service.Folder(fixture.ownerID, fixture.target.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load deleted folder: error = %v, want ErrNotFound", err)
	}
}

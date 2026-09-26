package filesystem_test

import (
	"testing"

	"github.com/tiim/photo-collect/internal/storage/filesystem"
	"github.com/tiim/photo-collect/internal/storage/storagetest"
)

func TestContract(t *testing.T) {
	s, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storagetest.Run(t, s)
}

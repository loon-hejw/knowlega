package projectlock

import (
	"strings"
	"testing"
)

func TestAcquireRejectsConcurrentProjectWriter(t *testing.T) {
	root := t.TempDir()
	first, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := Acquire(root); err == nil || !strings.Contains(err.Error(), "locked by another writer") {
		t.Fatalf("second lock error=%v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	_ = second.Release()
}

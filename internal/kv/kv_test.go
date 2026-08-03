package kv

import (
	"testing"

	"rables/internal/db"
)

func openDB(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return NewStore(d)
}

func TestSetGetRoundTrip(t *testing.T) {
	s := openDB(t)
	ctx := t.Context()

	if _, found, err := s.Get(ctx, "missing"); err != nil || found {
		t.Fatalf("Get(missing) = (found=%v, err=%v), want (false, nil)", found, err)
	}
	if err := s.Set(ctx, "k", "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if value, found, err := s.Get(ctx, "k"); err != nil || !found || value != "v1" {
		t.Fatalf("Get(k) = (%q, %v, %v), want (v1, true, nil)", value, found, err)
	}
	// Upsert overwrites.
	if err := s.Set(ctx, "k", "v2"); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	if value, found, err := s.Get(ctx, "k"); err != nil || !found || value != "v2" {
		t.Fatalf("Get(k) after upsert = (%q, %v, %v), want (v2, true, nil)", value, found, err)
	}
}

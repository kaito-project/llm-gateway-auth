package keyindex

import (
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	idx := New()
	if idx.Size() != 0 {
		t.Errorf("expected empty index, got size %d", idx.Size())
	}
}

func TestUpsertAndLookup(t *testing.T) {
	idx := New()
	rec := TenantRecord{Namespace: "team-alpha", KeyID: "a3f7c291"}
	idx.Upsert("hash1", rec)

	got, ok := idx.Lookup("hash1")
	if !ok {
		t.Fatal("expected to find record")
	}
	if got.Namespace != "team-alpha" {
		t.Errorf("expected namespace team-alpha, got %s", got.Namespace)
	}
	if got.KeyID != "a3f7c291" {
		t.Errorf("expected keyID a3f7c291, got %s", got.KeyID)
	}
}

func TestLookupMiss(t *testing.T) {
	idx := New()
	_, ok := idx.Lookup("nonexistent")
	if ok {
		t.Error("expected miss for nonexistent hash")
	}
}

func TestUpsertReplacesOldHash(t *testing.T) {
	idx := New()
	idx.Upsert("hash1", TenantRecord{Namespace: "team-alpha", KeyID: "key1"})
	idx.Upsert("hash2", TenantRecord{Namespace: "team-alpha", KeyID: "key2"})

	if _, ok := idx.Lookup("hash1"); ok {
		t.Error("expected old hash to be removed")
	}
	got, ok := idx.Lookup("hash2")
	if !ok {
		t.Fatal("expected to find new record")
	}
	if got.KeyID != "key2" {
		t.Errorf("expected keyID key2, got %s", got.KeyID)
	}
	if idx.Size() != 1 {
		t.Errorf("expected size 1, got %d", idx.Size())
	}
}

func TestRemoveByHash(t *testing.T) {
	idx := New()
	idx.Upsert("hash1", TenantRecord{Namespace: "ns1", KeyID: "k1"})
	idx.Upsert("hash2", TenantRecord{Namespace: "ns2", KeyID: "k2"})
	idx.RemoveByHash("hash1")

	if _, ok := idx.Lookup("hash1"); ok {
		t.Error("expected hash1 to be removed")
	}
	if _, ok := idx.Lookup("hash2"); !ok {
		t.Error("expected hash2 to still exist")
	}
	if idx.Size() != 1 {
		t.Errorf("expected size 1, got %d", idx.Size())
	}
}

func TestRemoveByNamespace(t *testing.T) {
	idx := New()
	idx.Upsert("hash1", TenantRecord{Namespace: "ns1", KeyID: "k1"})
	idx.Upsert("hash2", TenantRecord{Namespace: "ns2", KeyID: "k2"})
	idx.RemoveByNamespace("ns1")

	if _, ok := idx.Lookup("hash1"); ok {
		t.Error("expected hash1 to be removed")
	}
	if idx.Size() != 1 {
		t.Errorf("expected size 1, got %d", idx.Size())
	}
}

func TestExpiresAt(t *testing.T) {
	idx := New()
	expired := time.Now().Add(-1 * time.Hour)
	idx.Upsert("hash1", TenantRecord{
		Namespace: "ns1",
		KeyID:     "k1",
		ExpiresAt: &expired,
	})

	rec, ok := idx.Lookup("hash1")
	if !ok {
		t.Fatal("expected to find record (index does not check expiry)")
	}
	if rec.ExpiresAt == nil || !rec.ExpiresAt.Before(time.Now()) {
		t.Error("expected record to have past expiry")
	}
}

func TestSetNamespaceKeys_MultipleEntries(t *testing.T) {
	idx := New()

	// Simulate rotation: two keys for the same namespace
	entries := map[string]TenantRecord{
		"hashCurrent": {Namespace: "team-alpha", KeyID: "newKey"},
		"hashPrev":    {Namespace: "team-alpha", KeyID: "oldKey"},
	}
	idx.SetNamespaceKeys("team-alpha", entries)

	if idx.Size() != 2 {
		t.Errorf("expected size 2, got %d", idx.Size())
	}

	rec1, ok := idx.Lookup("hashCurrent")
	if !ok {
		t.Fatal("expected to find current key")
	}
	if rec1.KeyID != "newKey" {
		t.Errorf("expected keyID newKey, got %s", rec1.KeyID)
	}

	rec2, ok := idx.Lookup("hashPrev")
	if !ok {
		t.Fatal("expected to find previous key")
	}
	if rec2.KeyID != "oldKey" {
		t.Errorf("expected keyID oldKey, got %s", rec2.KeyID)
	}
}

func TestSetNamespaceKeys_ReplacesOldEntries(t *testing.T) {
	idx := New()

	// First: set two keys
	idx.SetNamespaceKeys("ns1", map[string]TenantRecord{
		"hash1": {Namespace: "ns1", KeyID: "k1"},
		"hash2": {Namespace: "ns1", KeyID: "k2"},
	})
	if idx.Size() != 2 {
		t.Errorf("expected size 2, got %d", idx.Size())
	}

	// Replace with single key (grace period ended)
	idx.SetNamespaceKeys("ns1", map[string]TenantRecord{
		"hash3": {Namespace: "ns1", KeyID: "k3"},
	})
	if idx.Size() != 1 {
		t.Errorf("expected size 1, got %d", idx.Size())
	}
	if _, ok := idx.Lookup("hash1"); ok {
		t.Error("expected hash1 removed")
	}
	if _, ok := idx.Lookup("hash2"); ok {
		t.Error("expected hash2 removed")
	}
	if _, ok := idx.Lookup("hash3"); !ok {
		t.Error("expected hash3 present")
	}
}

func TestRemoveByHash_WithMultipleEntries(t *testing.T) {
	idx := New()

	idx.SetNamespaceKeys("ns1", map[string]TenantRecord{
		"hashA": {Namespace: "ns1", KeyID: "kA"},
		"hashB": {Namespace: "ns1", KeyID: "kB"},
	})

	idx.RemoveByHash("hashA")

	if _, ok := idx.Lookup("hashA"); ok {
		t.Error("expected hashA removed")
	}
	rec, ok := idx.Lookup("hashB")
	if !ok {
		t.Fatal("expected hashB to still exist")
	}
	if rec.KeyID != "kB" {
		t.Errorf("expected keyID kB, got %s", rec.KeyID)
	}
	if idx.Size() != 1 {
		t.Errorf("expected size 1, got %d", idx.Size())
	}
}

func TestRemoveByNamespace_WithMultipleEntries(t *testing.T) {
	idx := New()

	idx.SetNamespaceKeys("ns1", map[string]TenantRecord{
		"hashA": {Namespace: "ns1", KeyID: "kA"},
		"hashB": {Namespace: "ns1", KeyID: "kB"},
	})
	idx.Upsert("hashC", TenantRecord{Namespace: "ns2", KeyID: "kC"})

	idx.RemoveByNamespace("ns1")

	if _, ok := idx.Lookup("hashA"); ok {
		t.Error("expected hashA removed")
	}
	if _, ok := idx.Lookup("hashB"); ok {
		t.Error("expected hashB removed")
	}
	if _, ok := idx.Lookup("hashC"); !ok {
		t.Error("expected hashC for ns2 to remain")
	}
	if idx.Size() != 1 {
		t.Errorf("expected size 1, got %d", idx.Size())
	}
}

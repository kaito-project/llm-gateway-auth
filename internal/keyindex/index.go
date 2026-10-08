// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package keyindex

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// TenantRecord holds the metadata associated with an API key hash.
type TenantRecord struct {
	Namespace string
	KeyID     string
	// ExpiresAt is zero only for a primary key whose Secret metadata explicitly
	// declared that it never expires.
	ExpiresAt time.Time
}

// Index is a concurrency-safe in-memory index mapping SHA-256 key hashes
// to tenant records. Supports multiple hashes per namespace (primary + secondary
// key during rotation grace periods).
type Index struct {
	mu          sync.RWMutex
	byHash      map[string]TenantRecord
	byNamespace map[string][]string
}

var indexSize = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "apikey_authz_index_size",
	Help: "Number of entries in the in-memory key index",
})

// New creates a new empty key index.
func New() *Index {
	return &Index{
		byHash:      make(map[string]TenantRecord),
		byNamespace: make(map[string][]string),
	}
}

// Upsert inserts or updates a tenant record, replacing all previous entries
// for the same namespace. For adding multiple keys (e.g. during rotation),
// use SetNamespaceKeys instead.
func (idx *Index) Upsert(hash string, rec TenantRecord) {
	idx.SetNamespaceKeys(rec.Namespace, map[string]TenantRecord{hash: rec})
}

// SetNamespaceKeys atomically replaces all entries for a namespace with the
// provided entries. This is used during rotation to maintain both the primary
// and secondary key in the index simultaneously.
func (idx *Index) SetNamespaceKeys(ns string, entries map[string]TenantRecord) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Remove old entries for this namespace
	for _, h := range idx.byNamespace[ns] {
		delete(idx.byHash, h)
	}

	// Add new entries
	hashes := make([]string, 0, len(entries))
	for h, rec := range entries {
		idx.byHash[h] = rec
		hashes = append(hashes, h)
	}
	if len(hashes) > 0 {
		idx.byNamespace[ns] = hashes
	} else {
		delete(idx.byNamespace, ns)
	}

	indexSize.Set(float64(len(idx.byHash)))
}

// Lookup retrieves a tenant record by the SHA-256 hash of the raw key.
func (idx *Index) Lookup(hash string) (TenantRecord, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	rec, ok := idx.byHash[hash]
	return rec, ok
}

// RemoveByHash removes an entry by its SHA-256 hash.
func (idx *Index) RemoveByHash(hash string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if rec, ok := idx.byHash[hash]; ok {
		delete(idx.byHash, hash)
		hashes := idx.byNamespace[rec.Namespace]
		for i, h := range hashes {
			if h == hash {
				idx.byNamespace[rec.Namespace] = append(hashes[:i], hashes[i+1:]...)
				break
			}
		}
		if len(idx.byNamespace[rec.Namespace]) == 0 {
			delete(idx.byNamespace, rec.Namespace)
		}
		indexSize.Set(float64(len(idx.byHash)))
	}
}

// RemoveByNamespace removes all entries associated with a given namespace.
func (idx *Index) RemoveByNamespace(ns string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for _, h := range idx.byNamespace[ns] {
		delete(idx.byHash, h)
	}
	delete(idx.byNamespace, ns)
	indexSize.Set(float64(len(idx.byHash)))
}

// Size returns the number of entries in the index.
func (idx *Index) Size() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.byHash)
}

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// Opaque PEM-shaped filler used to populate one half of a Secret while the
// other half is missing — the tests below cover the early-return branches in
// loadCertificateFromSecret, none of which actually parse this material. A
// happy-path parsing test would require generating a fresh keypair (or
// committing one), which isn't worth the maintenance overhead given e2e
// exercises the real cert-controller-issued cert end-to-end.
const (
	testCert = `-----BEGIN CERTIFICATE-----
not-a-real-cert
-----END CERTIFICATE-----
`
	testKey = `-----BEGIN PRIVATE KEY-----
not-a-real-key
-----END PRIVATE KEY-----
`
)

// newLister returns a Secret lister backed by a fake clientset, populated with
// the provided secrets. The lister's informer cache is fully synced before
// returning so the test doesn't race against the watch.
func newLister(t *testing.T, secrets ...*corev1.Secret) corev1listers.SecretLister {
	t.Helper()
	objs := make([]runtime.Object, 0, len(secrets))
	for _, s := range secrets {
		objs = append(objs, s)
	}
	client := fake.NewClientset(objs...)
	factory := informers.NewSharedInformerFactory(client, 0)
	lister := factory.Core().V1().Secrets().Lister()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	factory.Start(stop)
	factory.WaitForCacheSync(stop)
	return lister
}

// All four cases below correspond to the bootstrap window described in
// issue #57: the rotator-managed Secret either doesn't exist yet, or exists
// but hasn't had a serving cert written to it. loadCertificateFromSecret must
// return a non-nil error in each case so the readyz check fails and the pod
// is kept out of the webhook Service endpoints.

func TestLoadCertificateFromSecret_NotFound(t *testing.T) {
	t.Parallel()
	lister := newLister(t)
	_, err := loadCertificateFromSecret(lister, "ns", "tls")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestLoadCertificateFromSecret_MissingCert(t *testing.T) {
	t.Parallel()
	lister := newLister(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "ns"},
		Data:       map[string][]byte{"tls.key": []byte(testKey)},
	})
	_, err := loadCertificateFromSecret(lister, "ns", "tls")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls.crt missing")
}

func TestLoadCertificateFromSecret_MissingKey(t *testing.T) {
	t.Parallel()
	lister := newLister(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "ns"},
		Data:       map[string][]byte{"tls.crt": []byte(testCert)},
	})
	_, err := loadCertificateFromSecret(lister, "ns", "tls")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls.key missing")
}

func TestLoadCertificateFromSecret_EmptyCertBytes(t *testing.T) {
	t.Parallel()
	lister := newLister(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: "ns"},
		Data:       map[string][]byte{"tls.crt": {}, "tls.key": []byte(testKey)},
	})
	_, err := loadCertificateFromSecret(lister, "ns", "tls")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls.crt missing")
}

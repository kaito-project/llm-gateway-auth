package azure

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetFlags() {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
}

func TestParseFlags_Defaults(t *testing.T) {
	resetFlags()
	os.Args = []string{"test"}

	cfg, err := ParseFlags()
	require.NoError(t, err)

	assert.Equal(t, ":9001", cfg.GRPCAddr)
	assert.Equal(t, ":9090", cfg.MetricsAddr)
	assert.Equal(t, "aimanager", cfg.SARAPIGroup)
	assert.Equal(t, "namespaces", cfg.SARResource)
	assert.Equal(t, "inference", cfg.SARVerb)
	assert.False(t, cfg.FailOpen)
	assert.Equal(t, 1024, cfg.CacheSize)
}

func TestParseFlags_EnvOverride(t *testing.T) {
	resetFlags()
	os.Args = []string{"test"}
	t.Setenv("KUBECONFIG", "/some/kubeconfig")

	cfg, err := ParseFlags()
	require.NoError(t, err)

	assert.Equal(t, "/some/kubeconfig", cfg.KubeconfigPath)
}

func TestParseFlags_FailOpenEnv(t *testing.T) {
	resetFlags()
	os.Args = []string{"test"}
	t.Setenv("EXTAUTHZ_FAIL_OPEN", "true")

	cfg, err := ParseFlags()
	require.NoError(t, err)

	assert.True(t, cfg.FailOpen)
}

func TestParseFlags_TLSBothOrNeither(t *testing.T) {
	resetFlags()
	os.Args = []string{"test", "--tls-cert-file=/cert"}

	_, err := ParseFlags()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "tls-cert-file")
}

func TestParseFlags_InvalidCacheSize(t *testing.T) {
	resetFlags()
	os.Args = []string{"test", "--cache-size=0"}

	_, err := ParseFlags()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cache-size")
}

func TestValidate_PositiveRequestTimeout(t *testing.T) {
	resetFlags()
	os.Args = []string{"test", "--request-timeout=0s"}

	_, err := ParseFlags()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "request-timeout")
}

func TestValidate_PositiveCacheTTL(t *testing.T) {
	resetFlags()
	os.Args = []string{"test", "--cache-ttl=0s"}

	_, err := ParseFlags()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cache-ttl")
}

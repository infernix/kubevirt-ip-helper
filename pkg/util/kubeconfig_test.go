package util

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
)

// the informer client must not carry the one-shot client timeout: it
// would tear the watch connection down every time it expires and an
// initial list slower than the timeout would never complete
func TestWatchRestConfigStripsTheClientTimeout(t *testing.T) {
	config := &rest.Config{Host: "https://example.com", Timeout: 30 * time.Second}

	watchConfig := WatchRestConfig(config)

	if watchConfig.Timeout != 0 {
		t.Errorf("watch config timeout = %v, want it stripped", watchConfig.Timeout)
	}
	if watchConfig.Host != config.Host {
		t.Errorf("watch config host = %q, want %q preserved", watchConfig.Host, config.Host)
	}
	// the source config stays untouched: the one-shot clients keep their
	// bound
	if config.Timeout != 30*time.Second {
		t.Errorf("source config timeout = %v, want it untouched", config.Timeout)
	}
}

// the one-shot clients built from GetKubeConfig must carry a request
// bound (F08): without it a tcp blackhole against the api hangs the
// webhook's list, csr, secret and webhook-configuration calls forever
func TestGetKubeConfigBoundsOneShotRequests(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://example.com
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`
	if err := os.WriteFile(kubeconfig, []byte(content), 0600); err != nil {
		t.Fatalf("writing test kubeconfig: %s", err)
	}

	config, err := GetKubeConfig(kubeconfig, "")
	if err != nil {
		t.Fatalf("GetKubeConfig: %v", err)
	}
	if config.Timeout != 30*time.Second {
		t.Errorf("config timeout = %v, want the 30s one-shot bound", config.Timeout)
	}
}

// writeKubeconfigForServer writes a minimal kubeconfig pointing at the
// given API server URL and returns its path.
func writeKubeconfigForServer(t *testing.T, server string) string {
	t.Helper()

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: ` + server + `
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`
	if err := os.WriteFile(kubeconfig, []byte(content), 0600); err != nil {
		t.Fatalf("writing test kubeconfig: %s", err)
	}

	return kubeconfig
}

// The client-side rate limits are the product decision of the release
// path, not an implementation detail: client-go's default 5 QPS with a
// burst of 10 made one twelve-request release cost 2.4s of pure limiter
// wait (a measured 17.2s serialized n=8 drain) and a 64-address live
// drain 183s (21 addresses/min). The values are pinned literally so a
// silent return to the defaults - or to an unbounded limiter - fails
// here instead of in a production drain.
func TestGetKubeConfigCarriesTheReleasePathRateLimits(t *testing.T) {
	config, err := GetKubeConfig(writeKubeconfigForServer(t, "https://example.com"), "")
	if err != nil {
		t.Fatalf("GetKubeConfig: %v", err)
	}

	if config.QPS != 50 {
		t.Errorf("config QPS = %v, want 50 (client-go's default 5 throttles a twelve-request release to 2.4s)", config.QPS)
	}
	if config.Burst != 100 {
		t.Errorf("config Burst = %v, want 100 (two full 48-reservation E2E batches of twelve-request reconciles)", config.Burst)
	}
	if config.RateLimiter != nil {
		t.Error("config RateLimiter is set, want the clientset to build it from QPS/Burst so every client shares the documented bound")
	}
}

// The informer config is copied from the one-shot config, so the watch
// clientsets inherit the rate limits together with the stripped timeout.
func TestWatchRestConfigKeepsTheRateLimits(t *testing.T) {
	config, err := GetKubeConfig(writeKubeconfigForServer(t, "https://example.com"), "")
	if err != nil {
		t.Fatalf("GetKubeConfig: %v", err)
	}

	watchConfig := WatchRestConfig(config)

	if watchConfig.QPS != 50 || watchConfig.Burst != 100 {
		t.Errorf("watch config QPS/Burst = %v/%v, want the 50/100 of the one-shot config", watchConfig.QPS, watchConfig.Burst)
	}
}

// The release path must not be limiter-bound: 96 sequential reads through
// a clientset built from GetKubeConfig - the twelve requests of eight
// releases - cost (96-10)/5 = 17.2s under client-go's default limiter and
// fit inside the burst of the configured 50 QPS. The bound is generous
// because it only has to separate "no limiter wait" from "17s of it".
func TestGetKubeConfigClientsAreNotLimiterBound(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		pool := &kihv1.IPPool{
			TypeMeta:   metav1.TypeMeta{Kind: "IPPool", APIVersion: "kubevirtiphelper.k8s.binbash.org/v1"},
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", ResourceVersion: "1"},
		}
		if err := json.NewEncoder(w).Encode(pool); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)

	config, err := GetKubeConfig(writeKubeconfigForServer(t, server.URL), "")
	if err != nil {
		t.Fatalf("GetKubeConfig: %v", err)
	}
	client, err := kihclientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}

	const reads = 96
	start := time.Now()
	for range reads {
		if _, err := client.KubevirtiphelperV1().IPPools().Get(context.Background(), "pool-a", metav1.GetOptions{}); err != nil {
			t.Fatalf("IPPool get: %v", err)
		}
	}
	elapsed := time.Since(start)

	if got := requests.Load(); got != reads {
		t.Fatalf("server saw %d requests, want %d", got, reads)
	}
	if elapsed > 2*time.Second {
		t.Errorf("%d sequential reads took %s, want them inside the configured burst - client-go's default limiter needs 17.2s", reads, elapsed)
	}
}

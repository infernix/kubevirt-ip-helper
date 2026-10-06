package util

import (
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// configTimeout bounds every one-shot request of the clients built from
// this config (F08): without it a tcp blackhole against the api hangs the
// caller forever - the webhook's list, csr, secret and webhook-configuration
// calls all run on it. 30s matches the bound of the controller-side
// kubeconfig builders. the informer clients are not built from the bound
// config: the controllers strip it through WatchRestConfig (below), and
// nothing in the webhook watches.
const configTimeout = 30 * time.Second

// The helper is low-traffic by design, so client-go's default client-side
// rate limiter (5 QPS, burst 10) is the wrong bound for its clients. One
// release is a vmnetcfg finalizer reconcile issuing twelve sequential API
// requests (the pool list and get, the status write, the accounting and
// metrics reads, the vmnetcfg list and four gets, and two vmnetcfg
// writes), so once the burst is spent a release costs 12/5 = 2.4s of pure
// limiter wait. Measured: a serialized n=8 drain took 17.2s - exactly the
// (12n-10)/5 model - while the same drain with the limiter raised to
// 1000/1000 took 5.3ms; a 64-address live drain took 183s (2.86s per
// release, 21 addresses/min) and reported zero IPPool-status conflicts,
// and a fresh reservation waited 138s behind a 100-VM drain. The limiter,
// not status contention, was the bottleneck.
//
// clientQPS/clientBurst keep a bounded ceiling instead of removing the
// limiter. 50 QPS turns one release into ~0.24s (12/50) and a 100-VM
// drain from ~4 minutes into ~25s, while still capping a runaway
// reconcile loop at 50 requests/s (~1% of what one apiserver serves) so
// the helper can never flood the API server. Burst 100 starts two full
// 48-reservation E2E batches (96 twelve-request reconciles) without
// throttling their first attempts, and stays an order of magnitude above
// the twelve requests a single release issues. Both are compile-time
// constants: they are a product decision of this helper, not a
// per-deployment knob, and a constant cannot fail at runtime the way a
// malformed environment override would.
const (
	clientQPS   = 50
	clientBurst = 100
)

// GetKubeConfig returns the rest config for the given kubeconfig file and
// context, falling back to the in-cluster config when the file does not
// exist (the webhook and controller kubeconfig-detection paths share this
// behavior: an unreadable kubeconfig resolves to the in-cluster config
// rather than to a hard failure).
//
// This is the single construction point of every one-shot client of the
// process - the app's leader-election and startup clients, the vm,
// vmnetcfg and ippool controller clientsets and their kubevirt clients,
// and the webhook's clients - so the rate limits above are applied here
// instead of at each clientset. The informer clientsets are built from
// WatchRestConfig(config) and inherit both the limits and the stripped
// timeout.
func GetKubeConfig(kubeConfig string, kubeContext string) (config *rest.Config, err error) {
	if !FileExists(kubeConfig) {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeConfig},
			&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: kubeContext},
		).ClientConfig()
	}
	if err != nil {
		return
	}

	config.Timeout = configTimeout
	config.QPS = clientQPS
	config.Burst = clientBurst

	return
}

// WatchRestConfig strips the one-shot client timeout for the informer
// client: the timeout applies to the watch connections too, so the
// reflector's long-poll would be torn down by the http client every time
// it expires (a constant re-watch churn), and an initial list which takes
// longer than the timeout would never complete, leaving the controller
// blocked in the cache sync wait. the one-shot bound stays on the config
// handed to the one-shot clientsets.
func WatchRestConfig(config *rest.Config) *rest.Config {
	watchConfig := rest.CopyConfig(config)
	watchConfig.Timeout = 0

	return watchConfig
}

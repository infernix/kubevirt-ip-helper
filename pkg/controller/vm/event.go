package vm

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	kubevirtv1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	kihcache "github.com/joeyloman/kubevirt-ip-helper/pkg/cache"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ippoolstatus"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	log "github.com/sirupsen/logrus"
)

const (
	// the add/delete tokens are the ledger mutations of ippoolstatus; the
	// aliases keep the event values and the status-write switch from
	// drifting apart
	ADD    = ippoolstatus.EventAdd
	UPDATE = "update"
	DELETE = ippoolstatus.EventDelete
)

// resyncPeriod re-delivers every watched object periodically: virtual
// machines whose earlier event was dropped (a transient sync failure) get
// another reconciliation chance without any external event or pod restart.
const resyncPeriod = time.Minute

type EventHandler struct {
	ctx            context.Context
	ipam           *ipam.IPAllocator
	dhcp           *dhcp.DHCPAllocator
	metrics        *metrics.MetricsAllocator
	cache          *kihcache.CacheAllocator
	kubeConfig     string
	kubeContext    string
	kubeRestConfig *rest.Config
	kihClientset   *kihclientset.Clientset
	kcli           kubecli.KubevirtClient
	scope          util.NetworkScope
	reconcileMu    *sync.Mutex

	// staticIPReleases records the interfaces whose static ip request was
	// removed or changed between the observed updates: the vm controller
	// releases their stored address at the next reconciliation. It is
	// shared with the controller of this era and guarded by its own mutex
	staticIPReleases *staticIPReleases
}

type Event struct {
	key         string
	action      string
	vmName      string
	vmNamespace string
}

func NewEventHandler(
	ctx context.Context,
	ipam *ipam.IPAllocator,
	dhcp *dhcp.DHCPAllocator,
	metrics *metrics.MetricsAllocator,
	cache *kihcache.CacheAllocator,
	kubeConfig string,
	kubeContext string,
	kubeRestConfig *rest.Config,
	kihClientset *kihclientset.Clientset,
	kcli kubecli.KubevirtClient,
	scope util.NetworkScope,
	reconcileMu *sync.Mutex,
) *EventHandler {
	return &EventHandler{
		ctx:              ctx,
		ipam:             ipam,
		dhcp:             dhcp,
		metrics:          metrics,
		cache:            cache,
		kubeConfig:       kubeConfig,
		kubeContext:      kubeContext,
		kubeRestConfig:   kubeRestConfig,
		kihClientset:     kihClientset,
		kcli:             kcli,
		scope:            scope,
		reconcileMu:      reconcileMu,
		staticIPReleases: newStaticIPReleases(),
	}
}

func (e *EventHandler) Init() (err error) {
	e.kubeRestConfig, err = e.getKubeConfig()
	if err != nil {
		return
	}

	e.kihClientset, err = kihclientset.NewForConfig(e.kubeRestConfig)
	if err != nil {
		return
	}

	e.kcli, err = kubecli.GetKubevirtClientFromRESTConfig(watchRestConfig(e.kubeRestConfig))
	if err != nil {
		return
	}

	return
}

// watchRestConfig strips the one-shot client timeout for the informer
// client: the timeout applies to the watch connections too, so the
// reflector's long-poll would be torn down by the http client every time
// it expires (a constant re-watch churn), and an initial list which takes
// longer than the timeout would never complete, leaving the controller
// blocked in the cache sync wait. the one-shot bound stays on the config
// handed to the kihClientset.
func watchRestConfig(config *rest.Config) *rest.Config {
	watchConfig := rest.CopyConfig(config)
	watchConfig.Timeout = 0

	return watchConfig
}

func (e *EventHandler) getKubeConfig() (config *rest.Config, err error) {
	// bound every controller api call: a hang against the api must not
	// wedge the reconcilers behind an unresponsive transport
	const configTimeout = 30 * time.Second

	if !util.FileExists(e.kubeConfig) {
		if config, err = rest.InClusterConfig(); err != nil {
			return
		}
		config.Timeout = configTimeout

		return
	}

	config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: e.kubeConfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: e.kubeContext},
	).ClientConfig()
	if err == nil {
		config.Timeout = configTimeout
	}

	return
}

func (e *EventHandler) EventListener() (err error) {
	log.Infof("(vm.EventListener) starting the VirtualMachine event listener")

	vmWatcher := cache.NewListWatchFromClient(e.kcli.RestClient(), "virtualmachines", corev1.NamespaceAll, fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(vmWatcher, &kubevirtv1.VirtualMachine{}, resyncPeriod, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(Event{
					key:         key,
					action:      ADD,
					vmName:      obj.(*kubevirtv1.VirtualMachine).GetName(),
					vmNamespace: obj.(*kubevirtv1.VirtualMachine).GetNamespace(),
				})
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			e.enqueueVirtualMachineUpdate(queue, old, new)
		},
		DeleteFunc: func(obj interface{}) {
			e.enqueueVirtualMachineDelete(queue, obj)
		},
	}, cache.Indexers{})

	controller := NewController(e.ctx, queue, indexer, informer, e.cache, e.ipam, e.dhcp, e.metrics, e.kihClientset, e.scope, e.reconcileMu, e.staticIPReleases)
	stop := make(chan struct{})

	// join the controller on shutdown: EventListener only returns after
	// Controller.Run has fully stopped (its worker finished the in-flight
	// sync and exited), so the application restart flow can wait for the
	// old generation to be gone
	done := make(chan struct{})
	go func() {
		controller.Run(1, stop)
		close(done)
	}()

	select {
	case <-e.ctx.Done():
		log.Infof("(vm.EventListener) stopping the VirtualMachine event listener")
		close(stop)
		<-done
		return
	}
}

// enqueueVirtualMachineUpdate records the static ip requests an update
// removed or changed and enqueues the reconciliation of the virtual machine:
// the projection must not carry the stored address of such an interface over,
// so the vmnetcfg controller releases it. An update whose payload is no
// longer a VirtualMachine (a stale final state) does not come from this
// informer and stays dropped, like the delete handler's foreign objects.
func (e *EventHandler) enqueueVirtualMachineUpdate(queue workqueue.RateLimitingInterface, old interface{}, new interface{}) {
	virtualMachine, isVM := new.(*kubevirtv1.VirtualMachine)
	if !isVM {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(virtualMachine)
	if err != nil {
		return
	}

	e.recordStaticIPReleases(key, old, virtualMachine)

	queue.Add(Event{
		key:         key,
		action:      UPDATE,
		vmName:      virtualMachine.GetName(),
		vmNamespace: virtualMachine.GetNamespace(),
	})
}

// recordStaticIPReleases records the interfaces whose static ip request was
// removed or changed since the observed update: the projection of the next
// reconciliation releases their stored address. A malformed annotation is
// ignored, mirroring the fail-soft projection of the vm controller: the
// admission check rejects it, while a hand-edited vm keeps serving the
// address it holds.
func (e *EventHandler) recordStaticIPReleases(key string, old interface{}, new *kubevirtv1.VirtualMachine) {
	oldVM, isVM := old.(*kubevirtv1.VirtualMachine)
	if !isVM {
		return
	}

	oldIPs, oldErr := util.ParseStaticIPAnnotation(oldVM.ObjectMeta.Annotations)
	if oldErr != nil {
		log.Warnf("(vm.EventListener) [%s] ignoring the previous static ip annotation: %s", key, oldErr)

		oldIPs = nil
	}
	newIPs, newErr := util.ParseStaticIPAnnotation(new.ObjectMeta.Annotations)
	if newErr != nil {
		// the projection ignores a malformed annotation, so it must not
		// look like a removal of every previous request
		log.Warnf("(vm.EventListener) [%s] ignoring the static ip annotation: %s", key, newErr)

		return
	}

	var released map[string]bool
	for nicName, oldIP := range oldIPs {
		if newIP, requested := newIPs[nicName]; !requested || newIP != oldIP {
			if released == nil {
				released = make(map[string]bool)
			}
			released[nicName] = true
		}
	}
	if len(released) == 0 {
		return
	}

	log.Infof("(vm.EventListener) [%s] the static ip request of %d interface(s) was removed or changed, releasing their addresses at the next reconciliation",
		key, len(released))
	e.staticIPReleases.record(key, released)
}

// enqueueVirtualMachineDelete derives the cleanup event for a deleted
// virtual machine: a tombstone whose payload is no longer a VirtualMachine
// (a stale final state from a relist) still identifies the deleted object
// through its key, and dropping the event would strand the vmnetcfg object
// and its allocations forever. objects which are neither a virtual machine
// nor a tombstone do not come from this informer and stay dropped.
func (e *EventHandler) enqueueVirtualMachineDelete(queue workqueue.RateLimitingInterface, obj interface{}) {
	virtualMachine, isVM := util.UnwrapTombstone(obj).(*kubevirtv1.VirtualMachine)

	if !isVM {
		if _, isTombstone := obj.(cache.DeletedFinalStateUnknown); !isTombstone {
			return
		}
	}

	var key string
	var vmNamespace string
	var vmName string

	if isVM {
		var err error
		key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(virtualMachine)
		if err != nil {
			return
		}
		vmNamespace = virtualMachine.GetNamespace()
		vmName = virtualMachine.GetName()
	} else {
		var err error
		key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
		if err != nil {
			return
		}

		ns, name, splitErr := cache.SplitMetaNamespaceKey(key)
		if splitErr != nil {
			return
		}

		log.Warnf("(vm.EventListener) virtualmachine delete payload is no longer a VirtualMachine, deriving the cleanup from key %s", key)

		vmNamespace = ns
		vmName = name
	}

	queue.Add(Event{
		key:         key,
		action:      DELETE,
		vmName:      vmName,
		vmNamespace: vmNamespace,
	})
}

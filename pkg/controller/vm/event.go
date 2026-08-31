package vm

import (
	"context"

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
	"github.com/joeyloman/kubevirt-ip-helper/pkg/controller/ownership"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/dhcp"
	kihclientset "github.com/joeyloman/kubevirt-ip-helper/pkg/generated/clientset/versioned"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/ipam"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/metrics"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	log "github.com/sirupsen/logrus"
)

const (
	ADD    = "add"
	UPDATE = "update"
	DELETE = "delete"
)

type EventHandler struct {
	ctx            context.Context
	ipam           *ipam.IPAllocator
	dhcp           *dhcp.DHCPAllocator
	metrics        *metrics.MetricsAllocator
	cache          *kihcache.CacheAllocator
	scope          *ownership.Scope
	kubeConfig     string
	kubeContext    string
	kubeRestConfig *rest.Config
	kihClientset   *kihclientset.Clientset
	kcli           kubecli.KubevirtClient
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
	scope *ownership.Scope,
	kubeConfig string,
	kubeContext string,
	kubeRestConfig *rest.Config,
	kihClientset *kihclientset.Clientset,
	kcli kubecli.KubevirtClient,
) *EventHandler {
	return &EventHandler{
		ctx:            ctx,
		ipam:           ipam,
		dhcp:           dhcp,
		metrics:        metrics,
		cache:          cache,
		scope:          scope,
		kubeConfig:     kubeConfig,
		kubeContext:    kubeContext,
		kubeRestConfig: kubeRestConfig,
		kihClientset:   kihClientset,
		kcli:           kcli,
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

	e.kcli, err = kubecli.GetKubevirtClientFromRESTConfig(e.kubeRestConfig)
	if err != nil {
		return
	}

	return
}

func (e *EventHandler) getKubeConfig() (config *rest.Config, err error) {
	if !util.FileExists(e.kubeConfig) {
		return rest.InClusterConfig()
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: e.kubeConfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{}, CurrentContext: e.kubeContext},
	).ClientConfig()
}

func (e *EventHandler) EventListener() (err error) {
	log.Infof("(vm.EventListener) starting the VirtualMachine event listener")

	vmWatcher := cache.NewListWatchFromClient(e.kcli.RestClient(), "virtualmachines", corev1.NamespaceAll, fields.Everything())

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(vmWatcher, &kubevirtv1.VirtualMachine{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			vm := obj.(*kubevirtv1.VirtualMachine)
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(Event{
					key:         key,
					action:      ADD,
					vmName:      vm.GetName(),
					vmNamespace: vm.GetNamespace(),
				})
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			vm := new.(*kubevirtv1.VirtualMachine)
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(Event{
					key:         key,
					action:      UPDATE,
					vmName:      vm.GetName(),
					vmNamespace: vm.GetNamespace(),
				})
			}
		},
		DeleteFunc: func(obj interface{}) {
			vm, ok := deletedVirtualMachine(obj)
			if !ok {
				return
			}

			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(Event{
					key:         key,
					action:      DELETE,
					vmName:      vm.GetName(),
					vmNamespace: vm.GetNamespace(),
				})
			}
		},
	}, cache.Indexers{})

	controller := NewController(queue, indexer, informer, e.cache, e.scope, e.ipam, e.dhcp, e.metrics, e.kihClientset)
	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(1, stop)

	select {
	case <-e.ctx.Done():
		log.Infof("(vm.EventListener) stopping the VirtualMachine event listener")
		return
	}
}

func deletedVirtualMachine(obj interface{}) (*kubevirtv1.VirtualMachine, bool) {
	if vm, ok := obj.(*kubevirtv1.VirtualMachine); ok {
		return vm, vm != nil
	}

	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, false
	}

	vm, ok := tombstone.Obj.(*kubevirtv1.VirtualMachine)
	return vm, ok && vm != nil
}

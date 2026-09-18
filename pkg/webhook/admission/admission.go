package admission

import (
	"context"
	"fmt"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	log "github.com/sirupsen/logrus"
	admregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type Handler struct {
	ctx                         context.Context
	kubeConfig                  string
	kubeContext                 string
	clientset                   *kubernetes.Clientset
	webhookNamespace            string
	webhookName                 string
	validatingWebhookConfigName string
}

func Register(ctx context.Context, kubeConfig string, kubeContext string, webhookName string, webhookNamespace string, validatingWebhookConfigName string) *Handler {
	return &Handler{
		ctx:                         ctx,
		kubeConfig:                  kubeConfig,
		kubeContext:                 kubeContext,
		webhookName:                 webhookName,
		webhookNamespace:            webhookNamespace,
		validatingWebhookConfigName: validatingWebhookConfigName,
	}
}

func (h *Handler) Init() {
	config, err := util.GetKubeConfig(h.kubeConfig, h.kubeContext)
	if err != nil {
		log.Panicf("%s", err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Panicf("%s", err.Error())
	}
	h.clientset = clientset

	if err := h.AddValidatingWebhookConfiguration(); err != nil {
		log.Panicf("%s", err.Error())
	}
}

func (h *Handler) checkValidatingWebhookConfiguration() bool {
	_, err := h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), h.validatingWebhookConfigName, metav1.GetOptions{})
	if err != nil {
		return false
	}

	return true
}

func (h *Handler) AddValidatingWebhookConfiguration() (err error) {
	if h.checkValidatingWebhookConfiguration() {
		// the configuration already exists (created by an earlier version
		// of this webhook or by an operator): reconcile the admission
		// entries of this version into it instead of skipping entirely
		return h.ensureMissingWebhookEntries()
	}

	cert, err := h.getCaBundleFromCABundleConfigMap()
	if err != nil {
		return
	}

	vwc := admregv1.ValidatingWebhookConfiguration{}
	vwc.ObjectMeta.Name = h.validatingWebhookConfigName

	for _, webhook := range h.desiredWebhooks(cert) {
		vwc.Webhooks = append(vwc.Webhooks, webhook)
	}

	_, err = h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.TODO(), &vwc, metav1.CreateOptions{})

	return
}

// desiredWebhooks builds every admission entry this version serves: the
// ippool deletion gate, the vmnetcfg duplicate and range guards, the ippool
// spec guard and the vm static ip guard.
func (h *Handler) desiredWebhooks(cert string) []admregv1.ValidatingWebhook {
	return []admregv1.ValidatingWebhook{
		h.buildIPPoolWebhook(cert),
		h.buildVmNetCfgWebhook(cert),
		h.buildIPPoolSpecWebhook(cert),
		h.buildVirtualMachineWebhook(cert),
	}
}

func (h *Handler) buildWebhookClientConfig(webhookName string, path string, cert string) admregv1.WebhookClientConfig {
	clientconfig := admregv1.WebhookClientConfig{}
	serviceref := admregv1.ServiceReference{}
	serviceref.Namespace = h.webhookNamespace
	serviceref.Name = h.webhookName
	serviceref.Path = &path
	port := int32(8080)
	serviceref.Port = &port
	clientconfig.Service = &serviceref
	clientconfig.CABundle = []byte(cert)

	return clientconfig
}

func (h *Handler) buildIPPoolWebhook(cert string) admregv1.ValidatingWebhook {
	webhook := admregv1.ValidatingWebhook{}
	webhook.Name = fmt.Sprintf("%s.%s.svc", h.webhookName, h.webhookNamespace)

	matchLabels := make(map[string]string)
	matchLabels["admission-webhook"] = "enabled"
	nameSpaceSelector := metav1.LabelSelector{}
	nameSpaceSelector.MatchLabels = matchLabels
	webhook.NamespaceSelector = &nameSpaceSelector

	var rules []admregv1.RuleWithOperations
	rule := admregv1.RuleWithOperations{}
	rule.APIGroups = []string{"kubevirtiphelper.k8s.binbash.org"}
	rule.APIVersions = []string{"v1"}
	rule.Operations = []admregv1.OperationType{"DELETE"}
	rule.Resources = []string{"ippools"}
	scope := admregv1.AllScopes
	rule.Scope = &scope
	rules = append(rules, rule)
	webhook.Rules = rules

	sideeffects := admregv1.SideEffectClassNone
	webhook.SideEffects = &sideeffects

	webhook.ClientConfig = h.buildWebhookClientConfig(webhook.Name, "/validate-ippool", cert)

	webhook.AdmissionReviewVersions = []string{"v1"}

	return webhook
}

// buildVmNetCfgWebhook builds the admission entry which rejects a
// VirtualMachineNetworkConfig recording a (vmname, macaddress, networkname)
// tuple that another object of the same namespace already records. the
// helper controllers key the lease ownership on the spec's vmname, so two
// objects with an identical tuple are indistinguishable to them and
// contradictory specs oscillate the allocation on every resync. the entry
// uses failurePolicy Ignore: an admission outage must not block the
// controller's own vmnetcfg writes, and the controller guards remain the
// authoritative defense. it carries no namespace selector so the vmnetcfg
// objects of every namespace are guarded, mirroring the cluster-wide scope
// of the helper CRDs.
func (h *Handler) buildVmNetCfgWebhook(cert string) admregv1.ValidatingWebhook {
	webhook := admregv1.ValidatingWebhook{}
	webhook.Name = h.vmNetCfgWebhookName()

	var rules []admregv1.RuleWithOperations
	rule := admregv1.RuleWithOperations{}
	rule.APIGroups = []string{"kubevirtiphelper.k8s.binbash.org"}
	rule.APIVersions = []string{"v1"}
	rule.Operations = []admregv1.OperationType{"CREATE", "UPDATE"}
	rule.Resources = []string{"virtualmachinenetworkconfigs"}
	scope := admregv1.AllScopes
	rule.Scope = &scope
	rules = append(rules, rule)
	webhook.Rules = rules

	sideeffects := admregv1.SideEffectClassNone
	webhook.SideEffects = &sideeffects

	failurePolicy := admregv1.Ignore
	webhook.FailurePolicy = &failurePolicy

	webhook.ClientConfig = h.buildWebhookClientConfig(webhook.Name, "/validate-vmnetcfg", cert)

	webhook.AdmissionReviewVersions = []string{"v1"}

	return webhook
}

func (h *Handler) vmNetCfgWebhookName() string {
	return fmt.Sprintf("%s-vmnetcfg.%s.svc", h.webhookName, h.webhookNamespace)
}

// buildIPPoolSpecWebhook builds the admission entry which rejects an IPPool
// spec whose ipv4 configuration cannot serve: a subnet which does not parse
// as an ipv4 prefix (the crd schema accepts spellings like 10.0.0.0/33), a
// serverip or allocation range outside the subnet, a pool end before its
// start, or an exclude address outside the allocation range. the helper
// controller rejects such a projection on its own sync too, but the object
// is stored first and its rejection is re-logged on every resync while the
// previously registered configuration keeps serving - denying it at
// admission keeps the invalid spec out of the cluster. the entry uses
// failurePolicy Ignore like the vmnetcfg entry: spec writes are operator
// actions, and the controller's own rejection stays the authoritative
// fence when the webhook is unavailable.
func (h *Handler) buildIPPoolSpecWebhook(cert string) admregv1.ValidatingWebhook {
	webhook := admregv1.ValidatingWebhook{}
	webhook.Name = h.ippoolSpecWebhookName()

	var rules []admregv1.RuleWithOperations
	rule := admregv1.RuleWithOperations{}
	rule.APIGroups = []string{"kubevirtiphelper.k8s.binbash.org"}
	rule.APIVersions = []string{"v1"}
	rule.Operations = []admregv1.OperationType{"CREATE", "UPDATE"}
	rule.Resources = []string{"ippools"}
	scope := admregv1.AllScopes
	rule.Scope = &scope
	rules = append(rules, rule)
	webhook.Rules = rules

	sideeffects := admregv1.SideEffectClassNone
	webhook.SideEffects = &sideeffects

	failurePolicy := admregv1.Ignore
	webhook.FailurePolicy = &failurePolicy

	webhook.ClientConfig = h.buildWebhookClientConfig(webhook.Name, "/validate-ippool-spec", cert)

	webhook.AdmissionReviewVersions = []string{"v1"}

	return webhook
}

func (h *Handler) ippoolSpecWebhookName() string {
	return fmt.Sprintf("%s-ippool-spec.%s.svc", h.webhookName, h.webhookNamespace)
}

// buildVirtualMachineWebhook builds the admission entry which rejects a
// VirtualMachine whose kubevirtiphelper.k8s.binbash.org/static-ip annotation
// requests an address the helper cannot reserve for it: a malformed
// annotation, an interface the vm does not define or whose network is not a
// multus network, a network without an IPPool, an address outside the pool
// range or equal to the subnet broadcast address or an excluded entry, an
// address already recorded for another vm, and an address two interfaces of
// the same vm request at once. the reservation stays check-at-admission and
// claim-at-reconcile: the vm controller claims the address through the
// existing ownership-checked ipam path, so the gate only keeps a request out
// of the cluster which the helper could never serve.
//
// the entry uses failurePolicy Ignore like the vmnetcfg entry: an admission
// outage must not block a vm from being created, and the controller's own
// claim remains the authoritative defense. it carries no namespace selector so
// the vms of every namespace are guarded, mirroring the cluster-wide scope of
// the helper CRDs, and it serves the v1 and v1alpha3 versions, the two
// versions the kubevirt crd serves.
func (h *Handler) buildVirtualMachineWebhook(cert string) admregv1.ValidatingWebhook {
	webhook := admregv1.ValidatingWebhook{}
	webhook.Name = h.virtualMachineWebhookName()

	var rules []admregv1.RuleWithOperations
	rule := admregv1.RuleWithOperations{}
	rule.APIGroups = []string{"kubevirt.io"}
	rule.APIVersions = []string{"v1", "v1alpha3"}
	rule.Operations = []admregv1.OperationType{"CREATE", "UPDATE"}
	rule.Resources = []string{"virtualmachines"}
	scope := admregv1.AllScopes
	rule.Scope = &scope
	rules = append(rules, rule)
	webhook.Rules = rules

	sideeffects := admregv1.SideEffectClassNone
	webhook.SideEffects = &sideeffects

	failurePolicy := admregv1.Ignore
	webhook.FailurePolicy = &failurePolicy

	webhook.ClientConfig = h.buildWebhookClientConfig(webhook.Name, "/validate-vm", cert)

	webhook.AdmissionReviewVersions = []string{"v1"}

	return webhook
}

func (h *Handler) virtualMachineWebhookName() string {
	return fmt.Sprintf("%s-vm.%s.svc", h.webhookName, h.webhookNamespace)
}

// ensureMissingWebhookEntries appends the admission entries this version
// serves to an already existing ValidatingWebhookConfiguration. the
// existing entries are left untouched so a concurrent renewal of the
// serving certificate cannot be overwritten with a stale bundle.
func (h *Handler) ensureMissingWebhookEntries() (err error) {
	vwc, err := h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), h.validatingWebhookConfigName, metav1.GetOptions{})
	if err != nil {
		return
	}

	missing := []string{}
	for _, name := range []string{h.vmNetCfgWebhookName(), h.ippoolSpecWebhookName(), h.virtualMachineWebhookName()} {
		present := false

		for _, webhook := range vwc.Webhooks {
			if webhook.Name == name {
				present = true

				break
			}
		}

		if !present {
			missing = append(missing, name)
		}
	}

	if len(missing) == 0 {
		return
	}

	cert, err := h.getCaBundleFromCABundleConfigMap()
	if err != nil {
		return
	}

	for _, webhook := range h.desiredWebhooks(cert) {
		for _, name := range missing {
			if webhook.Name == name {
				vwc.Webhooks = append(vwc.Webhooks, webhook)
			}
		}
	}

	_, err = h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(context.TODO(), vwc, metav1.UpdateOptions{})
	if err == nil {
		log.Infof("(admission.ensureMissingWebhookEntries) added the admission webhooks %v to the ValidatingWebhookConfiguration %s",
			missing, h.validatingWebhookConfigName)
	}

	return
}

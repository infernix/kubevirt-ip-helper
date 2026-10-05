package admission

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	log "github.com/sirupsen/logrus"
	admregv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// one aggregate budget for the registration one-shots of the webhook
// configuration (the existence check, the ca bundle read and the
// create/update): the boot path must not hang behind a stalled apiserver
// (F08). it is a variable so the test can shrink it.
var webhookRegistrationBudget = 30 * time.Second

func (h *Handler) checkValidatingWebhookConfiguration(ctx context.Context) bool {
	_, err := h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, h.validatingWebhookConfigName, metav1.GetOptions{})
	if err != nil {
		return false
	}

	return true
}

func (h *Handler) AddValidatingWebhookConfiguration() (err error) {
	// the registration one-shots run under one bounded context derived
	// from the era context: a canceled era or an exhausted budget aborts
	// the registration before any configuration is written (F08)
	budgetCtx, cancel := context.WithTimeout(h.ctx, webhookRegistrationBudget)
	defer cancel()

	if h.checkValidatingWebhookConfiguration(budgetCtx) {
		// the configuration already exists (created by an earlier version
		// of this webhook or by an operator): reconcile the admission
		// entries of this version into it instead of skipping entirely
		return h.ensureMissingWebhookEntries(budgetCtx)
	}

	cert, err := h.getCaBundleFromCABundleConfigMap(budgetCtx)
	if err != nil {
		return
	}

	vwc := admregv1.ValidatingWebhookConfiguration{}
	vwc.ObjectMeta.Name = h.validatingWebhookConfigName

	for _, webhook := range h.desiredWebhooks(cert) {
		vwc.Webhooks = append(vwc.Webhooks, webhook)
	}
	_, err = h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(budgetCtx, &vwc, metav1.CreateOptions{})

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
	webhook.Name = h.ipPoolWebhookName()

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

// ipPoolWebhookName is the name of the deletion gate entry. it keeps the bare
// service-qualified spelling the entry had before the other entries gained their
// suffixes, so an existing installation does not orphan it.
func (h *Handler) ipPoolWebhookName() string {
	return fmt.Sprintf("%s.%s.svc", h.webhookName, h.webhookNamespace)
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

// ensureMissingWebhookEntries reconciles the admission entries of an already
// existing ValidatingWebhookConfiguration: it replaces every entry this version
// serves with the freshly built one, appends the entries the configuration
// lacks and prunes the entries a previous installation of this helper left
// behind. reconciling the content, not only the presence, is what lets a
// changed failurePolicy, rule, path or caBundle land on an installation which
// already carries an entry of that name. the configuration is only written when
// an entry actually changed, so a converged installation is not rewritten.
//
// the entry names are namespace-qualified, so moving the helper to another
// namespace renames every entry and leaves the previous ones in place. that is not
// benign: the ippool deletion gate carries the default failurePolicy Fail, so its
// stale entry fails every IPPool delete once the old namespace's service is gone.
// only the entries of this helper are pruned, matched on the serving service
// name; the entries of another product stay untouched.
//
// a concurrent bootstrap (a second replica, or a rolling restart racing the
// previous pod) can write the configuration between this read and the update
// below. the update is idempotent, so a conflict is retried once against the
// object the winner stored; a conflict which survives that retry is accepted
// when a fresh read already carries every desired entry, because the Init path
// panics on any surfaced error and a benign lost race must not fail the boot.
func (h *Handler) ensureMissingWebhookEntries(ctx context.Context) (err error) {
	for attempt := 1; ; attempt++ {
		err = h.reconcileWebhookEntriesOnce(ctx)
		if err == nil || !apierrors.IsConflict(err) || attempt >= 2 {
			break
		}
	}

	if err != nil && apierrors.IsConflict(err) && h.webhookEntriesConverged(ctx) {
		log.Infof("(admission.ensureMissingWebhookEntries) the ValidatingWebhookConfiguration %s was concurrently reconciled by another bootstrap; its entries already match this version",
			h.validatingWebhookConfigName)

		return nil
	}

	return err
}

// reconcileWebhookEntriesOnce performs one read-modify-write of the
// configuration's admission entries.
func (h *Handler) reconcileWebhookEntriesOnce(ctx context.Context) (err error) {
	vwc, err := h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, h.validatingWebhookConfigName, metav1.GetOptions{})
	if err != nil {
		return
	}

	// the ca bundle is read unconditionally: the desired entries carry it, so a
	// rotated bundle must be compared against (and written to) an existing entry
	// too, not only to an appended one.
	cert, err := h.getCaBundleFromCABundleConfigMap(ctx)
	if err != nil {
		return
	}

	desired := h.desiredWebhooks(cert)
	desiredByName := make(map[string]admregv1.ValidatingWebhook, len(desired))
	for _, webhook := range desired {
		desiredByName[webhook.Name] = webhook
	}

	present := make(map[string]struct{}, len(vwc.Webhooks))
	for i := range vwc.Webhooks {
		present[vwc.Webhooks[i].Name] = struct{}{}
	}

	reconciled := make([]admregv1.ValidatingWebhook, 0, len(vwc.Webhooks)+len(desired))
	added := []string{}
	replaced := []string{}
	pruned := []string{}

	for i := range vwc.Webhooks {
		webhook := vwc.Webhooks[i]

		if h.isStaleWebhookEntry(&webhook) {
			pruned = append(pruned, webhook.Name)

			continue
		}

		wanted, ok := desiredByName[webhook.Name]
		if !ok {
			reconciled = append(reconciled, webhook)

			continue
		}

		if !webhookEntriesEqual(webhook, wanted) {
			replaced = append(replaced, webhook.Name)
		}

		reconciled = append(reconciled, wanted)
	}

	for _, webhook := range desired {
		if _, ok := present[webhook.Name]; ok {
			continue
		}

		added = append(added, webhook.Name)
		reconciled = append(reconciled, webhook)
	}

	if len(added) == 0 && len(replaced) == 0 && len(pruned) == 0 {
		return
	}

	vwc.Webhooks = reconciled

	_, err = h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(ctx, vwc, metav1.UpdateOptions{})
	if err == nil {
		log.Infof("(admission.ensureMissingWebhookEntries) reconciled the ValidatingWebhookConfiguration %s: added %v, replaced %v, pruned the entries of a previous namespace %v",
			h.validatingWebhookConfigName, added, replaced, pruned)
	}

	return
}

// webhookEntriesEqual compares two entries on the shape the apiserver persists,
// not on the raw values: the desired entries only carry the fields this version
// owns while a GET returns every API-defaulted field populated (failurePolicy
// Fail, matchPolicy Equivalent, the empty namespace and object selectors and a
// 10s timeout). comparing the raw desired entry against the stored one therefore
// always reported a change and rewrote a converged configuration on every
// startup. normalizing both sides to the API-defaulted shape makes a converged
// installation a genuine no-op, while drift in an owned field (failurePolicy,
// rule, path, caBundle, ...) still differs and is repaired.
func webhookEntriesEqual(a, b admregv1.ValidatingWebhook) bool {
	return reflect.DeepEqual(withWebhookDefaults(a), withWebhookDefaults(b))
}

// withWebhookDefaults returns a copy of the entry with the fields the apiserver
// defaults populated, so a comparison against a stored entry does not treat the
// apiserver's own defaulting as drift. The defaults mirror
// SetDefaults_ValidatingWebhook of admissionregistration/v1.
func withWebhookDefaults(webhook admregv1.ValidatingWebhook) admregv1.ValidatingWebhook {
	if webhook.FailurePolicy == nil {
		policy := admregv1.Fail
		webhook.FailurePolicy = &policy
	}

	if webhook.MatchPolicy == nil {
		policy := admregv1.Equivalent
		webhook.MatchPolicy = &policy
	}

	if webhook.NamespaceSelector == nil {
		webhook.NamespaceSelector = &metav1.LabelSelector{}
	}

	if webhook.ObjectSelector == nil {
		webhook.ObjectSelector = &metav1.LabelSelector{}
	}

	if webhook.TimeoutSeconds == nil {
		timeout := int32(10)
		webhook.TimeoutSeconds = &timeout
	}

	return webhook
}

// webhookEntriesConverged reports whether a fresh read of the configuration
// already carries every desired entry (and none of this helper's stale ones),
// so a conflict which survived the retry was a benign concurrent write of the
// same state.
func (h *Handler) webhookEntriesConverged(ctx context.Context) bool {
	vwc, err := h.clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, h.validatingWebhookConfigName, metav1.GetOptions{})
	if err != nil {
		return false
	}

	cert, err := h.getCaBundleFromCABundleConfigMap(ctx)
	if err != nil {
		return false
	}

	missing := make(map[string]admregv1.ValidatingWebhook)
	for _, webhook := range h.desiredWebhooks(cert) {
		missing[webhook.Name] = webhook
	}

	for i := range vwc.Webhooks {
		if h.isStaleWebhookEntry(&vwc.Webhooks[i]) {
			return false
		}

		wanted, ok := missing[vwc.Webhooks[i].Name]
		if !ok {
			continue
		}

		if !webhookEntriesEqual(vwc.Webhooks[i], wanted) {
			return false
		}

		delete(missing, vwc.Webhooks[i].Name)
	}

	return len(missing) == 0
}

// isStaleWebhookEntry reports whether an entry was left behind by a previous
// installation of this helper: its serving service is this helper's service by
// name but runs in another namespace. an entry of another product is never stale,
// however it is named.
func (h *Handler) isStaleWebhookEntry(webhook *admregv1.ValidatingWebhook) bool {
	service := webhook.ClientConfig.Service
	if service == nil {
		return false
	}

	return service.Name == h.webhookName && service.Namespace != h.webhookNamespace
}

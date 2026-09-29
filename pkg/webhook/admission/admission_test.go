package admission

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	admregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// admissionTestAPI serves the two reads and the one write of the entry
// reconciliation over a real clientset, so the exercised path is the production
// request shapes rather than a fake clientset's bookkeeping.
type admissionTestAPI struct {
	mu       sync.Mutex
	config   *admregv1.ValidatingWebhookConfiguration
	updates  []admregv1.ValidatingWebhookConfiguration
	caBundle string
	server   *httptest.Server
}

func newAdmissionTestAPI(t *testing.T, config *admregv1.ValidatingWebhookConfiguration) *admissionTestAPI {
	t.Helper()

	api := &admissionTestAPI{config: config, caBundle: "live-bundle"}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/validatingwebhookconfigurations/"+config.Name) && r.Method == http.MethodGet:
			writeAdmissionJSON(t, w, api.config)

		case strings.HasSuffix(path, "/validatingwebhookconfigurations/"+config.Name) && r.Method == http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("cannot read the update body: %s", err)
			}

			updated := admregv1.ValidatingWebhookConfiguration{}
			if err := json.Unmarshal(body, &updated); err != nil {
				t.Errorf("cannot decode the update body: %s", err)
			}

			api.config = &updated
			api.updates = append(api.updates, updated)
			writeAdmissionJSON(t, w, updated)

		case strings.HasSuffix(path, "/namespaces/kube-system/configmaps/kube-root-ca.crt") && r.Method == http.MethodGet:
			writeAdmissionJSON(t, w, corev1.ConfigMap{
				TypeMeta:   metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-system"},
				Data:       map[string]string{"ca.crt": api.caBundle},
			})

		default:
			t.Errorf("unexpected request: %s %s", r.Method, path)

			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.server.Close)

	return api
}

func writeAdmissionJSON(t *testing.T, w http.ResponseWriter, value interface{}) {
	t.Helper()

	body, err := json.Marshal(value)
	if err != nil {
		t.Errorf("cannot encode the response: %s", err)
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (api *admissionTestAPI) client(t *testing.T) *kubernetes.Clientset {
	t.Helper()

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: api.server.URL})
	if err != nil {
		t.Fatalf("cannot build the clientset: %s", err)
	}

	return clientset
}

func (api *admissionTestAPI) updateCount() int {
	api.mu.Lock()
	defer api.mu.Unlock()

	return len(api.updates)
}

func (api *admissionTestAPI) entryNames() []string {
	api.mu.Lock()
	defer api.mu.Unlock()

	names := make([]string, 0, len(api.config.Webhooks))
	for _, webhook := range api.config.Webhooks {
		names = append(names, webhook.Name)
	}

	return names
}

func (api *admissionTestAPI) entryByName(name string) *admregv1.ValidatingWebhook {
	api.mu.Lock()
	defer api.mu.Unlock()

	for i := range api.config.Webhooks {
		if api.config.Webhooks[i].Name == name {
			return &api.config.Webhooks[i]
		}
	}

	return nil
}

// admissionEntry builds an entry the way the builders do, with an explicit service
// identity and certificate so the test can tell which entry survived.
func admissionEntry(name, serviceName, serviceNamespace, bundle string) admregv1.ValidatingWebhook {
	path := "/validate"
	port := int32(8080)
	sideEffects := admregv1.SideEffectClassNone
	failurePolicy := admregv1.Fail

	return admregv1.ValidatingWebhook{
		Name:                    name,
		SideEffects:             &sideEffects,
		FailurePolicy:           &failurePolicy,
		AdmissionReviewVersions: []string{"v1"},
		ClientConfig: admregv1.WebhookClientConfig{
			Service:  &admregv1.ServiceReference{Name: serviceName, Namespace: serviceNamespace, Path: &path, Port: &port},
			CABundle: []byte(bundle),
		},
	}
}

func testHandler(t *testing.T, api *admissionTestAPI) *Handler {
	t.Helper()

	return &Handler{
		ctx:                         context.Background(),
		clientset:                   api.client(t),
		webhookName:                 "kubevirt-ip-helper-webhook",
		webhookNamespace:            "dhcp",
		validatingWebhookConfigName: "kubevirt-ip-helper-validator",
	}
}

func currentEntries(t *testing.T) []admregv1.ValidatingWebhook {
	t.Helper()

	handler := &Handler{webhookName: "kubevirt-ip-helper-webhook", webhookNamespace: "dhcp"}

	return handler.desiredWebhooks("live-bundle")
}

func staleEntries() []admregv1.ValidatingWebhook {
	return []admregv1.ValidatingWebhook{
		admissionEntry("kubevirt-ip-helper-webhook.kubevirt-ip-helper.svc", "kubevirt-ip-helper-webhook", "kubevirt-ip-helper", "stale-bundle"),
		admissionEntry("kubevirt-ip-helper-webhook-vmnetcfg.kubevirt-ip-helper.svc", "kubevirt-ip-helper-webhook", "kubevirt-ip-helper", "stale-bundle"),
		admissionEntry("kubevirt-ip-helper-webhook-ippool-spec.kubevirt-ip-helper.svc", "kubevirt-ip-helper-webhook", "kubevirt-ip-helper", "stale-bundle"),
		admissionEntry("kubevirt-ip-helper-webhook-vm.kubevirt-ip-helper.svc", "kubevirt-ip-helper-webhook", "kubevirt-ip-helper", "stale-bundle"),
	}
}

func testConfig(t *testing.T, webhooks []admregv1.ValidatingWebhook) *admregv1.ValidatingWebhookConfiguration {
	t.Helper()

	return &admregv1.ValidatingWebhookConfiguration{
		TypeMeta:   metav1.TypeMeta{Kind: "ValidatingWebhookConfiguration", APIVersion: "admissionregistration.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "kubevirt-ip-helper-validator", ResourceVersion: "1"},
		Webhooks:   webhooks,
	}
}

// TestEnsureMissingWebhookEntriesPrunesPreviousNamespace proves that a namespace
// move cannot leave the previous install's entries behind: the stale ippool deletion
// gate carries the default failurePolicy Fail and would block every IPPool delete
// once the old namespace's service is gone. Entries of another product, and the
// surviving entries' certificates, must stay untouched.
func TestEnsureMissingWebhookEntriesPrunesPreviousNamespace(t *testing.T) {
	foreign := admissionEntry("policy-engine.kyverno.svc", "policy-engine", "kyverno", "foreign-bundle")

	seeded := append(staleEntries(), foreign)
	seeded = append(seeded, currentEntries(t)...)

	api := newAdmissionTestAPI(t, testConfig(t, seeded))
	handler := testHandler(t, api)

	if err := handler.ensureMissingWebhookEntries(); err != nil {
		t.Fatalf("reconcile returned an error: %s", err)
	}

	if updates := api.updateCount(); updates != 1 {
		t.Fatalf("updates = %d, want 1 (the prune must be persisted)", updates)
	}

	for _, stale := range staleEntries() {
		if api.entryByName(stale.Name) != nil {
			t.Errorf("stale entry %s of the previous namespace survived", stale.Name)
		}
	}

	if kept := api.entryByName(foreign.Name); kept == nil {
		t.Errorf("the entry of another product %s was pruned", foreign.Name)
	} else if !reflect.DeepEqual(kept.ClientConfig, foreign.ClientConfig) {
		t.Errorf("foreign entry client config changed: %+v", kept.ClientConfig)
	}

	for _, current := range currentEntries(t) {
		kept := api.entryByName(current.Name)
		if kept == nil {
			t.Errorf("current entry %s disappeared", current.Name)

			continue
		}

		if got := string(kept.ClientConfig.CABundle); got != "live-bundle" {
			t.Errorf("current entry %s bundle = %q, want the live bundle untouched", current.Name, got)
		}
	}

	if names := api.entryNames(); len(names) != len(currentEntries(t))+1 {
		t.Errorf("entries = %v, want the four current entries plus the foreign one", names)
	}
}

// TestEnsureMissingWebhookEntriesAppendsAndPrunes proves the append path still
// works in the same reconcile: a configuration which only carries the previous
// namespace's entries gets this version's entries with the current CA bundle.
func TestEnsureMissingWebhookEntriesAppendsAndPrunes(t *testing.T) {
	api := newAdmissionTestAPI(t, testConfig(t, staleEntries()))
	api.caBundle = "fresh-bundle"
	handler := testHandler(t, api)

	if err := handler.ensureMissingWebhookEntries(); err != nil {
		t.Fatalf("reconcile returned an error: %s", err)
	}

	if updates := api.updateCount(); updates != 1 {
		t.Fatalf("updates = %d, want 1", updates)
	}

	if names := api.entryNames(); len(names) != 4 {
		t.Fatalf("entries = %v, want the four current entries", names)
	}

	for _, current := range currentEntries(t) {
		kept := api.entryByName(current.Name)
		if kept == nil {
			t.Errorf("missing entry %s", current.Name)

			continue
		}

		if got := string(kept.ClientConfig.CABundle); got != "fresh-bundle" {
			t.Errorf("appended entry %s bundle = %q, want the current CA bundle", current.Name, got)
		}
	}
}

// TestEnsureMissingWebhookEntriesIsIdempotent proves a converged configuration is
// not rewritten, so a concurrent certificate renewal keeps its bundle.
func TestEnsureMissingWebhookEntriesIsIdempotent(t *testing.T) {
	api := newAdmissionTestAPI(t, testConfig(t, currentEntries(t)))
	handler := testHandler(t, api)

	if err := handler.ensureMissingWebhookEntries(); err != nil {
		t.Fatalf("reconcile returned an error: %s", err)
	}

	if updates := api.updateCount(); updates != 0 {
		t.Errorf("updates = %d, want 0 for a converged configuration", updates)
	}
}

// TestEnsureMissingWebhookEntriesKeepsForeignNamespaceEntries proves the prune is
// matched on this helper's service name: another product serving from another
// namespace is never touched, even when its name is namespace-qualified too.
func TestEnsureMissingWebhookEntriesKeepsForeignNamespaceEntries(t *testing.T) {
	foreign := admissionEntry("other-webhook.other-namespace.svc", "other-webhook", "other-namespace", "foreign-bundle")
	api := newAdmissionTestAPI(t, testConfig(t, append(currentEntries(t), foreign)))
	handler := testHandler(t, api)

	if err := handler.ensureMissingWebhookEntries(); err != nil {
		t.Fatalf("reconcile returned an error: %s", err)
	}

	if kept := api.entryByName(foreign.Name); kept == nil {
		t.Fatalf("foreign entry %s was pruned", foreign.Name)
	}

	if updates := api.updateCount(); updates != 0 {
		t.Errorf("updates = %d, want 0 when only foreign entries coexist", updates)
	}
}

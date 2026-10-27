package correlationlog

import (
	"context"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const orgA = "org-a"

var portGVK = schema.GroupVersionKind{Group: "networking.openstack.m.crossplane.io", Version: "v1alpha1", Kind: "PortV2"}
var instanceGVK = schema.GroupVersionKind{Group: "compute.openstack.m.crossplane.io", Version: "v1alpha1", Kind: "InstanceV2"}

type entry struct {
	msg string
	kvs []any
}

// recorder is a logging.Logger that keeps every line with the values bound
// through WithValues prepended, as a real structured logger would emit them.
type recorder struct {
	bound   []any
	entries *[]entry
}

func newRecorder() *recorder { return &recorder{entries: &[]entry{}} }

func (r *recorder) Info(msg string, kvs ...any) {
	*r.entries = append(*r.entries, entry{msg: msg, kvs: append(append([]any{}, r.bound...), kvs...)})
}
func (r *recorder) Debug(msg string, kvs ...any) { r.Info(msg, kvs...) }
func (r *recorder) WithValues(kvs ...any) logging.Logger {
	return &recorder{bound: append(append([]any{}, r.bound...), kvs...), entries: r.entries}
}

func (r *recorder) last(t *testing.T) map[string]any {
	t.Helper()
	if len(*r.entries) == 0 {
		t.Fatal("no log line recorded")
	}
	e := (*r.entries)[len(*r.entries)-1]
	m := map[string]any{}
	for i := 0; i+1 < len(e.kvs); i += 2 {
		m[e.kvs[i].(string)] = e.kvs[i+1]
	}
	return m
}

func resetStore() {
	store.Range(func(k, _ any) bool { store.Delete(k); return true })
}

// typedManaged keeps its TypeMeta like the generated upjet types do;
// fake.Managed always reports an empty kind.
type typedManaged struct {
	fake.Managed
	metav1.TypeMeta
}

func (m *typedManaged) GetObjectKind() schema.ObjectKind { return &m.TypeMeta }

func managedResource(gvk schema.GroupVersionKind, ns, name, annotation string) *typedManaged {
	mg := &typedManaged{}
	mg.SetGroupVersionKind(gvk)
	mg.SetNamespace(ns)
	mg.SetName(name)
	mg.SetUID(uidOf(gvk, ns, name))
	if annotation != "" {
		mg.SetAnnotations(map[string]string{CorrelationAnnotationKey: annotation})
	}
	return mg
}

func uidOf(gvk schema.GroupVersionKind, ns, name string) types.UID {
	return types.UID(gvk.Kind + "/" + ns + "/" + name)
}

func reconcileLogger(base logging.Logger, gvk schema.GroupVersionKind, ns, name string) logging.Logger {
	l := NewCorrelatingLogger(base).WithValues("controller", managed.ControllerName(gvk.String()))
	return l.WithValues("request", reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
}

func initialize(t *testing.T, mg *typedManaged) {
	t.Helper()
	if err := NewInitializer(schema.GroupKind{}).Initialize(context.Background(), mg); err != nil {
		t.Fatalf("Initialize returned an error: %v", err)
	}
}

func TestResourceWithoutAnnotationDoesNotInheritAnotherResourcesFields(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`))
	reconcileLogger(rec, portGVK, orgA, "port-a").Debug("observed")

	initialize(t, managedResource(portGVK, "org-b", "port-b", ""))
	reconcileLogger(rec, portGVK, "org-b", "port-b").Debug("observed")

	if v, ok := rec.last(t)["owner_org_id"]; ok {
		t.Fatalf("port-b logged owner_org_id=%v from port-a", v)
	}
}

func TestReconcileLinesBeforeInitializeCarryOnlyTheirOwnResourcesFields(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`))
	initialize(t, managedResource(portGVK, "org-b", "port-b", `{"owner_org_id":"org-b"}`))

	reconcileLogger(rec, portGVK, orgA, "port-a").Debug("Reconciling")

	if got := rec.last(t)["owner_org_id"]; got != orgA {
		t.Fatalf("owner_org_id = %v, want org-a", got)
	}
}

func TestSameNameInAnotherKindIsADifferentResource(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "vm-1", `{"resource_name":"vm-1-port"}`))
	initialize(t, managedResource(instanceGVK, orgA, "vm-1", `{"resource_name":"vm-1"}`))

	reconcileLogger(rec, portGVK, orgA, "vm-1").Debug("observed")

	if got := rec.last(t)["resource_name"]; got != "vm-1-port" {
		t.Fatalf("resource_name = %v, want vm-1-port", got)
	}
}

func TestAsyncTrackerLinesCarryTheResourcesFields(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`))

	tracker := NewCorrelatingLogger(rec).WithValues(
		"trackerUID", uidOf(portGVK, orgA, "port-a"), "resourceName", "port-a", "resourceNamespace", orgA, "gvk", portGVK.String())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tracker.Info("Async update callback failed", "error", "conflict")
	}()
	<-done

	if got := rec.last(t)["owner_org_id"]; got != orgA {
		t.Fatalf("owner_org_id = %v, want org-a", got)
	}
}

// The typed client's decoder clears TypeMeta after an API write (the reference
// resolver's patch, say), so upjet can bind an empty gvk to the resource.
func TestTrackerBoundToAnEmptyGVKFindsTheResourceByUID(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`))

	NewCorrelatingLogger(rec).WithValues(
		"trackerUID", uidOf(portGVK, orgA, "port-a"), "resourceName", "port-a", "resourceNamespace", orgA,
		"gvk", schema.GroupVersionKind{}.String()).Info("Async create callback failed")

	if got := rec.last(t)["owner_org_id"]; got != orgA {
		t.Fatalf("owner_org_id = %v, want org-a", got)
	}
}

func TestEventHandlerRequeueLinesCarryTheResourcesFields(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`))

	NewCorrelatingLogger(rec).WithValues("gvk", portGVK).
		WithValues("name", types.NamespacedName{Namespace: orgA, Name: "port-a"}).
		Info("Failure limit has been exceeded.")

	if got := rec.last(t)["owner_org_id"]; got != orgA {
		t.Fatalf("owner_org_id = %v, want org-a", got)
	}
}

// fakeClock replaces the store's clock and resets its sweep rate limit for the
// duration of the test.
func fakeClock(t *testing.T) *time.Time {
	t.Helper()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prevNow, prevSweep := now, lastSweep.Load()
	now = func() time.Time { return clock }
	lastSweep.Store(0)
	t.Cleanup(func() { now = prevNow; lastSweep.Store(prevSweep) })
	return &clock
}

func deleting(mg *typedManaged, at time.Time) *typedManaged {
	ts := metav1.NewTime(at)
	mg.SetDeletionTimestamp(&ts)
	return mg
}

func TestDeletedResourceKeepsItsFieldsWhileBeingDeletedThenIsEvicted(t *testing.T) {
	t.Cleanup(resetStore)
	clock := fakeClock(t)
	rec := newRecorder()
	uid := uidOf(portGVK, orgA, "port-a")

	initialize(t, deleting(managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`), *clock))
	*clock = clock.Add(evictDeletedAfter - time.Minute)
	initialize(t, managedResource(instanceGVK, orgA, "vm-a", `{"owner_org_id":"org-b"}`))

	reconcileLogger(rec, portGVK, orgA, "port-a").WithValues("uid", uid).Debug("deleting")
	if got := rec.last(t)["owner_org_id"]; got != orgA {
		t.Fatalf("owner_org_id = %v during deletion, want %s", got, orgA)
	}

	*clock = clock.Add(2 * time.Minute)
	initialize(t, managedResource(instanceGVK, orgA, "vm-a", `{"owner_org_id":"org-b"}`))

	reconcileLogger(rec, portGVK, orgA, "port-a").Debug("after deletion")
	if v, ok := rec.last(t)["owner_org_id"]; ok {
		t.Fatalf("owner_org_id=%v still stored by name after the grace period", v)
	}
	if _, ok := store.Load("uid|" + string(uid)); ok {
		t.Fatal("entry still stored by UID after the grace period")
	}
}

func TestResourceNotBeingDeletedIsNeverEvicted(t *testing.T) {
	t.Cleanup(resetStore)
	clock := fakeClock(t)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`))
	*clock = clock.Add(10 * evictDeletedAfter)
	initialize(t, managedResource(instanceGVK, orgA, "vm-a", `{"owner_org_id":"org-b"}`))

	reconcileLogger(rec, portGVK, orgA, "port-a").Debug("observed")
	if got := rec.last(t)["owner_org_id"]; got != orgA {
		t.Fatalf("owner_org_id = %v, want %s", got, orgA)
	}
}

func TestRecreatedResourceKeepsItsNameEntryWhenTheDeletedOneIsEvicted(t *testing.T) {
	t.Cleanup(resetStore)
	clock := fakeClock(t)
	rec := newRecorder()

	initialize(t, deleting(managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-old"}`), *clock))
	recreated := managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-new"}`)
	recreated.SetUID("recreated")
	initialize(t, recreated)

	*clock = clock.Add(evictDeletedAfter + time.Minute)
	initialize(t, managedResource(instanceGVK, orgA, "vm-a", `{"owner_org_id":"org-b"}`))

	reconcileLogger(rec, portGVK, orgA, "port-a").Debug("observed")
	if got := rec.last(t)["owner_org_id"]; got != "org-new" {
		t.Fatalf("owner_org_id = %v, want org-new", got)
	}
}

func TestMalformedAnnotationDoesNotFailTheReconcileAndClearsOldFields(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`))
	initialize(t, managedResource(portGVK, orgA, "port-a", `{not json`))

	reconcileLogger(rec, portGVK, orgA, "port-a").Debug("observed")
	if v, ok := rec.last(t)["owner_org_id"]; ok {
		t.Fatalf("stale owner_org_id=%v kept after a malformed annotation", v)
	}

	reconcileLogger(rec, portGVK, orgA, "port-a").WithValues("uid", uidOf(portGVK, orgA, "port-a")).Debug("observed")
	if v, ok := rec.last(t)["owner_org_id"]; ok {
		t.Fatalf("stale owner_org_id=%v kept under the UID after a malformed annotation", v)
	}
}

func TestMalformedAnnotationIsReportedAsAnInputFault(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()
	prev := warnLogger
	SetLogger(rec)
	t.Cleanup(func() { SetLogger(prev) })

	initialize(t, managedResource(portGVK, orgA, "port-a", `{not json`))

	got := rec.last(t)
	want := map[string]any{"faultDomain": "input", "kind": portGVK.GroupKind().String(), "name": "port-a", "namespace": orgA}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v in %v", k, got[k], v, got)
		}
	}
}

func TestInitializerFallsBackToTheConfiguredKindWhenTypeMetaIsEmpty(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	mg := managedResource(schema.GroupVersionKind{}, orgA, "port-a", `{"owner_org_id":"org-a"}`)
	if err := NewInitializer(portGVK.GroupKind()).Initialize(context.Background(), mg); err != nil {
		t.Fatal(err)
	}
	reconcileLogger(rec, portGVK, orgA, "port-a").Debug("observed")

	if got := rec.last(t)["owner_org_id"]; got != orgA {
		t.Fatalf("owner_org_id = %v, want org-a", got)
	}
}

func TestAnnotationKeysThatCollideWithFrameworkKeysAreRemapped(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"gvk":"x","request":"y","name":"z","ts":"t","msg":"m"}`))
	reconcileLogger(rec, portGVK, orgA, "port-a").Debug("observed")

	got := rec.last(t)
	for _, k := range []string{"correlation_id_gvk", "correlation_id_request", "correlation_id_name", "correlation_id_ts", "correlation_id_msg"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing remapped key %s in %v", k, got)
		}
	}
}

func TestCallTimeNameDoesNotRebindABoundLogger(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`))
	initialize(t, managedResource(portGVK, orgA, "sg-1", `{"owner_org_id":"org-other"}`))

	reconcileLogger(rec, portGVK, orgA, "port-a").Debug("referenced security group", "name", "sg-1")

	if got := rec.last(t)["owner_org_id"]; got != orgA {
		t.Fatalf("owner_org_id = %v, want org-a", got)
	}
}

func TestEmptyGVKValueDoesNotClearTheBoundKind(t *testing.T) {
	t.Cleanup(resetStore)
	rec := newRecorder()

	initialize(t, managedResource(portGVK, orgA, "port-a", `{"owner_org_id":"org-a"}`))

	reconcileLogger(rec, portGVK, orgA, "port-a").WithValues("gvk", "").Debug("observed")

	if got := rec.last(t)["owner_org_id"]; got != orgA {
		t.Fatalf("owner_org_id = %v, want org-a", got)
	}
}

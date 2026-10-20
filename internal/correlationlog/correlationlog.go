package correlationlog

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	xpresource "github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// CorrelationAnnotationKey is the annotation used to carry correlation
	// metadata as a JSON object for structured log enrichment.
	CorrelationAnnotationKey = "nby.one/correlation_id"

	reservedFieldPrefix = "correlation_id_"
)

// reservedLogFieldKeys are keys the reconciler, upjet and controller-runtime
// already emit. Annotation keys with these names are remapped so they never
// shadow or duplicate them.
var reservedLogFieldKeys = map[string]struct{}{
	"controller":        {},
	"controllerKind":    {},
	"name":              {},
	"namespace":         {},
	"reconcileID":       {},
	"request":           {},
	"uid":               {},
	"gvk":               {},
	"trackerUID":        {},
	"resourceName":      {},
	"resourceNamespace": {},
	"error":             {},
	"external-name":     {},
	"version":           {},
}

// store maps a managed resource (identity.key) to the key/value pairs parsed
// from its annotation. It is keyed by resource rather than by goroutine so that
// a line can never carry another resource's fields, and so that lines logged
// outside the reconcile goroutine (async upjet callbacks) are enriched too.
var store sync.Map

var warnLogger logging.Logger = logging.NewNopLogger()

// SetLogger sets the logger used to report malformed correlation annotations.
func SetLogger(l logging.Logger) {
	warnLogger = l
}

// identity is the managed resource a logger is bound to, learnt from the keys
// the managed reconciler and upjet already attach with WithValues.
type identity struct {
	groupKind string
	namespace string
	name      string
}

func (id identity) key() (string, bool) {
	if id.groupKind == "" || id.name == "" {
		return "", false
	}
	return id.groupKind + "|" + id.namespace + "|" + id.name, true
}

// with returns id updated from kvs. With fillOnly, only fields that are still
// empty are set, so call-time values (a referenced object's "name", say) never
// rebind a logger that WithValues already bound to its resource.
func (id identity) with(kvs []any, fillOnly bool) identity {
	for i := 0; i+1 < len(kvs); i += 2 {
		if k, ok := kvs[i].(string); ok {
			id = id.merge(identityFields(k, kvs[i+1]), fillOnly)
		}
	}
	return id
}

func (id identity) merge(f identity, fillOnly bool) identity {
	set := func(field *string, v string) {
		if v != "" && (!fillOnly || *field == "") {
			*field = v
		}
	}
	set(&id.groupKind, f.groupKind)
	set(&id.namespace, f.namespace)
	set(&id.name, f.name)
	return id
}

// identityFields returns the parts of a resource's identity carried by one
// key/value pair.
func identityFields(k string, v any) identity {
	switch v := v.(type) {
	case reconcile.Request:
		if k == "request" {
			return identity{namespace: v.Namespace, name: v.Name}
		}
	case types.NamespacedName:
		if k == "request" {
			return identity{namespace: v.Namespace, name: v.Name}
		}
	case schema.GroupVersionKind:
		if k == "gvk" {
			return identity{groupKind: groupKindKey(v.GroupKind())}
		}
	case string:
		return identityFromString(k, v)
	}
	return identity{}
}

func identityFromString(k, v string) identity {
	switch k {
	case "controller":
		// managed.ControllerName: "managed/" + lower(gvk.String()).
		if gvk, ok := strings.CutPrefix(v, "managed/"); ok {
			return identity{groupKind: groupKindFromGVKString(gvk)}
		}
	case "gvk":
		return identity{groupKind: groupKindFromGVKString(v)}
	case "name", "resourceName":
		return identity{name: v}
	case "namespace", "resourceNamespace":
		return identity{namespace: v}
	}
	return identity{}
}

func groupKindKey(gk schema.GroupKind) string {
	if gk.Kind == "" {
		return ""
	}
	return strings.ToLower(gk.Group + "/" + gk.Kind)
}

// groupKindFromGVKString parses schema.GroupVersionKind.String() output
// ("group/version, Kind=Kind"), in any letter case.
func groupKindFromGVKString(s string) string {
	s = strings.ToLower(s)
	gv, kind, ok := strings.Cut(s, ", kind=")
	if !ok || kind == "" {
		return ""
	}
	group, _, _ := strings.Cut(gv, "/")
	return group + "/" + kind
}

// NewCorrelatingLogger wraps base so that every Info and Debug call carries the
// correlation fields of the managed resource the logger is bound to. Wire it
// around the top-level logger in main.go.
func NewCorrelatingLogger(base logging.Logger) *CorrelatingLogger {
	return &CorrelatingLogger{base: base}
}

// CorrelatingLogger wraps a logging.Logger and adds the correlation fields of
// the managed resource identified by its bound values.
type CorrelatingLogger struct {
	base logging.Logger
	id   identity
}

// Info implements logging.Logger.
func (l *CorrelatingLogger) Info(msg string, kvs ...any) {
	l.base.Info(msg, l.enrich(kvs)...)
}

// Debug implements logging.Logger.
func (l *CorrelatingLogger) Debug(msg string, kvs ...any) {
	l.base.Debug(msg, l.enrich(kvs)...)
}

// WithValues implements logging.Logger.
func (l *CorrelatingLogger) WithValues(kvs ...any) logging.Logger {
	return &CorrelatingLogger{base: l.base.WithValues(kvs...), id: l.id.with(kvs, false)}
}

func (l *CorrelatingLogger) enrich(kvs []any) []any {
	key, ok := l.id.with(kvs, true).key()
	if !ok {
		return kvs
	}
	v, ok := store.Load(key)
	if !ok {
		return kvs
	}
	corr := v.([]any)
	out := make([]any, 0, len(corr)+len(kvs))
	return append(append(out, corr...), kvs...)
}

// NewInitializer returns a managed.Initializer that, on each reconcile, records
// the managed resource's correlation fields for every CorrelatingLogger bound
// to that resource. fallback names the resource's kind when the object's
// TypeMeta is empty.
func NewInitializer(fallback schema.GroupKind) managed.Initializer {
	return &correlationInitializer{fallback: fallback}
}

type correlationInitializer struct {
	fallback schema.GroupKind
}

// Initialize never returns an error: correlation is diagnostic metadata and must
// not block provisioning.
func (c correlationInitializer) Initialize(_ context.Context, mg xpresource.Managed) error {
	gk := mg.GetObjectKind().GroupVersionKind().GroupKind()
	if gk.Kind == "" {
		gk = c.fallback
	}
	key, ok := identity{groupKind: groupKindKey(gk), namespace: mg.GetNamespace(), name: mg.GetName()}.key()
	if !ok {
		return nil
	}
	values, err := extractCorrelationValues(mg.GetAnnotations())
	if err != nil {
		store.Delete(key)
		warnLogger.Info("ignoring malformed correlation annotation",
			"faultDomain", "input",
			"annotation", CorrelationAnnotationKey,
			"name", mg.GetName(),
			"namespace", mg.GetNamespace(),
			"error", err.Error())
		return nil
	}
	if len(values) == 0 {
		store.Delete(key)
		return nil
	}
	keys := sortedKeys(values)
	kvs := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		kvs = append(kvs, logFieldKey(k), values[k])
	}
	store.Store(key, kvs)
	return nil
}

func extractCorrelationValues(annotations map[string]string) (map[string]string, error) {
	if len(annotations) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(annotations[CorrelationAnnotationKey])
	if raw == "" {
		return nil, nil
	}
	parsed := make(map[string]any)
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", CorrelationAnnotationKey, err)
	}
	values := make(map[string]string, len(parsed))
	for k, v := range parsed {
		if strings.TrimSpace(k) == "" {
			continue
		}
		values[k] = stringify(v)
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values, nil
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

func logFieldKey(key string) string {
	if _, reserved := reservedLogFieldKeys[key]; reserved {
		return reservedFieldPrefix + key
	}
	return key
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

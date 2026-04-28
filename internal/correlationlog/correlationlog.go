package correlationlog

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	xpresource "github.com/crossplane/crossplane-runtime/v2/pkg/resource"
)

const (
	// CorrelationAnnotationKey is the annotation used to carry correlation
	// metadata as a JSON object for structured log enrichment.
	CorrelationAnnotationKey = "nby.one/correlation_id"

	reservedFieldPrefix = "correlation_id_"
)

var reservedLogFieldKeys = map[string]struct{}{
	"controller":     {},
	"controllerKind": {},
	"name":           {},
	"namespace":      {},
	"reconcileID":    {},
}

// sharedFields is the single goroutine-local store used by all
// CorrelatingLogger instances and all correlationInitializer instances in this
// process. Keyed by goroutine ID (uint64), values are []any kv slices.
var sharedFields sync.Map

// NewCorrelatingLogger wraps base so that every Info and Debug call
// automatically prepends the per-goroutine correlation fields written by the
// correlationInitializer at the start of each reconcile. Wire it around the
// top-level logger in main.go.
func NewCorrelatingLogger(base logging.Logger) *CorrelatingLogger {
	return &CorrelatingLogger{base: base}
}

// NewInitializer returns a managed.Initializer that, on each reconcile,
// parses the correlation annotation of the managed resource and stores the
// fields in the shared goroutine-local store so that every subsequent log call
// on any CorrelatingLogger picks them up.
func NewInitializer() managed.Initializer {
	return &correlationInitializer{}
}

// CorrelatingLogger wraps a logging.Logger and prepends per-goroutine
// correlation fields on every Info and Debug call.
type CorrelatingLogger struct {
	base logging.Logger
}

// Info implements logging.Logger.
func (l *CorrelatingLogger) Info(msg string, kvs ...any) {
	l.base.Info(msg, enrich(kvs)...)
}

// Debug implements logging.Logger.
func (l *CorrelatingLogger) Debug(msg string, kvs ...any) {
	l.base.Debug(msg, enrich(kvs)...)
}

// WithValues implements logging.Logger.
func (l *CorrelatingLogger) WithValues(kvs ...any) logging.Logger {
	return &CorrelatingLogger{base: l.base.WithValues(kvs...)}
}

func enrich(kvs []any) []any {
	v, ok := sharedFields.Load(currentGoroutineID())
	if !ok {
		return kvs
	}
	corr := v.([]any)
	out := make([]any, 0, len(corr)+len(kvs))
	return append(append(out, corr...), kvs...)
}

// correlationInitializer implements managed.Initializer.
type correlationInitializer struct{}

func (correlationInitializer) Initialize(_ context.Context, mg xpresource.Managed) error {
	values, err := extractCorrelationValues(mg.GetAnnotations())
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	keys := sortedKeys(values)
	kvs := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		kvs = append(kvs, logFieldKey(k), values[k])
	}
	sharedFields.Store(currentGoroutineID(), kvs)
	return nil
}

// currentGoroutineID returns the ID of the calling goroutine by parsing the
// first line of a runtime stack trace ("goroutine NNN [...]"). This format
// has been stable across all Go versions and is widely used in practice.
func currentGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[1], 10, 64)
	return id
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

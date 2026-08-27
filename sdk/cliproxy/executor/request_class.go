package executor

import (
	"context"
	"strings"
)

const (
	// RequestClassNormal is a regular user-facing model call.
	RequestClassNormal = "normal"
	// RequestClassSessionName is a new-conversation title request.
	RequestClassSessionName = "session_name"
	// RequestClassCompaction is a context compression request.
	RequestClassCompaction = "compaction"
	// RequestClassInternal is a background/system model call.
	RequestClassInternal = "internal"

	// RequestClassMetadataKey stores the classified request kind on Options.Metadata.
	RequestClassMetadataKey = "llm_request_class"
)

type requestClassContextKey struct{}

// NormalizeRequestClass maps unknown values to normal.
func NormalizeRequestClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case RequestClassSessionName:
		return RequestClassSessionName
	case RequestClassCompaction:
		return RequestClassCompaction
	case RequestClassInternal:
		return RequestClassInternal
	default:
		return RequestClassNormal
	}
}

// RequestClassUsesAuxPool reports whether this kind should use the auxiliary
// 5-account bucket and skip credential rotation.
func RequestClassUsesAuxPool(class string) bool {
	switch NormalizeRequestClass(class) {
	case RequestClassSessionName, RequestClassCompaction, RequestClassInternal:
		return true
	default:
		return false
	}
}

// WithRequestClass stores the classified request kind on ctx.
func WithRequestClass(ctx context.Context, class string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requestClassContextKey{}, NormalizeRequestClass(class))
}

// RequestClassFromContext returns the classified request kind, defaulting to normal.
func RequestClassFromContext(ctx context.Context) string {
	if ctx == nil {
		return RequestClassNormal
	}
	if class, ok := ctx.Value(requestClassContextKey{}).(string); ok {
		return NormalizeRequestClass(class)
	}
	return RequestClassNormal
}

// RequestClassFromMetadata reads the classified request kind from execution metadata.
func RequestClassFromMetadata(meta map[string]any) string {
	if meta == nil {
		return RequestClassNormal
	}
	raw, ok := meta[RequestClassMetadataKey]
	if !ok || raw == nil {
		return RequestClassNormal
	}
	switch value := raw.(type) {
	case string:
		return NormalizeRequestClass(value)
	default:
		return RequestClassNormal
	}
}

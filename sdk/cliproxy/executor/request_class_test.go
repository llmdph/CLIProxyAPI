package executor

import (
	"context"
	"testing"
)

func TestNormalizeRequestClass(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":             RequestClassNormal,
		"NORMAL":       RequestClassNormal,
		"session_name": RequestClassSessionName,
		"compaction":   RequestClassCompaction,
		"internal":     RequestClassInternal,
		"other":        RequestClassNormal,
	}
	for in, want := range cases {
		if got := NormalizeRequestClass(in); got != want {
			t.Fatalf("NormalizeRequestClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestClassUsesAuxPool(t *testing.T) {
	t.Parallel()
	if RequestClassUsesAuxPool(RequestClassNormal) {
		t.Fatal("normal should stay on the main pool")
	}
	for _, class := range []string{RequestClassSessionName, RequestClassCompaction, RequestClassInternal} {
		if !RequestClassUsesAuxPool(class) {
			t.Fatalf("%s should use the auxiliary pool", class)
		}
	}
}

func TestRequestClassContextAndMetadata(t *testing.T) {
	t.Parallel()
	ctx := WithRequestClass(context.Background(), RequestClassCompaction)
	if got := RequestClassFromContext(ctx); got != RequestClassCompaction {
		t.Fatalf("ctx class = %q", got)
	}
	meta := map[string]any{RequestClassMetadataKey: RequestClassInternal}
	if got := RequestClassFromMetadata(meta); got != RequestClassInternal {
		t.Fatalf("meta class = %q", got)
	}
	if got := RequestClassFromMetadata(nil); got != RequestClassNormal {
		t.Fatalf("nil meta class = %q", got)
	}
}

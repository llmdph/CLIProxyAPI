package warprotate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNodeLabel(t *testing.T) {
	t.Parallel()
	if got := NodeLabel("warp-lb-3", "w3"); got != "lb3" {
		t.Fatalf("NodeLabel warp-lb-3 = %q", got)
	}
	if got := NodeLabel("", "w10"); got != "lb10" {
		t.Fatalf("NodeLabel w10 = %q", got)
	}
}

func TestLooksLikeIP(t *testing.T) {
	t.Parallel()
	if !LooksLikeIP("104.28.254.46") {
		t.Fatal("expected ipv4")
	}
	if LooksLikeIP(`{"error": "Too Many Requests"}`) {
		t.Fatal("json error must not count as ip")
	}
	if LooksLikeIP("warp-lb:1080") {
		t.Fatal("host:port must not count as ip")
	}
}

func TestLookupURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/lookup" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("client") != "172.19.0.9:55656" {
			t.Fatalf("client = %s", r.URL.Query().Get("client"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"client":   "172.19.0.9:55656",
			"server":   "w7",
			"instance": "warp-lb-7",
			"label":    "lb7",
			"exit_ip":  "104.28.254.46",
		})
	}))
	t.Cleanup(srv.Close)
	got := LookupURL(context.Background(), srv.URL, "172.19.0.9:55656")
	if got == nil || !got.OK || got.Label != "lb7" || got.ExitIP != "104.28.254.46" {
		t.Fatalf("lookup = %+v", got)
	}
}

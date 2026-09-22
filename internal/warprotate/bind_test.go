package warprotate

import "testing"

func TestNodeLabelFromHost(t *testing.T) {
	t.Parallel()
	if got := NodeLabelFromHost("warp-lb-3:1080"); got != "lb3" {
		t.Fatalf("dedicated host = %q", got)
	}
	if got := NodeLabelFromHost("warp-lb:1080"); got != "backup" {
		t.Fatalf("shared host = %q", got)
	}
	if got := NodeLabelFromHost("warp-lb-10"); got != "lb10" {
		t.Fatalf("lb10 = %q", got)
	}
}

package management

import "testing"

func TestParseThinkFromResponseDetectsReasoning(t *testing.T) {
	body := `{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"sky is blue because of scattering"}]}]}`
	ev := parseThinkFromResponse(body)
	if !ev.ok() {
		t.Fatalf("expected think ok, got has=%v len=%d", ev.HasThink, ev.Length)
	}
}

func TestParseThinkFromResponseEmptyReasoningIsBad(t *testing.T) {
	body := `{"output":[{"type":"reasoning","summary":[]}]}`
	ev := parseThinkFromResponse(body)
	if ev.ok() {
		t.Fatal("empty reasoning should not be ok")
	}
	if !ev.HasThink {
		t.Fatal("empty reasoning should still count as has-think")
	}
}

func TestParseThinkFromResponseNoThink(t *testing.T) {
	body := `{"output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}`
	ev := parseThinkFromResponse(body)
	if ev.HasThink || ev.ok() {
		t.Fatalf("expected no think, got %+v", ev)
	}
}

package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestClassifyLLMRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  cliproxyexecutor.Request
		opts cliproxyexecutor.Options
		want string
	}{
		{
			name: "normal grok chat",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"model":"grok-4.6","input":"hello","reasoning":{"effort":"xhigh"}}`)},
			want: cliproxyexecutor.RequestClassNormal,
		},
		{
			name: "codex user turn stays normal",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"instructions":"You are Codex, an agent.","input":[{"type":"message","role":"user","content":"fix the bug"}],"reasoning":{"effort":"xhigh"}}`)},
			want: cliproxyexecutor.RequestClassNormal,
		},
		{
			name: "compact alt",
			opts: cliproxyexecutor.Options{Alt: "responses/compact"},
			req:  cliproxyexecutor.Request{Payload: []byte(`{"input":[]}`)},
			want: cliproxyexecutor.RequestClassCompaction,
		},
		{
			name: "compaction trigger",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"input":[{"type":"compaction_trigger"}],"reasoning":{"effort":"xhigh"}}`)},
			want: cliproxyexecutor.RequestClassCompaction,
		},
		{
			name: "codex turn metadata header",
			opts: cliproxyexecutor.Options{Headers: http.Header{"X-Codex-Turn-Metadata": []string{`{"request_kind":"compaction","compaction":{"trigger":"auto","implementation":"responses","phase":"pre_turn","strategy":"memento"}}`}}},
			req:  cliproxyexecutor.Request{Payload: []byte(`{"model":"grok-4.6","reasoning":{"effort":"xhigh"},"input":[{"type":"message","role":"user","content":"history"}]}`)},
			want: cliproxyexecutor.RequestClassCompaction,
		},
		{
			name: "codex client metadata json string",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"model":"grok-4.6","reasoning":{"effort":"xhigh"},"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\",\"compaction\":{\"strategy\":\"memento\"}}"},"input":[{"type":"message","role":"user","content":"history"}]}`)},
			want: cliproxyexecutor.RequestClassCompaction,
		},
		{
			name: "memento checkpoint prompt",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"model":"grok-4.6","reasoning":{"effort":"xhigh"},"tools":[],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task."}]}]}`)},
			want: cliproxyexecutor.RequestClassCompaction,
		},
		{
			name: "checkpoint prompt in history stays normal",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"model":"grok-4.6","reasoning":{"effort":"xhigh"},"input":[{"type":"message","role":"user","content":"You are performing a CONTEXT CHECKPOINT COMPACTION."},{"type":"message","role":"assistant","content":"ok"},{"type":"message","role":"user","content":"continue"}]}`)},
			want: cliproxyexecutor.RequestClassNormal,
		},
		{
			name: "compact request path",
			opts: cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/responses/compact"}},
			req:  cliproxyexecutor.Request{Payload: []byte(`{"input":[]}`)},
			want: cliproxyexecutor.RequestClassCompaction,
		},
		{
			name: "compaction object",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"object":"response.compaction","output":[{"type":"compaction"}]}`)},
			want: cliproxyexecutor.RequestClassNormal,
		},
		{
			name: "post compact user turn stays normal",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"model":"grok-4.6","reasoning":{"effort":"xhigh"},"input":[{"type":"compaction","encrypted_content":"opaque"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)},
			want: cliproxyexecutor.RequestClassNormal,
		},
		{
			name: "context management compaction config stays normal",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"context_management":{"compaction":{"enabled":true}},"input":[{"role":"user","content":"hello"}],"reasoning":{"effort":"xhigh"}}`)},
			want: cliproxyexecutor.RequestClassNormal,
		},
		{
			name: "title needle in xhigh chat stays normal",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"instructions":"You are Codex","input":[{"type":"message","role":"user","content":"please generate a title later after this long task"}],"reasoning":{"effort":"xhigh"}}`)},
			want: cliproxyexecutor.RequestClassNormal,
		},
		{
			name: "session title",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"instructions":"Generate a concise title for this conversation. Reply with the title only.","input":[{"type":"message","role":"user","content":"hello"}],"reasoning":{"effort":"low"}}`)},
			want: cliproxyexecutor.RequestClassSessionName,
		},
		{
			name: "claude code short title helper",
			req: cliproxyexecutor.Request{Payload: []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":[{"type":"text","text":"helper probe"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Return a short title."}],"tools":[],"max_tokens":32000,"thinking":{"type":"disabled"},"temperature":1,"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"stream":true}`)},
			want: cliproxyexecutor.RequestClassSessionName,
		},
		{
			name: "claude title after xai translate keeps session name",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"instructions":"Return a short title.\nYou are Claude Code, Anthropic's official CLI for Claude.","input":[{"type":"message","role":"user","content":"name this chat"}],"reasoning":{"effort":"medium"}}`)},
			want: cliproxyexecutor.RequestClassSessionName,
		},
		{
			name: "claude code 2.1.246 naming session helper",
			req: cliproxyexecutor.Request{Payload: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":[{"type":"text","text":"<session>\n湖南每个流程节点的计划办理窗口\n</session>\n\nWrite the title in the predominant language of the session — a stray word or code token in another language doesn't change it, and neither does the English of these instructions."}]}],"system":[{"type":"text","text":"You are naming a coding session so the user can pick it out of a long list of sessions. Return JSON with a single \"title\" field."}],"tools":[],"max_tokens":32000,"thinking":{"type":"enabled"},"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"stream":true}`)},
			opts: cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/messages"}},
			want: cliproxyexecutor.RequestClassSessionName,
		},
		{
			name: "claude code 2.1.246 title after xai translate medium effort",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"instructions":"You are naming a coding session so the user can pick it out of a long list of sessions. Return JSON with a single \"title\" field.","input":[{"type":"message","role":"user","content":"<session>\n湖南每个流程节点的计划办理窗口\n</session>\n\nWrite the title in the predominant language of the session."}],"reasoning":{"effort":"medium"}}`)},
			want: cliproxyexecutor.RequestClassSessionName,
		},
		{
			name: "claude code 2.1.246 title schema only medium effort",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":[{"type":"text","text":"<session>plan window</session>"}]}],"thinking":{"type":"enabled"},"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}}`)},
			want: cliproxyexecutor.RequestClassSessionName,
		},
		{
			name: "claude code chat stays normal",
			req: cliproxyexecutor.Request{Payload: []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"explain this repo"}],"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"thinking":{"type":"enabled"},"max_tokens":32000}`)},
			opts: cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/messages"}},
			want: cliproxyexecutor.RequestClassNormal,
		},
		{
			name: "ambient internal",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"instructions":"You are Codex","input":[{"type":"message","content":"ambient_suggestions background turn"}],"reasoning":{"effort":"low"}}`)},
			want: cliproxyexecutor.RequestClassInternal,
		},
		{
			name: "codex low effort internal",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"instructions":"You are Codex, based on GPT-5.","messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"low"}}`)},
			want: cliproxyexecutor.RequestClassInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyLLMRequest(tt.req, tt.opts); got != tt.want {
				t.Fatalf("class = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBeginFillFirstHoldRoutesAuxToDownrank(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  cliproxyexecutor.Request
		opts cliproxyexecutor.Options
		want string
	}{
		{
			name: "compaction",
			opts: cliproxyexecutor.Options{Alt: "responses/compact"},
			want: cliproxyexecutor.RequestClassCompaction,
		},
		{
			name: "session name",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"instructions":"Generate a concise title for this conversation. Reply with the title only."}`)},
			want: cliproxyexecutor.RequestClassSessionName,
		},
		{
			name: "claude code title",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"system":[{"type":"text","text":"Return a short title."}],"messages":[{"role":"user","content":[{"type":"text","text":"name this"}]}],"thinking":{"type":"disabled"},"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}}}`)},
			want: cliproxyexecutor.RequestClassSessionName,
		},
		{
			name: "internal",
			req:  cliproxyexecutor.Request{Payload: []byte(`{"instructions":"You are Codex","input":[{"content":"ambient_suggestions"}],"reasoning":{"effort":"low"}}`)},
			want: cliproxyexecutor.RequestClassInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := NewManager(nil, &FillFirstSelector{}, nil)
			opts := tt.opts
			ctx, finish := m.beginFillFirstHold(context.Background(), tt.req, &opts)
			t.Cleanup(finish)
			if got := cliproxyexecutor.RequestClassFromContext(ctx); got != tt.want {
				t.Fatalf("ctx class = %q, want %q", got, tt.want)
			}
			if got := cliproxyexecutor.RequestClassFromMetadata(opts.Metadata); got != tt.want {
				t.Fatalf("meta class = %q, want %q", got, tt.want)
			}
			hold := fillFirstHoldFrom(ctx)
			if hold == nil {
				t.Fatal("missing fill-first hold")
			}
			if hold.pool != m.fillFirstDownrank {
				t.Fatal("aux request did not use downrank pool")
			}
			if !hold.noRetry {
				t.Fatal("aux request should skip credential retry")
			}
			if !skipCredentialRetry(ctx) {
				t.Fatal("skipCredentialRetry = false")
			}
		})
	}
}

func TestBeginFillFirstHoldKeepsNormalOnMainPool(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, &FillFirstSelector{}, nil)
	opts := cliproxyexecutor.Options{}
	ctx, finish := m.beginFillFirstHold(context.Background(), cliproxyexecutor.Request{Payload: []byte(`{"input":"hello"}`)}, &opts)
	t.Cleanup(finish)
	if got := cliproxyexecutor.RequestClassFromContext(ctx); got != cliproxyexecutor.RequestClassNormal {
		t.Fatalf("class = %q", got)
	}
	hold := fillFirstHoldFrom(ctx)
	if hold == nil || hold.pool != m.fillFirst || hold.noRetry {
		t.Fatalf("hold = %#v", hold)
	}
	if skipCredentialRetry(ctx) {
		t.Fatal("normal requests should rotate credentials")
	}
}

package auth

import (
	"errors"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestIsXAIQuotaExhaustedError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "free usage code", err: errors.New(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage"}`), want: true},
		{name: "spending limit", err: errors.New(`{"code":"personal-team-blocked:spending-limit","error":"out of credits"}`), want: true},
		{name: "bare 429", err: errors.New(`status 429: rate limit`), want: false},
		{name: "network", err: errors.New(`dial tcp: i/o timeout`), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isXAIQuotaExhaustedError(tc.err); got != tc.want {
				t.Fatalf("isXAIQuotaExhaustedError() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldRotateXAICredential(t *testing.T) {
	t.Parallel()
	if shouldRotateXAICredential(errors.New("temporary upstream blip")) {
		t.Fatal("transient error should not rotate")
	}
	if !shouldRotateXAICredential(errors.New(`subscription:free-usage-exhausted`)) {
		t.Fatal("quota exhausted should rotate")
	}
	if !shouldRotateXAICredential(&cliproxyexecutor.NoThinkStreamError{Detail: "missing think"}) {
		t.Fatal("no-think should rotate")
	}
}

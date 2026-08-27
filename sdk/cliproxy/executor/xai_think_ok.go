package executor

import "context"

type xaiThinkOKKey struct{}

// XAIThinkOKFunc is invoked when an xAI response has a real Think stream.
type XAIThinkOKFunc func(authID string)

// WithXAIThinkOK stores a Think-OK callback on ctx.
func WithXAIThinkOK(ctx context.Context, fn XAIThinkOKFunc) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, xaiThinkOKKey{}, fn)
}

// NotifyXAIThinkOK reports that the current request produced a real Think stream.
func NotifyXAIThinkOK(ctx context.Context, authID string) {
	if ctx == nil {
		return
	}
	fn, _ := ctx.Value(xaiThinkOKKey{}).(XAIThinkOKFunc)
	if fn == nil {
		return
	}
	fn(authID)
}

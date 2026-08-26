package helps

import (
	"context"
	"net"
	"net/http/httptrace"
	"sync"
)

type warpDialRecorderKey struct{}

type warpDialRecorder struct {
	mu         sync.Mutex
	clientAddr string
}

// WithWarpDialRecorder records the local TCP address of the SOCKS/WARP
// connection used by this request so no-think quarantine can target that node.
func WithWarpDialRecorder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if warpDialRecorderFrom(ctx) != nil {
		return ctx
	}
	rec := &warpDialRecorder{}
	ctx = context.WithValue(ctx, warpDialRecorderKey{}, rec)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn != nil {
				rec.setAddr(info.Conn.LocalAddr())
			}
		},
	})
}

func warpDialRecorderFrom(ctx context.Context) *warpDialRecorder {
	if ctx == nil {
		return nil
	}
	rec, _ := ctx.Value(warpDialRecorderKey{}).(*warpDialRecorder)
	return rec
}

func (r *warpDialRecorder) setAddr(addr net.Addr) {
	if r == nil || addr == nil {
		return
	}
	s := addr.String()
	if s == "" {
		return
	}
	r.mu.Lock()
	r.clientAddr = s
	r.mu.Unlock()
}

func recordWarpDial(ctx context.Context, conn net.Conn) {
	if conn == nil {
		return
	}
	if rec := warpDialRecorderFrom(ctx); rec != nil {
		rec.setAddr(conn.LocalAddr())
	}
}

// WarpDialClientAddr is the HAProxy client address (containerIP:port) for this request.
func WarpDialClientAddr(ctx context.Context) string {
	rec := warpDialRecorderFrom(ctx)
	if rec == nil {
		return ""
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.clientAddr
}

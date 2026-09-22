package helps

import (
	"context"
	"net"
	"net/http/httptrace"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/warprotate"
)

type warpDialRecorderKey struct{}

type warpDialRecorder struct {
	mu         sync.Mutex
	clientAddr string
	instance   string
	server     string
	label      string
	exitIP     string
}

// WarpExitMeta is the LB node captured for this request.
type WarpExitMeta struct {
	Client   string
	Instance string
	Server   string
	Label    string
	ExitIP   string
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
	same := r.clientAddr == s && r.label != ""
	r.clientAddr = s
	r.mu.Unlock()
	if same {
		return
	}
	go r.resolve(s)
}

func (r *warpDialRecorder) resolve(client string) {
	if r == nil || strings.TrimSpace(client) == "" || warprotate.BaseURL() == "" {
		return
	}
	info := warprotate.Lookup(context.Background(), client)
	if info == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.clientAddr != client {
		return
	}
	r.instance = info.Instance
	r.server = info.Server
	r.label = info.Label
	r.exitIP = info.ExitIP
}

func recordWarpDial(ctx context.Context, conn net.Conn) {
	if conn == nil {
		return
	}
	recordWarpDialAddr(ctx, conn.LocalAddr())
}

func recordWarpDialAddr(ctx context.Context, addr net.Addr) {
	if rec := warpDialRecorderFrom(ctx); rec != nil {
		rec.setAddr(addr)
	}
}

// WarpDialClientAddr is the HAProxy client address (containerIP:port) for this request.
func WarpDialClientAddr(ctx context.Context) string {
	meta := WarpDialMeta(ctx)
	return meta.Client
}

// WarpDialMeta returns the captured SOCKS client address and resolved LB node.
func WarpDialMeta(ctx context.Context) WarpExitMeta {
	rec := warpDialRecorderFrom(ctx)
	if rec == nil {
		return WarpExitMeta{}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return WarpExitMeta{
		Client:   rec.clientAddr,
		Instance: rec.instance,
		Server:   rec.server,
		Label:    rec.label,
		ExitIP:   rec.exitIP,
	}
}

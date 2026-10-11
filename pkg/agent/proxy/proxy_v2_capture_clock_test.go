package proxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// lagStopParser is a V2 parser held behind its connection's traffic that,
// once released, stops on the first exchange it gets to: it cannot decode it,
// reports it (Session.ReportStoppedOn) and returns, as the HTTP/1 and MySQL
// recorders do.
type lagStopParser struct {
	stubParser
	release chan struct{}
	stopped chan struct{}
}

func (p *lagStopParser) IsV2() bool { return true }

func (p *lagStopParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	defer close(p.stopped)
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	c, err := s.V2.ClientStream.ReadChunk()
	if err != nil {
		return err
	}
	s.V2.ReportStoppedOn(models.HTTP, c.ReadAt, "http decode error: a request it cannot decode")
	return errors.New("http decode error: a request it cannot decode")
}

// startConnection runs recordViaSupervisor for one connection of p, recording
// to mgr, and returns the application's end of it.
func startConnection(t *testing.T, p *Proxy, mgr *syncMock.SyncMockManager, parser integrations.Integrations, clientConnID int64) net.Conn {
	t.Helper()
	clientApp, srcConn := net.Pipe()
	dstConn, destSvc := net.Pipe()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := destSvc.Read(buf); err != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), parser, "test",
			make(chan *models.Mock, 8), &errgroup.Group{}, zap.NewNop(), clientConnID, clientConnID+1000, models.OutgoingOptions{})
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientApp.Close()
		_ = destSvc.Close()
		<-done
		_ = srcConn.Close()
		_ = dstConn.Close()
	})
	return clientApp
}

// P20 (stress audit rank 1): a left-out span ended on the agent's clock. A
// parser held 10 s behind its capture stops on connection A, on an exchange A
// carried at W, and A carries nothing after. A test case on a healthy
// connection B, recorded 5 s after W (inside those 10 s), is held until B's
// lagging parser has got through it, and checked after A's stop. With the stop
// dated on the agent's clock, A's span ran from W to the moment its parser got
// there, W+10 s, and left B's test case out; on the capture's clock it ends at
// the last chunk A carried, and B's test case is kept. A's own exchange is
// still left out.
func TestLeftOutSpan_AStopBehindTheTrafficDoesNotLeaveOutAHealthyConnectionsTestCase(t *testing.T) {
	const lag = 10 * time.Second
	mgr := syncMock.New(nil)
	w := relay.NewWatermark()
	p := &Proxy{recordBufferQueueSize: 64, capture: w}
	t.Cleanup(mgr.AddWatermark(proxyWatermark{Watermark: w, mgr: mgr}))

	a := &lagStopParser{release: make(chan struct{}), stopped: make(chan struct{})}
	b := &heldParser{release: make(chan struct{})}
	appA := startConnection(t, p, mgr, a, 1)
	appB := startConnection(t, p, mgr, b, 2)
	waitUntilT(t, 30*time.Second, "the proxy to follow both connections", func() bool { return w.Following() == 2 })

	// A carries one exchange at W, and nothing after.
	w0 := time.Now()
	if _, err := appA.Write([]byte("BAD / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// B's test case, half way through A's lag. The sleeps are the lag under
	// test, not waits for anything to settle.
	time.Sleep(lag / 2)
	start := time.Now()
	if _, err := appB.Write([]byte("GET /healthy HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	end := time.Now()
	wm := mgr.Watermark()
	if wm.Settled("", "", start, end) {
		t.Fatal("B's test case is final before its own parser got to it")
	}

	// A's parser gets to W's exchange lag later, and stops on it.
	time.Sleep(time.Until(w0.Add(lag)))
	close(a.release)
	<-a.stopped
	waitUntilT(t, 30*time.Second, "A's parser to return", func() bool { return w.Following() == 1 })

	// Then B's parser catches up: its test case is final, and checked now.
	close(b.release)
	waitUntilT(t, 30*time.Second, "B's test case to be final", func() bool { return wm.Settled("", "", start, end) })
	if left, n := mgr.WasMockOrphanedInWindow(start, end); left {
		t.Fatalf("B's test case, recorded %v after A's last chunk and on a connection that kept up, was left out by %d span(s) of A's stop: the stop was dated when A's parser got there, not when A carried it",
			start.Sub(w0), n)
	}
	// A's own exchange is left out: what A carried is lost with its parser.
	if left, _ := mgr.WasMockOrphanedInWindow(w0, w0.Add(time.Millisecond)); !left {
		t.Fatal("A's exchange, which its parser stopped on, is not left out")
	}
}

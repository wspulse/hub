package wspulse_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	wspulse "github.com/wspulse/server"
)

// ── NoopCollector ─────────────────────────────────────────────────────────────

func TestNoopCollector_ImplementsInterface(t *testing.T) {
	t.Parallel()
	// Compile-time check already exists in metrics.go; this test documents intent.
	var _ wspulse.MetricsCollector = wspulse.NoopCollector{}
}

func TestNoopCollector_AllMethodsCallable(t *testing.T) {
	t.Parallel()
	var c wspulse.NoopCollector
	c.ConnectionOpened("room", "conn")
	c.ConnectionClosed("room", "conn", time.Second, wspulse.DisconnectNormal)
	c.ResumeAttempt("room", "conn")
	c.RoomCreated("room")
	c.RoomDestroyed("room")
	c.MessageReceived("room", 100)
	c.MessageBroadcast("room", 100, 5)
	c.MessageSent("room", "conn", 100)
	c.FrameDropped("room", "conn")
	c.SendBufferUtilization("room", "conn", 10, 256)
	c.PongTimeout("room", "conn")
}

// ── WithMetrics option ────────────────────────────────────────────────────────

func TestWithMetrics_NilPanics(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() {
		_ = wspulse.WithMetrics(nil)
	})
}

func TestWithMetrics_DefaultIsNoop(t *testing.T) {
	t.Parallel()
	// Hub should start and close cleanly without WithMetrics (uses NoopCollector).
	srv := wspulse.NewHub(func(_ *http.Request) (string, string, error) {
		return "room", "conn", nil
	})
	srv.Close()
}

func TestWithMetrics_CustomCollector_Accepted(t *testing.T) {
	t.Parallel()
	srv := wspulse.NewHub(
		func(_ *http.Request) (string, string, error) {
			return "room", "", nil
		},
		wspulse.WithMetrics(&recordingCollector{}),
	)
	t.Cleanup(srv.Close)
}

// ── recordingCollector ────────────────────────────────────────────────────────

// recordingCollector is a test helper that records all metrics calls.
// Safe for concurrent use.
type recordingCollector struct {
	mu     sync.Mutex
	events []metricsEvent
}

type metricsEvent struct {
	name         string
	roomID       string
	connectionID string
	sizeBytes    int
	fanOut       int
	duration     time.Duration
	used         int
	capacity     int
	reason       wspulse.DisconnectReason
}

func (r *recordingCollector) record(e metricsEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recordingCollector) ConnectionOpened(roomID, connectionID string) {
	r.record(metricsEvent{name: "ConnectionOpened", roomID: roomID, connectionID: connectionID})
}
func (r *recordingCollector) ConnectionClosed(roomID, connectionID string, duration time.Duration, reason wspulse.DisconnectReason) {
	r.record(metricsEvent{name: "ConnectionClosed", roomID: roomID, connectionID: connectionID, duration: duration, reason: reason})
}
func (r *recordingCollector) ResumeAttempt(roomID, connectionID string) {
	r.record(metricsEvent{name: "ResumeAttempt", roomID: roomID, connectionID: connectionID})
}
func (r *recordingCollector) RoomCreated(roomID string) {
	r.record(metricsEvent{name: "RoomCreated", roomID: roomID})
}
func (r *recordingCollector) RoomDestroyed(roomID string) {
	r.record(metricsEvent{name: "RoomDestroyed", roomID: roomID})
}
func (r *recordingCollector) MessageReceived(roomID string, sizeBytes int) {
	r.record(metricsEvent{name: "MessageReceived", roomID: roomID, sizeBytes: sizeBytes})
}
func (r *recordingCollector) MessageBroadcast(roomID string, sizeBytes int, fanOut int) {
	r.record(metricsEvent{name: "MessageBroadcast", roomID: roomID, sizeBytes: sizeBytes, fanOut: fanOut})
}
func (r *recordingCollector) MessageSent(roomID, connectionID string, sizeBytes int) {
	r.record(metricsEvent{name: "MessageSent", roomID: roomID, connectionID: connectionID, sizeBytes: sizeBytes})
}
func (r *recordingCollector) FrameDropped(roomID, connectionID string) {
	r.record(metricsEvent{name: "FrameDropped", roomID: roomID, connectionID: connectionID})
}
func (r *recordingCollector) SendBufferUtilization(roomID, connectionID string, used, capacity int) {
	r.record(metricsEvent{name: "SendBufferUtilization", roomID: roomID, connectionID: connectionID, used: used, capacity: capacity})
}
func (r *recordingCollector) PongTimeout(roomID, connectionID string) {
	r.record(metricsEvent{name: "PongTimeout", roomID: roomID, connectionID: connectionID})
}

var _ wspulse.MetricsCollector = (*recordingCollector)(nil)

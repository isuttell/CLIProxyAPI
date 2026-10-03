package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestExecutionDrainWaitsForHandlerAndRejectsNewRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	drain := newExecutionDrain()
	engine := gin.New()
	engine.Use(drain.middleware())
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	engine.GET("/stream", func(c *gin.Context) { close(entered); <-release; c.Status(http.StatusOK) })
	go func() {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stream", nil))
		close(finished)
	}()
	<-entered
	drain.stop()
	select {
	case <-drain.done:
		t.Fatal("drain completed with a live stream")
	default:
	}
	rejected := httptest.NewRecorder()
	engine.ServeHTTP(rejected, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("new request status: %d", rejected.Code)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := drain.wait(cancelled); err != context.Canceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
	close(release)
	<-finished
	if err := drain.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	drain.stop()
}

func TestExecutionDrainClosesHijackedWebSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	drain := newExecutionDrain()
	engine := gin.New()
	engine.Use(drain.middleware())
	entered := make(chan struct{})
	finished := make(chan struct{})
	engine.GET("/ws", func(c *gin.Context) {
		upgrader := websocket.Upgrader{}
		conn, errUpgrade := upgrader.Upgrade(c.Writer, c.Request, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade: %v", errUpgrade)
			close(finished)
			return
		}
		defer func() {
			_ = conn.Close()
			close(finished)
		}()
		close(entered)
		_, _, _ = conn.ReadMessage()
	})
	server := httptest.NewServer(engine)
	defer server.Close()
	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = conn.Close() }()
	<-entered
	drain.stop()
	if errWait := drain.wait(context.Background()); errWait != nil {
		t.Fatal(errWait)
	}
	<-finished
	if _, _, errRead := conn.ReadMessage(); errRead == nil {
		t.Fatal("websocket remained open after drain")
	}
}

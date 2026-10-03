package cliproxy

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
)

// executionDrain joins handlers after listener closure; Close alone does not wait for them.
type executionDrain struct {
	mu          sync.Mutex
	closing     bool
	active      int
	done        chan struct{}
	connections map[net.Conn]struct{}
}

func newExecutionDrain() *executionDrain {
	return &executionDrain{done: make(chan struct{}), connections: make(map[net.Conn]struct{})}
}

func (d *executionDrain) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		d.mu.Lock()
		if d.closing {
			d.mu.Unlock()
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		d.active++
		d.mu.Unlock()
		writer := &drainResponseWriter{ResponseWriter: c.Writer, drain: d}
		c.Writer = writer
		defer func() {
			d.mu.Lock()
			if writer.conn != nil {
				delete(d.connections, writer.conn)
			}
			d.active--
			if d.closing && d.active == 0 {
				close(d.done)
			}
			d.mu.Unlock()
		}()
		c.Next()
	}
}

type drainResponseWriter struct {
	gin.ResponseWriter
	drain *executionDrain
	conn  net.Conn
}

func (w *drainResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, errHijack := w.ResponseWriter.Hijack()
	if errHijack != nil {
		return nil, nil, errHijack
	}
	w.drain.mu.Lock()
	if w.drain.closing {
		w.drain.mu.Unlock()
		_ = conn.Close()
		return nil, nil, net.ErrClosed
	}
	w.conn = conn
	w.drain.connections[conn] = struct{}{}
	w.drain.mu.Unlock()
	return conn, rw, nil
}

func (d *executionDrain) stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.closing {
		d.closing = true
		for conn := range d.connections {
			_ = conn.Close()
		}
		if d.active == 0 {
			close(d.done)
		}
	}
	d.mu.Unlock()
}

func (d *executionDrain) wait(ctx context.Context) error {
	if d == nil {
		return nil
	}
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

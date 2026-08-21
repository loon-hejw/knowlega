package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/config"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

func TestHTTPProxyKeepsSessionStateSSEAlivePastKratosDefaultTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/session-state/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, ": connected\n\n")
		flusher.Flush()
		time.Sleep(1200 * time.Millisecond)
		_, _ = io.WriteString(w, "event: session_state\ndata: {\"threadRef\":\"web:alice:1\"}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.QM.RouteMode = "proxy"
	cfg.QM.NodeCoreURL = upstream.URL
	srv, err := NewHTTPServer(cfg, &data.Postgres{}, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	frontend := httptest.NewServer(srv)
	defer frontend.Close()

	response, err := http.Get(frontend.URL + "/v1/session-state/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "session_state") {
		t.Fatalf("SSE body was cut off before the post-timeout event: %q", body)
	}
}

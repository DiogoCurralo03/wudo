package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/italoalmeida0/wudo/internal/protocol"
	"github.com/italoalmeida0/wudo/internal/server"
	"github.com/italoalmeida0/wudo/internal/state"
)

// TestKeepAliveKillsSilentClient checks that a connection which stops answering
// pings is treated like a disconnect: the command is terminated instead of being
// left running forever.
func TestKeepAliveKillsSilentClient(t *testing.T) {
	t.Setenv("WUDO_KEEPALIVE", "300ms")
	t.Setenv(state.DirEnvVar, t.TempDir())

	srv := server.New(server.Config{})
	t.Cleanup(srv.Shutdown)
	if err := srv.Start(); err != nil {
		t.Fatalf("start service: %v", err)
	}
	srv.Activate(time.Minute)

	ep, err := state.ReadEndpoint()
	if err != nil {
		t.Fatalf("read endpoint: %v", err)
	}

	// A raw WebSocket client that starts a command and then goes silent: it
	// stops reading and never answers another ping.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d%s", ep.Port, protocol.PathRun), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	start := protocol.StartRequest{
		Type:   protocol.TypeStart,
		Schema: protocol.SchemaVersion,
		Argv:   []string{"cmd", "/c", "ping -n 60 127.0.0.1 >nul"},
		Shell:  true,
		Cwd:    os.TempDir(),
	}
	payload, err := json.Marshal(start)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("write start: %v", err)
	}

	waitForSessions(t, srv, 1)

	// Stop reading entirely. The service must notice and tear the command down.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if srv.SessionCount() == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the silent client kept its command running past the keepalive deadline")
}

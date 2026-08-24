package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/net/websocket"
)

// registerBrowserWebSocket installs the only browser-facing byte relay. The
// HTTP handler authorizes before upgrade; after upgrade it forwards opaque RFB
// payloads and never interprets or persists them.
func (application *apiServer) registerBrowserWebSocket(mux *http.ServeMux, publicOrigin string) {
	handler := websocket.Handler(func(conn *websocket.Conn) {
		request := conn.Request()
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(parts) != 7 {
			_ = conn.Close()
			return
		}
		systemKey := parts[3]
		operationID, err := strconv.ParseInt(parts[5], 10, 64)
		if err != nil || operationID < 1 {
			_ = conn.Close()
			return
		}
		cookie, ok := findSessionCookie(request)
		if !ok {
			_ = conn.Close()
			return
		}
		session, err := application.authenticateFull(request.Context(), cookie, "附着人工浏览器登录")
		if err != nil {
			_ = conn.Close()
			return
		}
		op, err := application.browsers.GetOperation(request.Context(), systemKey, operationID)
		if err != nil || op.ActorUserID == nil || op.ActorSessionID == nil || *op.ActorUserID != session.User.ID || *op.ActorSessionID != session.ID || (op.State != "Running" && op.State != "AwaitingReconnect") {
			_ = conn.Close()
			return
		}
		tunnel, err := application.browserTunnels.attachAwait(request.Context(), operationID)
		if err != nil {
			_ = conn.Close()
			return
		}
		if op.State == "AwaitingReconnect" && application.browsers.ResumeReconnect(request.Context(), operationID) != nil {
			application.browserTunnels.detach(tunnel)
			_ = conn.Close()
			return
		}
		defer func() {
			application.browserTunnels.detach(tunnel)
			// A VNC server permits one RFB client per bridge. Closing this
			// attachment asks Lintel to finish that generation; its live
			// operation then opens a higher-sequence tunnel for reattach.
			application.browserTunnels.closeAttachment(tunnel)
			application.awaitBrowserReconnect(operationID)
		}()
		bridgeWebSocket(request.Context(), conn, tunnel)
	})
	mux.HandleFunc("GET /api/v1/browser-login/{systemKey}/operations/{browserOperationId}/ws", func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Origin") != publicOrigin {
			http.Error(writer, "browser websocket origin is not allowed", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(writer, request)
	})
}

func bridgeWebSocket(ctx context.Context, conn *websocket.Conn, tunnel *browserTunnel) {
	defer conn.Close()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buffer := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buffer)
			if n > 0 {
				payload := append([]byte(nil), buffer[:n]...)
				select {
				case tunnel.toLintel <- payload:
				case <-tunnel.closed:
					return
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tunnel.closed:
			return
		case <-readDone:
			return
		case payload := <-tunnel.fromLintel:
			if _, err := conn.Write(payload); err != nil {
				return
			}
		}
	}
}

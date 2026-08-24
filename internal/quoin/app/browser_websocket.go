package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/net/websocket"
)

type browserAttachmentReservationContextKey struct{}

type browserAttachmentReservationContext struct {
	operationID int64
	reservation *browserAttachmentReservation
}

type browserWebSocketTunnelContextKey struct{}

type browserWebSocketTunnelContext struct {
	tunnel  *browserTunnel
	handled *bool
}

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
		// RFB is an opaque byte protocol; x/net/websocket otherwise defaults
		// Conn.Write to text frames, which noVNC cannot decode as ArrayBuffer.
		conn.PayloadType = websocket.BinaryFrame
		attachment, ok := request.Context().Value(browserAttachmentReservationContextKey{}).(browserAttachmentReservationContext)
		preAttached, attached := request.Context().Value(browserWebSocketTunnelContextKey{}).(browserWebSocketTunnelContext)
		if !ok || !attached || attachment.operationID != operationID || preAttached.tunnel == nil {
			_ = conn.Close()
			return
		}
		*preAttached.handled = true
		tunnel := preAttached.tunnel
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
		// x/net/websocket upgrades inside Handler.ServeHTTP. Authenticate the
		// complete Session and operation ownership first so rejected requests
		// receive an HTTP error rather than a successful Upgrade then EOF.
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(parts) != 7 {
			http.Error(writer, "browser operation path is invalid", http.StatusNotFound)
			return
		}
		operationID, err := strconv.ParseInt(parts[5], 10, 64)
		if err != nil || operationID < 1 {
			http.Error(writer, "browser operation is invalid", http.StatusUnprocessableEntity)
			return
		}
		cookie, ok := findSessionCookie(request)
		if !ok {
			http.Error(writer, "browser login requires an authenticated Session", http.StatusUnauthorized)
			return
		}
		session, err := application.authenticateFull(request.Context(), cookie, "附着人工浏览器登录")
		if err != nil {
			http.Error(writer, "browser login Session is not authorized", http.StatusUnauthorized)
			return
		}
		op, err := application.browsers.GetOperation(request.Context(), parts[3], operationID)
		if err != nil || op.ActorUserID == nil || op.ActorSessionID == nil || *op.ActorUserID != session.User.ID || *op.ActorSessionID != session.ID || (op.State != "Running" && op.State != "AwaitingReconnect") {
			http.Error(writer, "browser operation is not attachable", http.StatusForbidden)
			return
		}
		reservation, err := application.browserTunnels.reserveAttachment(operationID)
		if err != nil {
			browserAttachmentConflict(writer)
			return
		}
		tunnel, err := application.browserTunnels.attachAwait(request.Context(), operationID, reservation)
		if err != nil {
			application.browserTunnels.releaseReservation(operationID, reservation)
			browserTunnelUnavailable(writer)
			return
		}
		handled := false
		request = request.WithContext(context.WithValue(request.Context(), browserAttachmentReservationContextKey{}, browserAttachmentReservationContext{operationID: operationID, reservation: reservation}))
		request = request.WithContext(context.WithValue(request.Context(), browserWebSocketTunnelContextKey{}, browserWebSocketTunnelContext{tunnel: tunnel, handled: &handled}))
		handler.ServeHTTP(writer, request)
		application.browserTunnels.releaseReservation(operationID, reservation)
		if !handled {
			application.browserTunnels.detach(tunnel)
			application.browserTunnels.closeAttachment(tunnel)
			application.awaitBrowserReconnect(operationID)
		}
	})
}

func browserAttachmentConflict(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(writer).Encode(map[string]string{
		"code": "identity_busy", "detail": "该浏览器身份已有活动连接。",
	})
}

func browserTunnelUnavailable(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(writer).Encode(struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}{
		Code: "runtime_unavailable", Message: "安全浏览器正在准备连接，请稍后重试。", Retryable: true,
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

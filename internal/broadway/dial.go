package broadway

import (
	"context"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// wsHeaders are the handshake headers broadwayd matches case-sensitively.
// Go canonicalises them to Sec-Websocket-*, which broadwayd rejects with 400.
var wsHeaders = []string{
	"Sec-WebSocket-Key",
	"Sec-WebSocket-Version",
	"Sec-WebSocket-Protocol",
	"Sec-WebSocket-Extensions",
}

// Dial opens a broadway-subprotocol WebSocket to url (ws:// or http://).
func Dial(ctx context.Context, url string) (*websocket.Conn, error) {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols:    []string{"broadway"},
		CompressionMode: websocket.CompressionDisabled, // broadwayd does not negotiate permessage-deflate
		HTTPClient:      &http.Client{Transport: casedHeaderTransport{next: http.DefaultTransport}},
	})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxMessageBytes)
	return conn, nil
}

// casedHeaderTransport rewrites handshake header keys to their exact spelling.
// Go writes map keys verbatim, so the rewritten keys go out on the wire unchanged.
type casedHeaderTransport struct {
	next http.RoundTripper
}

// RoundTrip sends the request, fixing header case on WebSocket upgrades.
func (t casedHeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		r = r.Clone(r.Context())
		for _, k := range wsHeaders {
			canon := http.CanonicalHeaderKey(k)
			if v, ok := r.Header[canon]; ok {
				delete(r.Header, canon)
				r.Header[k] = v
			}
		}
	}
	return t.next.RoundTrip(r)
}

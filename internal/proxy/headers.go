package proxy

import "net/http"

var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func copyRequestHeaders(destination, source http.Header) {
	copyHeaders(destination, source)
	destination.Del("Host")
	destination.Del("Content-Length")
	destination.Del(accountHeader)
	destination.Del(requestIDHeader)
	// Browser access belongs to the client-to-relay hop, not the relay's
	// authenticated server-to-server request to Anthropic.
	destination.Del("Origin")
	destination.Del("anthropic-dangerous-direct-browser-access")
}

func copyResponseHeaders(destination, source http.Header) {
	copyHeaders(destination, source)
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		if _, excluded := hopByHopHeaders[http.CanonicalHeaderKey(key)]; excluded {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

package server

import (
	"crypto/tls"
	"net/http"
	"time"
)

// NewHTTPServer creates a configured HTTP server with the standard middleware stack.
func NewHTTPServer(addr string, handler http.Handler, tlsCfg *tls.Config) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		TLSConfig:    tlsCfg,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
}

// Package imapserver hosts a local IMAP server for a caller-provided mailbox
// backend.
package imapserver

import (
	"fmt"
	"log/slog"
	"net"

	"github.com/emersion/go-imap/backend"
	imap "github.com/emersion/go-imap/server"
)

// Backend supplies mailbox operations to the IMAP protocol server.
type Backend = backend.Backend

// Server serves IMAP connections from a local backend.
//
// Authentication is allowed without TLS because the listener is restricted to
// loopback. Credentials and message contents must not cross a network boundary.
type Server struct {
	inner *imap.Server
}

// New creates an IMAP server backed by b. The server accepts client logins over
// loopback and limits incoming literals to 64 MiB.
func New(b Backend) *Server {
	s := imap.New(b)
	s.AllowInsecureAuth = true
	s.MaxLiteralSize = 64 << 20
	return &Server{inner: s}
}

// ListenAndServe listens on a loopback TCP address. An empty address uses
// 127.0.0.1:1143.
func (s *Server) ListenAndServe(addr string) error {
	l, err := s.Listen(addr)
	if err != nil {
		return err
	}
	return s.Serve(l)
}

// Listen binds a loopback TCP address and returns its listener.
func (s *Server) Listen(addr string) (net.Listener, error) {
	if addr == "" {
		addr = "127.0.0.1:1143"
	}
	if err := checkLoopbackAddress(addr); err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return l, nil
}

func checkLoopbackAddress(addr string) error {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return err
	}
	if tcpAddr.IP == nil || !tcpAddr.IP.IsLoopback() {
		return fmt.Errorf("IMAP server only accepts loopback TCP listeners")
	}
	return nil
}

// Serve accepts a TCP listener bound to a loopback address. Other listener
// types and addresses are rejected so the server cannot expose credentials or
// decrypted mail to another machine.
func (s *Server) Serve(l net.Listener) error {
	if l == nil {
		return fmt.Errorf("IMAP listener is required")
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok || addr.IP == nil || !addr.IP.IsLoopback() {
		_ = l.Close()
		return fmt.Errorf("IMAP server only accepts loopback TCP listeners")
	}
	return s.inner.Serve(observedListener{Listener: l})
}

type observedListener struct{ net.Listener }

func (l observedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		slog.Debug("local IMAP TCP client connected")
	}
	return conn, err
}

// Close stops the listener and closes active client connections.
func (s *Server) Close() error { return s.inner.Close() }

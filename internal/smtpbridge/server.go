// Package smtpbridge accepts local SMTP submissions and sends them through
// the authenticated Proton mail service.
package smtpbridge

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"strings"
	"time"

	mailsvc "github.com/roman-16/proton-cli/internal/service/mail"
)

const maxMessageSize = 64 << 20

// Server accepts unencrypted SMTP on loopback and delivers submitted messages
// through the caller's already authenticated Proton profile.
type Server struct {
	ctx  context.Context
	mail *mailsvc.Service
	ln   net.Listener
}

func New(ctx context.Context, service *mailsvc.Service) *Server {
	return &Server{ctx: ctx, mail: service}
}

func (s *Server) Listen(addr string) (net.Listener, error) {
	if addr == "" {
		addr = "127.0.0.1:1025"
	}
	a, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	if a.IP == nil || !a.IP.IsLoopback() {
		return nil, fmt.Errorf("SMTP server only accepts loopback TCP listeners")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.ln = ln
	return ln, nil
}

func (s *Server) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serveConn(conn)
	}
}

func (s *Server) Close() error {
	if s.ln == nil {
		return nil
	}
	return s.ln.Close()
}

func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(line string) bool {
		_, err := fmt.Fprintf(w, "%s\r\n", line)
		return err == nil && w.Flush() == nil
	}
	if !write("220 localhost proton-cli local SMTP") {
		return
	}
	var from string
	var recipients []mailsvc.Recipient
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if len(line) > 4096 {
			if !write("500 command too long") {
				return
			}
			continue
		}
		line = strings.TrimSpace(line)
		cmd, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "EHLO", "HELO":
			if !write("250-localhost") || !write("250 SIZE 67108864") {
				return
			}
		case "NOOP":
			if !write("250 ok") {
				return
			}
		case "RSET":
			from, recipients = "", nil
			if !write("250 reset") {
				return
			}
		case "QUIT":
			write("221 bye")
			return
		case "MAIL":
			address, ok := smtpPath(arg, "FROM:")
			if !ok {
				if !write("501 expected MAIL FROM:<address>") {
					return
				}
				continue
			}
			from, recipients = address, nil
			if !write("250 sender accepted") {
				return
			}
		case "RCPT":
			address, ok := smtpPath(arg, "TO:")
			if !ok || from == "" {
				if !write("503 expected MAIL FROM before RCPT TO") {
					return
				}
				continue
			}
			recipients = append(recipients, mailsvc.Recipient{Address: address})
			if !write("250 recipient accepted") {
				return
			}
		case "DATA":
			if from == "" || len(recipients) == 0 {
				if !write("503 sender and recipient required") {
					return
				}
				continue
			}
			if !write("354 end with <CRLF>.<CRLF>") {
				return
			}
			body, err := io.ReadAll(io.LimitReader(textproto.NewReader(r).DotReader(), maxMessageSize+1))
			if err != nil || len(body) > maxMessageSize {
				from, recipients = "", nil
				if !write("552 message too large or malformed") {
					return
				}
				continue
			}
			if err := s.deliver(from, recipients, body); err != nil {
				if !write("451 message could not be delivered") {
					return
				}
			} else if !write("250 message accepted") {
				return
			}
			from, recipients = "", nil
		default:
			if !write("502 command not supported") {
				return
			}
		}
	}
}

func smtpPath(arg, prefix string) (string, bool) {
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", false
	}
	pathAndParams := strings.TrimSpace(arg[len(prefix):])
	if pathAndParams == "" {
		return "", false
	}
	fields := strings.Fields(pathAndParams)
	path := fields[0]
	if strings.HasPrefix(path, "<") {
		if !strings.HasSuffix(path, ">") {
			return "", false
		}
		path = path[1 : len(path)-1]
	}
	// ESMTP clients append parameters such as SIZE=1234 and BODY=8BITMIME to
	// MAIL FROM. They qualify the transaction; the sender path remains the
	// first argument and is all the Proton send operation needs.
	for _, parameter := range fields[1:] {
		if strings.ContainsAny(parameter, "<>\r\n") {
			return "", false
		}
	}
	if path == "" || strings.ContainsAny(path, "<>\r\n") {
		return "", false
	}
	return path, true
}

func (s *Server) deliver(from string, recipients []mailsvc.Recipient, raw []byte) error {
	parsed, err := mailsvc.ParseEML(bytes.NewReader(raw))
	if err != nil && !errors.Is(err, mailsvc.ErrEmptyEML) {
		slog.ErrorContext(s.ctx, "local SMTP message could not be parsed", "error", err)
		return err
	}
	sender, err := s.mail.ResolveSender(s.ctx, mailsvc.SenderRequest{Explicit: from})
	if err != nil {
		slog.ErrorContext(s.ctx, "local SMTP sender could not be resolved", "error", err)
		return err
	}
	content := mailsvc.Content{
		From: sender, To: recipients, Subject: parsed.Subject, Body: parsed.Body,
		HTML: parsed.HTML, Attach: parsed.Attachments,
	}
	_, err = s.mail.Send(s.ctx, content, mailsvc.Delivery{})
	if err != nil {
		slog.ErrorContext(s.ctx, "local SMTP message could not be sent through Proton", "error", err)
	}
	return err
}

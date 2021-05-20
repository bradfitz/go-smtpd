// Copyright 2011 The go-smtpd Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package smtpd implements an SMTP server. Hooks are provided to customize
// its behavior.
package smtpd

// TODO:
//  -- send 421 to connected clients on graceful server shutdown (s3.8)
//

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	rcptToRE   = regexp.MustCompile(`^[Tt][Oo]:<(.+)>`)
	mailFromRE = regexp.MustCompile(`^[Ff][Rr][Oo][Mm]:<([^>]*)>(.*)$`)
	mailSizeRE = regexp.MustCompile(`[Ss][Ii][Zz][Ee]=(\d+)`)
)

// Server is an SMTP server.
type Server struct {
	Addr         string        // TCP address to listen on, ":25" if empty
	Hostname     string        // optional Hostname to announce; "" to use system hostname
	ReadTimeout  time.Duration // optional read timeout
	WriteTimeout time.Duration // optional write timeout

	PlainAuth bool        // advertise plain auth (assumes you're on SSL)
	TLSConfig *tls.Config // advertise STARTTLS and use the given config to upgrade the connection with
	MaxSize   int         // maximum email size to report

	// OnNewConnection, if non-nil, is called on new connections.
	// If it returns non-nil, the connection is closed.
	OnNewConnection func(c Connection) error

	// OnNewMail must be defined and is called when a new message beings.
	// (when a MAIL FROM line arrives)
	OnNewMail func(c Connection, from MailAddress, size *int) (Envelope, error)

	// Log overrides standard logging
	// If nil, log.Printf is used
	Log func(format string, args ...interface{})
}

// MailAddress is defined by
type MailAddress interface {
	Email() string    // email address, as provided
	Hostname() string // canonical hostname, lowercase
}

// Connection is implemented by the SMTP library and provided to callers
// customizing their own Servers.
type Connection interface {
	Addr() net.Addr
	Close() error // to force-close a connection
}

type Envelope interface {
	AddRecipient(rcpt MailAddress) error
	BeginData() error
	Write(line []byte) error
	Close() error
}

type BasicEnvelope struct {
	rcpts []MailAddress
}

func (e *BasicEnvelope) AddRecipient(rcpt MailAddress) error {
	e.rcpts = append(e.rcpts, rcpt)
	return nil
}

func (e *BasicEnvelope) BeginData() error {
	if len(e.rcpts) == 0 {
		return SMTPError("554 5.5.1 Error: no valid recipients")
	}
	return nil
}

func (e *BasicEnvelope) Write(line []byte) error {
	fmt.Printf("Line: %q", string(line))
	return nil
}

func (e *BasicEnvelope) Close() error {
	return nil
}

func (srv *Server) log(format string, args ...interface{}) {
	if srv.Log != nil {
		srv.Log(format, args...)
		return
	}
	log.Printf("smtpd: "+format, args...)
}

func (srv *Server) hostname() string {
	if srv.Hostname != "" {
		return srv.Hostname
	}
	out, err := exec.Command("hostname").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls Serve to handle requests on incoming connections.  If
// srv.Addr is blank, ":25" is used.
func (srv *Server) ListenAndServe() error {
	addr := srv.Addr
	if addr == "" {
		addr = ":25"
	}
	ln, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	return srv.Serve(ln)
}

func (srv *Server) Serve(ln net.Listener) error {
	defer ln.Close()
	for {
		rw, e := ln.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				srv.log("Accept error: %v", e)
				continue
			}
			return e
		}
		sess, err := srv.newSession(rw)
		if err != nil {
			continue
		}
		go sess.serve()
	}
	panic("not reached")
}

type session struct {
	srv *Server
	rwc net.Conn
	br  *bufio.Reader
	bw  *bufio.Writer

	env Envelope // current envelope, or nil

	helloType string
	helloHost string
}

func (srv *Server) newSession(rwc net.Conn) (s *session, err error) {
	s = &session{
		srv: srv,
		rwc: rwc,
		br:  bufio.NewReader(rwc),
		bw:  bufio.NewWriter(rwc),
	}
	return
}

func (s *session) errorf(format string, args ...interface{}) {
	s.srv.log("Client error: "+format, args...)
}

func (s *session) sendf(format string, args ...interface{}) {
	if s.srv.WriteTimeout != 0 {
		s.rwc.SetWriteDeadline(time.Now().Add(s.srv.WriteTimeout))
	}
	fmt.Fprintf(s.bw, format, args...)
	s.bw.Flush()
}

func (s *session) sendlinef(format string, args ...interface{}) {
	s.sendf(format+"\r\n", args...)
}

func (s *session) sendSMTPErrorOrLinef(err error, format string, args ...interface{}) {
	if se, ok := err.(SMTPError); ok {
		s.sendlinef("%s", se.Error())
		return
	}
	s.sendlinef(format, args...)
}

func (s *session) Addr() net.Addr {
	return s.rwc.RemoteAddr()
}

func (s *session) Close() error { return s.rwc.Close() }

func (s *session) serve() {
	defer s.rwc.Close()
	if onc := s.srv.OnNewConnection; onc != nil {
		if err := onc(s); err != nil {
			s.sendSMTPErrorOrLinef(err, "554 connection rejected")
			return
		}
	}
	s.sendf("220 %s ESMTP gosmtpd\r\n", s.srv.hostname())
	for {
		if s.srv.ReadTimeout != 0 {
			s.rwc.SetReadDeadline(time.Now().Add(s.srv.ReadTimeout))
		}
		sl, err := s.br.ReadSlice('\n')
		if err != nil {
			s.errorf("read error: %v", err)
			return
		}
		line := cmdLine(string(sl))
		if err := line.checkValid(); err != nil {
			s.sendlinef("500 %v", err)
			continue
		}

		switch line.Verb() {
		case "HELO", "EHLO":
			s.handleHello(line.Verb(), line.Arg())
		case "STARTTLS":
			if s.srv.TLSConfig == nil {
				s.sendlinef("502 5.5.2 Error: command not recognized")
				continue
			}
			if err := s.handleStartTLS(); err != nil {
				s.errorf("failed to start tls: %s", err)
			}
		case "QUIT":
			s.sendlinef("221 2.0.0 Bye")
			return
		case "RSET":
			s.env = nil
			s.sendlinef("250 2.0.0 OK")
		case "NOOP":
			s.sendlinef("250 2.0.0 OK")
		case "MAIL":
			arg := line.Arg() // "From:<foo@bar.com>"
			m := mailFromRE.FindStringSubmatch(arg)
			if m == nil {
				s.srv.log("Invalid MAIL arg: %q", arg)
				s.sendlinef("501 5.1.7 Bad sender address syntax")
				continue
			}
			var size *int
			if len(m) == 3 && len(m[2]) > 0 {
				if sizeMatch := mailSizeRE.FindStringSubmatch(m[2]); sizeMatch != nil {
					parsedSize, err := strconv.Atoi(sizeMatch[1])
					if err != nil {
						s.sendlinef("501 5.5.4 Syntax error in parameters or arguments (invalid SIZE parameter)")
						continue
					}
					size = &parsedSize
				}
			}
			s.handleMailFrom(m[1], size)
		case "RCPT":
			s.handleRcpt(line)
		case "DATA":
			s.handleData()
		default:
			s.sendlinef("502 5.5.2 Error: command not recognized")
		}
	}
}

func (s *session) handleHello(greeting, host string) {
	s.helloType = greeting
	s.helloHost = host
	fmt.Fprintf(s.bw, "250-%s\r\n", s.srv.hostname())
	extensions := []string{}
	if s.srv.PlainAuth {
		extensions = append(extensions, "250-AUTH PLAIN")
	}
	if s.srv.TLSConfig != nil {
		extensions = append(extensions, "250-STARTTLS")
	}
	if s.srv.MaxSize != 0 {
		extensions = append(extensions, fmt.Sprintf("250-SIZE %d", s.srv.MaxSize))
	}
	extensions = append(extensions,
		"250-PIPELINING",
		"250-ENHANCEDSTATUSCODES",
		"250-8BITMIME",
		"250 DSN")
	for _, ext := range extensions {
		fmt.Fprintf(s.bw, "%s\r\n", ext)
	}
	s.bw.Flush()
}

func (s *session) handleStartTLS() error {
	s.sendlinef("220 Ready to start TLS")
	tlsConn := tls.Server(s.rwc, s.srv.TLSConfig)
	err := tlsConn.Handshake()
	if err != nil {
		s.sendSMTPErrorOrLinef(err, "403 4.7.0 TLS handshake failed")
		return err
	}
	s.rwc = net.Conn(tlsConn)
	s.bw.Reset(s.rwc)
	s.br.Reset(s.rwc)
	return nil
}

func (s *session) handleMailFrom(email string, size *int) {
	// TODO: 4.1.1.11.  If the server SMTP does not recognize or
	// cannot implement one or more of the parameters associated
	// qwith a particular MAIL FROM or RCPT TO command, it will return
	// code 555.

	if s.env != nil {
		s.sendlinef("503 5.5.1 Error: nested MAIL command")
		return
	}
	cb := s.srv.OnNewMail
	if cb == nil {
		panic("smtpd: Server.OnNewMail is nil")
	}
	s.env = nil
	env, err := cb(s, addrString(email), size)
	if err != nil {
		s.srv.log("Rejecting MAIL FROM %q: %v", email, err)
		s.sendf("451 denied\r\n")

		s.bw.Flush()
		time.Sleep(100 * time.Millisecond)
		s.rwc.Close()
		return
	}
	s.env = env
	s.sendlinef("250 2.1.0 Ok")
}

func (s *session) handleRcpt(line cmdLine) {
	// TODO: 4.1.1.11.  If the server SMTP does not recognize or
	// cannot implement one or more of the parameters associated
	// qwith a particular MAIL FROM or RCPT TO command, it will return
	// code 555.

	if s.env == nil {
		s.sendlinef("503 5.5.1 Error: need MAIL command")
		return
	}
	arg := line.Arg() // "To:<foo@bar.com>"
	m := rcptToRE.FindStringSubmatch(arg)
	if m == nil {
		s.srv.log("Bad RCPT address: %q", arg)
		s.sendlinef("501 5.1.7 Bad sender address syntax")
		return
	}
	err := s.env.AddRecipient(addrString(m[1]))
	if err != nil {
		s.sendSMTPErrorOrLinef(err, "550 bad recipient")
		return
	}
	s.sendlinef("250 2.1.0 Ok")
}

func (s *session) handleData() {
	if s.env == nil {
		s.sendlinef("503 5.5.1 Error: need RCPT command")
		return
	}
	if err := s.env.BeginData(); err != nil {
		s.handleError("BeginData", err)
		return
	}
	s.sendlinef("354 Go ahead")
	for {
		sl, err := s.br.ReadSlice('\n')
		if err != nil {
			s.errorf("read error: %v", err)
			return
		}
		if bytes.Equal(sl, []byte(".\r\n")) {
			break
		}
		if sl[0] == '.' {
			sl = sl[1:]
		}
		err = s.env.Write(sl)
		if err != nil {
			s.sendSMTPErrorOrLinef(err, "550 ??? failed")
			return
		}
	}
	if err := s.env.Close(); err != nil {
		s.handleError("Close", err)
		return
	}
	s.sendlinef("250 2.0.0 Ok: queued")
	s.env = nil
}

func (s *session) handleError(method string, err error) {
	if se, ok := err.(SMTPError); ok {
		s.sendlinef("%s", se)
		return
	}
	s.srv.log("Method %s returns error: %v", method, err)
	s.env = nil
}

type addrString string

func (a addrString) Email() string {
	return string(a)
}

func (a addrString) Hostname() string {
	e := string(a)
	if idx := strings.Index(e, "@"); idx != -1 {
		return strings.ToLower(e[idx+1:])
	}
	return ""
}

type cmdLine string

func (cl cmdLine) checkValid() error {
	if !strings.HasSuffix(string(cl), "\r\n") {
		return errors.New(`line doesn't end in \r\n`)
	}
	// Check for verbs defined not to have an argument
	// (RFC 5321 s4.1.1)
	switch cl.Verb() {
	case "RSET", "DATA", "QUIT":
		if cl.Arg() != "" {
			return errors.New("unexpected argument")
		}
	}
	return nil
}

func (cl cmdLine) Verb() string {
	s := string(cl)
	if idx := strings.Index(s, " "); idx != -1 {
		return strings.ToUpper(s[:idx])
	}
	return strings.ToUpper(s[:len(s)-2])
}

func (cl cmdLine) Arg() string {
	s := string(cl)
	if idx := strings.Index(s, " "); idx != -1 {
		return strings.TrimRightFunc(s[idx+1:len(s)-2], unicode.IsSpace)
	}
	return ""
}

func (cl cmdLine) String() string {
	return string(cl)
}

type SMTPError string

func (e SMTPError) Error() string {
	return string(e)
}

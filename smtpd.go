package main

import (
	"bufio"
	"exec"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
)

// Server is an SMTP server.
type Server struct {
	Addr         string // TCP address to listen on, ":25" if empty
	Hostname     string // optional Hostname to announce; "" to use system hostname
	ReadTimeout  int64  // optional net.Conn.SetReadTimeout value for new connections
	WriteTimeout int64  // optional net.Conn.SetWriteTimeout value for new connections

	// OnNewConnection, if non-nil, is called on new connections.
	// If it returns non-nil, the connection is closed.
	OnNewConnection func(c Connection) os.Error

	// OnNewMail must be defined and is called when a new message beings.
	// (when a MAIL FROM line arrives)
	OnNewMail func(c Connection, from MailAddress) (Envelope, os.Error)
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
}

type Envelope interface {
	AddRecipient(rcpt MailAddress) os.Error
}

// ArrivingMessage is the interface that must be implement by servers
// receiving mail.
type ArrivingMessage interface {
	AddHeaderLine(s string) os.Error
	EndHeaders() os.Error
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
func (srv *Server) ListenAndServe() os.Error {
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

func (srv *Server) Serve(ln net.Listener) os.Error {
	defer ln.Close()
	for {
		rw, e := ln.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				log.Printf("smtpd: Accept error: %v", e)
				continue
			}
			return e
		}
		if srv.ReadTimeout != 0 {
			rw.SetReadTimeout(srv.ReadTimeout)
		}
		if srv.WriteTimeout != 0 {
			rw.SetWriteTimeout(srv.WriteTimeout)
		}
		c, err := srv.newConn(rw)
		if err != nil {
			continue
		}
		go c.serve()
	}
	panic("not reached")
}

type conn struct {
	srv *Server
	rwc net.Conn
	br  *bufio.Reader
	bw  *bufio.Writer

	env Envelope // current envelope, or nil

	helloType string
	helloHost string
}

func (srv *Server) newConn(rwc net.Conn) (c *conn, err os.Error) {
	c = &conn{
		srv: srv,
		rwc: rwc,
		br:  bufio.NewReader(rwc),
		bw:  bufio.NewWriter(rwc),
	}
	return
}

func (c *conn) errorf(format string, args ...interface{}) {
	log.Printf("Client error: "+format, args...)
}

func (c *conn) sendf(format string, args ...interface{}) {
	fmt.Fprintf(c.bw, format, args...)
	c.bw.Flush()
}

func (c *conn) Addr() net.Addr {
	return c.rwc.RemoteAddr()
}

func (c *conn) serve() {
	defer c.rwc.Close()
	if onc := c.srv.OnNewConnection; onc != nil {
		if err := onc(c); err != nil {
			// TODO: if the error implements a SMTPErrorStringer,
			// call it and send the error back
			return
		}
	}
	c.sendf("220 %s ESMTP gosmtpd (Ajas)\r\n", c.srv.hostname())
	for {
		lines, err := c.br.ReadSlice('\n')
		if err != nil {
			c.errorf("read error: %v", err)
			return
		}
		line := string(lines)
		switch {
		case strings.HasSuffix(line, "\r\n"):
			line = line[:len(line)-2]
		case strings.HasSuffix(line, "\n"):
			line = line[:len(line)-1]
		default:
			c.errorf("received line not ending in newline: %q", line)
			return
		}
		prefix := func(s string) bool {
			return strings.HasPrefix(line, s)
		}
		log.Printf("Client: %q", line)
		switch {
		case prefix("HELO ") || prefix("EHLO "):
			c.handleHello(line[:4], line[5:])
		case prefix("MAIL FROM:<"):
			gt := strings.Index(line, ">")
			if gt == -1 {
				c.sendf("501 5.1.7 Bad sender address syntax\r\n")
				continue
			}
			c.handleMailFrom(line[len("MAIL FROM:<"):gt])
		case prefix("RCPT TO:<"):
			gt := strings.Index(line, ">")
			if gt == -1 {
				c.sendf("501 5.1.7 Bad sender address syntax\r\n")
				continue
			}
			c.handleRcptTo(line[len("RCPT TO:<"):gt])
		case line == "DATA":
			c.sendf("354 Go ahead\r\n")
		default:

		}
	}
}

func (c *conn) handleHello(greeting, host string) {
	c.helloType = greeting
	c.helloHost = host
	fmt.Fprintf(c.bw, "250-%s\r\n", c.srv.hostname())
	for _, ext := range []string{"250-PIPELINING", "250-SIZE 10240000", "250-ENHANCEDSTATUSCODES",
		"250-8BITMIME", "250 DSN"} {
		fmt.Fprintf(c.bw, "%s\r\n", ext)
	}
	c.bw.Flush()
}

func (c *conn) handleMailFrom(email string) {
	log.Printf("mail from: %q", email)
	cb := c.srv.OnNewMail
	if cb == nil {
		log.Printf("smtp: Server.OnNewMail is nil; rejecting MAIL FROM")
		c.sendf("451 Server.OnNewMail not configured\r\n")
		return
	}
	c.env = nil
	env, err := cb(c, addrString(email))
	if err != nil {
		// TODO: send it back to client if warranted, like above
		return
	}
	c.env = env
	c.sendf("250 2.1.0 Ok\r\n")
}

func (c *conn) handleRcptTo(email string) {
	log.Printf("rcpt to: %q", email)
	c.sendf("250 2.1.0 Ok\r\n")
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

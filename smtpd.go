package main

import (
	"bufio"
	"exec"
	"fmt"
	"log"
	"net"
	"os"
	"exp/regexp"
	"strings"
)

var (
	rcptToRE   = regexp.MustCompile(`(?i)^to:\s*<(.+?)>`)
	mailFromRE = regexp.MustCompile(`(?i)^from:\s*<(.+?)>`)
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

func (c *conn) sendlinef(format string, args ...interface{}) {
	c.sendf(format+"\r\n", args...)
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
	c.sendf("220 %s ESMTP gosmtpd (Gosmtpd)\r\n", c.srv.hostname())
	for {
		sl, err := c.br.ReadSlice('\n')
		if err != nil {
			c.errorf("read error: %v", err)
			return
		}
		line := cmdLine{line: string(sl)}
		if !line.valid() {
			c.sendlinef("500 ??? invalid line received, not ending in \\r\\n")
			return
		}
		log.Printf("Client: %q, verb: %q", line, line.Verb())
		switch line.Verb() {
		case "HELO", "EHLO":
			c.handleHello(line.Verb(), line.Arg())
		case "QUIT":
			c.sendlinef("221 2.0.0 Bye")
			return
		case "MAIL":
			arg := line.Arg() // "From:<foo@bar.com>"
			m := mailFromRE.FindStringSubmatch(arg)
			if m == nil {
				c.sendlinef("501 5.1.7 Bad sender address syntax")
				continue
			}
			c.handleMailFrom(m[1])
		case "RCPT":
			arg := line.Arg() // "To:<foo@bar.com>"
			m := rcptToRE.FindStringSubmatch(arg)
			if m == nil {
				c.sendlinef("501 5.1.7 Bad sender address syntax")
				continue
			}
			c.handleRcptTo(m[1])
		case "DATA":
			c.sendlinef("354 Go ahead")
		default:
			c.sendlinef("502 5.5.2 Error: command not recognized")
		}
	}
}

func (c *conn) handleHello(greeting, host string) {
	c.helloType = greeting
	c.helloHost = host
	fmt.Fprintf(c.bw, "250-%s\r\n", c.srv.hostname())
	for _, ext := range []string{
		"250-PIPELINING",
		"250-SIZE 10240000",
		"250-ENHANCEDSTATUSCODES",
		"250-8BITMIME",
		"250 DSN",
	} {
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

type cmdLine struct {
	line string
	verb string // lazily set
}

func (cl *cmdLine) valid() bool {
	return strings.HasSuffix(cl.line, "\r\n")
}

func (cl *cmdLine) Verb() string {
	if cl.verb == "" {
		if idx := strings.Index(cl.line, " "); idx != -1 {
			cl.verb = strings.ToUpper(cl.line[:idx])
		} else {
			cl.verb = strings.ToUpper(cl.line[:len(cl.line)-2])
		}
	}
	return cl.verb
}

func (cl *cmdLine) Arg() string {
	if idx := strings.Index(cl.line, " "); idx != -1 {
		return cl.line[idx+1 : len(cl.line)-2]
	}
	return ""
}

func (cl *cmdLine) String() string {
	if cl.valid() {
		return cl.line[:len(cl.line)-2]
	}
	return cl.line + "[!MISSING_NEWLINE]"
}

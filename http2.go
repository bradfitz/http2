// Copyright 2014 The Go Authors.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// Package http2 implements the HTTP/2 protocol.
//
// It currently targets draft-13. See http://http2.github.io/
package http2

import (
	"bytes"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"sync"
)

const (
	// ClientPreface is the string that must be sent by new
	// connections from clients.
	ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
)

var (
	clientPreface = []byte(ClientPreface)
)

const npnProto = "h2-13"

// Server is an HTTP2 server.
type Server struct {
	// MaxStreams optionally ...
	MaxStreams int

	mu sync.Mutex
}

func (srv *Server) handleClientConn(hs *http.Server, c *tls.Conn, h http.Handler) {
	cc := &clientConn{hs, c, h}
	cc.serve(srv.MaxStreams)
}

type clientConn struct {
	hs *http.Server
	c  *tls.Conn
	h  http.Handler
}

func (cc *clientConn) logf(format string, args ...interface{}) {
	if lg := cc.hs.ErrorLog; lg != nil {
		lg.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

func (cc *clientConn) serve(maxStreams int) {
	defer cc.c.Close()
	log.Printf("HTTP/2 connection from %v on %p", cc.c.RemoteAddr(), cc.hs)

	buf := make([]byte, len(ClientPreface))
	// TODO: timeout reading from the client
	if _, err := io.ReadFull(cc.c, buf); err != nil {
		cc.logf("error reading client preface: %v", err)
		return
	}
	if !bytes.Equal(buf, clientPreface) {
		cc.logf("bogus greeting from client: %q", buf)
		return
	}
	log.Printf("client %v said hello", cc.c.RemoteAddr())
	var frameReader = io.LimitedReader{
		R: cc.c,
	}
	s := &streamHandler{}
	for {
		// TODO(jmhodges): handle HeaderFrame sent on an open stream
		// (that is, one where a HeaderFrame has already been seen)
		fh, err := ReadFrameHeader(cc.c)
		if err != nil {
			if err != io.EOF {
				cc.logf("error reading frame: %v", err)
			}
			return
		}
		frameReader.N = int64(fh.Length)
		f, err := typeFrameParser(fh.Type)(fh, &frameReader)
		if h2e, ok := err.(Error); ok {
			if h2e.IsConnectionError() {
				log.Printf("Disconnection; connection error: %v", err)
				return
			}
			// TODO: stream errors, etc
		}
		if err != nil {
			log.Printf("Disconnection to other error: %v", err)
			return
		}
		err = s.Handle(f, &frameReader)
		if h2e, ok := err.(Error); ok {
			if h2e.IsConnectionError() {
				log.Printf("Disconnection from stream handler; connection error: %v", err)
				return
			}
			// TODO: stream errors, etc
		}
		if err != nil {
			// TODO: handle this error
			log.Printf("Outer error handling for %s", fh.Type)
		}
	}
}

// ConfigureServer adds HTTP2 support to s as configured by the HTTP/2
// server configuration in conf. The configuration may be nil.
//
// ConfigureServer must be called before s beings serving.
func ConfigureServer(s *http.Server, conf *Server) {
	if conf == nil {
		conf = new(Server)
	}
	if s.TLSConfig == nil {
		s.TLSConfig = new(tls.Config)
	}
	haveNPN := false
	for _, p := range s.TLSConfig.NextProtos {
		if p == npnProto {
			haveNPN = true
			break
		}
	}
	if !haveNPN {
		s.TLSConfig.NextProtos = append(s.TLSConfig.NextProtos, npnProto)
	}

	if s.TLSNextProto == nil {
		s.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	}
	s.TLSNextProto[npnProto] = func(hs *http.Server, c *tls.Conn, h http.Handler) {
		conf.handleClientConn(hs, c, h)
	}
}

// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

// TODO: replace all <-sc.conn.done with reads from the stream's cw
// instead, and make sure that on close we close all open
// streams. then remove conn.done?

// TODO: finish GOAWAY support. Consider each incoming frame type and
// whether it should be ignored during a shutdown race.

// TODO: disconnect idle clients. GFE seems to do 4 minutes. make
// configurable?  or maximum number of idle clients and remove the
// oldest?

// TODO: turn off the serve goroutine when idle, so
// an idle conn only has the readFrames goroutine active. (which could
// also be optimized probably to pin less memory in crypto/tls). This
// would involve tracking when the serve goroutine is active (atomic
// int32 read/CAS probably?) and starting it up when frames arrive,
// and shutting it down when all handlers exit. the occasional PING
// packets could use time.AfterFunc to call sc.wakeStartServeLoop()
// (which is a no-op if already running) and then queue the PING write
// as normal. The serve loop would then exit in most cases (if no
// Handlers running) and not be woken up again until the PING packet
// returns.

// TODO (maybe): add a mechanism for Handlers to going into
// half-closed-local mode (rw.(io.Closer) test?) but not exit their
// handler, and continue to be able to read from the
// Request.Body. This would be a somewhat semantic change from HTTP/1
// (or at least what we expose in net/http), so I'd probably want to
// add it there too. For now, this package says that returning from
// the Handler ServeHTTP function means you're both done reading and
// done writing, without a way to stop just one or the other.

package http2

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bradfitz/http2/hpack"
)

const (
	prefaceTimeout        = 10 * time.Second
	firstSettingsTimeout  = 2 * time.Second // should be in-flight with preface anyway
	handlerChunkWriteSize = 4 << 10
	defaultMaxStreams     = 250 // TODO: make this 100 as the GFE seems to?
)

var (
	errClientDisconnected = errors.New("client disconnected")
	errClosedBody         = errors.New("body closed by handler")
	errStreamBroken       = errors.New("http2: stream broken")
)

var responseWriterStatePool = sync.Pool{
	New: func() interface{} {
		rws := &responseWriterState{}
		rws.bw = bufio.NewWriterSize(chunkWriter{rws}, handlerChunkWriteSize)
		return rws
	},
}

// Test hooks.
var (
	testHookOnConn        func()
	testHookGetServerConn func(*serverConn)
	testHookOnPanicMu     *sync.Mutex // nil except in tests
	testHookOnPanic       func(c *conn, panicVal interface{}) (rePanic bool)
)

// Server is an HTTP/2 server.
type Server struct {
	// MaxHandlers limits the number of http.Handler ServeHTTP goroutines
	// which may run at a time over all connections.
	// Negative or zero no limit.
	// TODO: implement
	MaxHandlers int

	// MaxConcurrentStreams optionally specifies the number of
	// concurrent streams that each client may have open at a
	// time. This is unrelated to the number of http.Handler goroutines
	// which may be active globally, which is MaxHandlers.
	// If zero, MaxConcurrentStreams defaults to at least 100, per
	// the HTTP/2 spec's recommendations.
	MaxConcurrentStreams uint32

	// MaxReadFrameSize optionally specifies the largest frame
	// this server is willing to read. A valid value is between
	// 16k and 16M, inclusive. If zero or otherwise invalid, a
	// default value is used.
	MaxReadFrameSize uint32

	// PermitProhibitedCipherSuites, if true, permits the use of
	// cipher suites prohibited by the HTTP/2 spec.
	PermitProhibitedCipherSuites bool
}

func (s *Server) maxReadFrameSize() uint32 {
	if v := s.MaxReadFrameSize; v >= minMaxFrameSize && v <= maxFrameSize {
		return v
	}
	return defaultMaxReadFrameSize
}

func (s *Server) maxConcurrentStreams() uint32 {
	if v := s.MaxConcurrentStreams; v > 0 {
		return v
	}
	return defaultMaxStreams
}

// ConfigureServer adds HTTP/2 support to a net/http Server.
//
// The configuration conf may be nil.
//
// ConfigureServer must be called before s begins serving.
func ConfigureServer(s *http.Server, conf *Server) {
	if conf == nil {
		conf = new(Server)
	}
	if s.TLSConfig == nil {
		s.TLSConfig = new(tls.Config)
	}

	// Note: not setting MinVersion to tls.VersionTLS12,
	// as we don't want to interfere with HTTP/1.1 traffic
	// on the user's server. We enforce TLS 1.2 later once
	// we accept a connection. Ideally this should be done
	// during next-proto selection, but using TLS <1.2 with
	// HTTP/2 is still the client's bug.

	// Be sure we advertise tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
	// at least.
	// TODO: enable PreferServerCipherSuites?
	if s.TLSConfig.CipherSuites != nil {
		const requiredCipher = tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
		haveRequired := false
		for _, v := range s.TLSConfig.CipherSuites {
			if v == requiredCipher {
				haveRequired = true
				break
			}
		}
		if !haveRequired {
			s.TLSConfig.CipherSuites = append(s.TLSConfig.CipherSuites, requiredCipher)
		}
	}

	haveNPN := false
	for _, p := range s.TLSConfig.NextProtos {
		if p == NextProtoTLS {
			haveNPN = true
			break
		}
	}
	if !haveNPN {
		s.TLSConfig.NextProtos = append(s.TLSConfig.NextProtos, NextProtoTLS)
	}
	// h2-14 is temporary (as of 2015-03-05) while we wait for all browsers
	// to switch to "h2".
	s.TLSConfig.NextProtos = append(s.TLSConfig.NextProtos, "h2-14")

	if s.TLSNextProto == nil {
		s.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	}
	protoHandler := func(hs *http.Server, c *tls.Conn, h http.Handler) {
		if testHookOnConn != nil {
			testHookOnConn()
		}
		conf.handleConn(hs, c, h)
	}
	s.TLSNextProto[NextProtoTLS] = protoHandler
	s.TLSNextProto["h2-14"] = protoHandler // temporary; see above.
}

func (srv *Server) handleConn(hs *http.Server, c net.Conn, h http.Handler) {
	sc := &serverConn{
		srv:     srv,
		handler: h,
		conn: &conn{
			conn:             c,
			remoteAddrStr:    c.RemoteAddr().String(),
			bw:               newBufferedWriter(c),
			streams:          make(map[uint32]*stream),
			readFrameCh:      make(chan frameAndGate),
			readFrameErrCh:   make(chan error, 1), // must be buffered for 1
			wantWriteFrameCh: make(chan frameWriteMsg, 8),
			wroteFrameCh:     make(chan struct{}, 1), // buffered; one send in reading goroutine
			bodyReadCh:       make(chan bodyReadMsg), // buffering doesn't matter either way
			done:             make(chan struct{}),
			advMaxStreams:    srv.maxConcurrentStreams(),
			writeSched: writeScheduler{
				maxFrameSize: initialMaxFrameSize,
			},
			initialWindowSize: initialWindowSize,
			headerTableSize:   initialHeaderTableSize,
			runG:              newGoroutineLock(),
			pushEnabled:       true,
			logger:            hs.ErrorLog,
			maxReadFrameSize:  srv.maxReadFrameSize(),
			isClient:          false,
		},
	}
	sc.conn.onRequest = sc.runHandler // TODO(bgentry): remove this, untangle it
	// TODO(bgentry): move these to somewhere shared on the conn, probably along
	// with other conn constructor args above
	sc.conn.flow.add(initialWindowSize)
	sc.conn.inflow.add(initialWindowSize)
	sc.conn.hpackEncoder = hpack.NewEncoder(&sc.conn.headerWriteBuf)
	sc.conn.hpackDecoder = hpack.NewDecoder(initialHeaderTableSize, sc.conn.onNewHeaderField)

	fr := NewFramer(sc.conn.bw, c)
	fr.SetMaxReadFrameSize(srv.maxReadFrameSize())
	sc.conn.framer = fr

	if tc, ok := c.(*tls.Conn); ok {
		sc.conn.tlsState = new(tls.ConnectionState)
		*sc.conn.tlsState = tc.ConnectionState()
		// 9.2 Use of TLS Features
		// An implementation of HTTP/2 over TLS MUST use TLS
		// 1.2 or higher with the restrictions on feature set
		// and cipher suite described in this section. Due to
		// implementation limitations, it might not be
		// possible to fail TLS negotiation. An endpoint MUST
		// immediately terminate an HTTP/2 connection that
		// does not meet the TLS requirements described in
		// this section with a connection error (Section
		// 5.4.1) of type INADEQUATE_SECURITY.
		if sc.conn.tlsState.Version < tls.VersionTLS12 {
			sc.rejectConn(ErrCodeInadequateSecurity, "TLS version too low")
			return
		}

		if sc.conn.tlsState.ServerName == "" {
			// Client must use SNI, but we don't enforce that anymore,
			// since it was causing problems when connecting to bare IP
			// addresses during development.
			//
			// TODO: optionally enforce? Or enforce at the time we receive
			// a new request, and verify the the ServerName matches the :authority?
			// But that precludes proxy situations, perhaps.
			//
			// So for now, do nothing here again.
		}

		if !srv.PermitProhibitedCipherSuites && isBadCipher(sc.conn.tlsState.CipherSuite) {
			// "Endpoints MAY choose to generate a connection error
			// (Section 5.4.1) of type INADEQUATE_SECURITY if one of
			// the prohibited cipher suites are negotiated."
			//
			// We choose that. In my opinion, the spec is weak
			// here. It also says both parties must support at least
			// TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 so there's no
			// excuses here. If we really must, we could allow an
			// "AllowInsecureWeakCiphers" option on the server later.
			// Let's see how it plays out first.
			sc.rejectConn(ErrCodeInadequateSecurity, fmt.Sprintf("Prohibited TLS 1.2 Cipher Suite: %x", sc.conn.tlsState.CipherSuite))
			return
		}
	}

	if hook := testHookGetServerConn; hook != nil {
		hook(sc)
	}
	sc.conn.vlogf("HTTP/2 connection from %v on %p", sc.conn.conn.RemoteAddr(), hs)
	sc.conn.run()
}

// isBadCipher reports whether the cipher is blacklisted by the HTTP/2 spec.
func isBadCipher(cipher uint16) bool {
	switch cipher {
	case tls.TLS_RSA_WITH_RC4_128_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
		tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA:
		// Reject cipher suites from Appendix A.
		// "This list includes those cipher suites that do not
		// offer an ephemeral key exchange and those that are
		// based on the TLS null, stream or block cipher type"
		return true
	default:
		return false
	}
}

func (sc *serverConn) rejectConn(err ErrCode, debug string) {
	log.Printf("REJECTING conn: %v, %s", err, debug)
	// ignoring errors. hanging up anyway.
	sc.conn.framer.WriteGoAway(0, err, []byte(debug))
	sc.conn.bw.Flush()
	sc.conn.CloseConn()
}

// frameAndGates coordinates the readFrames and serve
// goroutines. Because the Framer interface only permits the most
// recently-read Frame from being accessed, the readFrames goroutine
// blocks until it has a frame, passes it to serve, and then waits for
// serve to be done with it before reading the next one.
type frameAndGate struct {
	f Frame
	g gate
}

type serverConn struct {
	// Immutable:
	conn    *conn
	srv     *Server
	handler http.Handler
}

// Run on its own goroutine.
func (sc *serverConn) runHandler(rw *responseWriter, req *http.Request) {
	defer rw.handlerDone()
	// TODO: catch panics like net/http.Server
	sc.handler.ServeHTTP(rw, req)
}

// responseWriter is the http.ResponseWriter implementation.  It's
// intentionally small (1 pointer wide) to minimize garbage.  The
// responseWriterState pointer inside is zeroed at the end of a
// request (in handlerDone) and calls on the responseWriter thereafter
// simply crash (caller's mistake), but the much larger responseWriterState
// and buffers are reused between multiple requests.
type responseWriter struct {
	rws *responseWriterState
}

// Optional http.ResponseWriter interfaces implemented.
var (
	_ http.CloseNotifier = (*responseWriter)(nil)
	_ http.Flusher       = (*responseWriter)(nil)
	_ stringWriter       = (*responseWriter)(nil)
)

type responseWriterState struct {
	// immutable within a request:
	stream *stream
	req    *http.Request
	body   *msgBody // to close at end of request, if DATA frames didn't
	conn   *conn

	// TODO: adjust buffer writing sizes based on server config, frame size updates from peer, etc
	bw *bufio.Writer // writing to a chunkWriter{this *responseWriterState}

	// mutated by http.Handler goroutine:
	handlerHeader http.Header // nil until called
	snapHeader    http.Header // snapshot of handlerHeader at WriteHeader time
	status        int         // status code passed to WriteHeader
	wroteHeader   bool        // WriteHeader called (explicitly or implicitly). Not necessarily sent to user yet.
	sentHeader    bool        // have we sent the header frame?
	handlerDone   bool        // handler has finished
	curWrite      writeData
	frameWriteCh  chan error // re-used whenever we need to block on a frame being written

	closeNotifierMu sync.Mutex // guards closeNotifierCh
	closeNotifierCh chan bool  // nil until first used
}

type chunkWriter struct{ rws *responseWriterState }

func (cw chunkWriter) Write(p []byte) (n int, err error) { return cw.rws.writeChunk(p) }

// writeChunk writes chunks from the bufio.Writer. But because
// bufio.Writer may bypass its chunking, sometimes p may be
// arbitrarily large.
//
// writeChunk is also responsible (on the first chunk) for sending the
// HEADER response.
func (rws *responseWriterState) writeChunk(p []byte) (n int, err error) {
	if !rws.wroteHeader {
		rws.writeHeader(200)
	}
	if !rws.sentHeader {
		rws.sentHeader = true
		var ctype, clen string // implicit ones, if we can calculate it
		if rws.handlerDone && rws.snapHeader.Get("Content-Length") == "" {
			clen = strconv.Itoa(len(p))
		}
		if rws.snapHeader.Get("Content-Type") == "" {
			ctype = http.DetectContentType(p)
		}
		endStream := rws.handlerDone && len(p) == 0
		rws.conn.writeHeadersFromExternal(rws.stream, &writeResHeaders{
			streamID:      rws.stream.id,
			httpResCode:   rws.status,
			h:             rws.snapHeader,
			endStream:     endStream,
			contentType:   ctype,
			contentLength: clen,
		}, rws.frameWriteCh)
		if endStream {
			return 0, nil
		}
	}
	if len(p) == 0 && !rws.handlerDone {
		return 0, nil
	}
	curWrite := &rws.curWrite
	curWrite.streamID = rws.stream.id
	curWrite.p = p
	curWrite.endStream = rws.handlerDone
	if err := rws.conn.writeDataFromExternal(rws.stream, curWrite, rws.frameWriteCh); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *responseWriter) Flush() {
	rws := w.rws
	if rws == nil {
		panic("Header called after Handler finished")
	}
	if rws.bw.Buffered() > 0 {
		if err := rws.bw.Flush(); err != nil {
			// Ignore the error. The frame writer already knows.
			return
		}
	} else {
		// The bufio.Writer won't call chunkWriter.Write
		// (writeChunk with zero bytes, so we have to do it
		// ourselves to force the HTTP response header and/or
		// final DATA frame (with END_STREAM) to be sent.
		rws.writeChunk(nil)
	}
}

func (w *responseWriter) CloseNotify() <-chan bool {
	rws := w.rws
	if rws == nil {
		panic("CloseNotify called after Handler finished")
	}
	rws.closeNotifierMu.Lock()
	ch := rws.closeNotifierCh
	if ch == nil {
		ch = make(chan bool, 1)
		rws.closeNotifierCh = ch
		go func() {
			rws.stream.cw.Wait() // wait for close
			ch <- true
		}()
	}
	rws.closeNotifierMu.Unlock()
	return ch
}

func (w *responseWriter) Header() http.Header {
	rws := w.rws
	if rws == nil {
		panic("Header called after Handler finished")
	}
	if rws.handlerHeader == nil {
		rws.handlerHeader = make(http.Header)
	}
	return rws.handlerHeader
}

func (w *responseWriter) WriteHeader(code int) {
	rws := w.rws
	if rws == nil {
		panic("WriteHeader called after Handler finished")
	}
	rws.writeHeader(code)
}

func (rws *responseWriterState) writeHeader(code int) {
	if !rws.wroteHeader {
		rws.wroteHeader = true
		rws.status = code
		if len(rws.handlerHeader) > 0 {
			rws.snapHeader = cloneHeader(rws.handlerHeader)
		}
	}
}

func cloneHeader(h http.Header) http.Header {
	h2 := make(http.Header, len(h))
	for k, vv := range h {
		vv2 := make([]string, len(vv))
		copy(vv2, vv)
		h2[k] = vv2
	}
	return h2
}

// The Life Of A Write is like this:
//
// * Handler calls w.Write or w.WriteString ->
// * -> rws.bw (*bufio.Writer) ->
// * (Handler might call Flush)
// * -> chunkWriter{rws}
// * -> responseWriterState.writeChunk(p []byte)
// * -> responseWriterState.writeChunk (most of the magic; see comment there)
func (w *responseWriter) Write(p []byte) (n int, err error) {
	return w.write(len(p), p, "")
}

func (w *responseWriter) WriteString(s string) (n int, err error) {
	return w.write(len(s), nil, s)
}

// either dataB or dataS is non-zero.
func (w *responseWriter) write(lenData int, dataB []byte, dataS string) (n int, err error) {
	rws := w.rws
	if rws == nil {
		panic("Write called after Handler finished")
	}
	if !rws.wroteHeader {
		w.WriteHeader(200)
	}
	if dataB != nil {
		return rws.bw.Write(dataB)
	} else {
		return rws.bw.WriteString(dataS)
	}
}

func (w *responseWriter) handlerDone() {
	rws := w.rws
	if rws == nil {
		panic("handlerDone called twice")
	}
	rws.handlerDone = true
	w.Flush()
	w.rws = nil
	responseWriterStatePool.Put(rws)
}

// Copyright 2015 The Go Authors.
// See https://go.googlesource.com/go/+/master/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://go.googlesource.com/go/+/master/LICENSE

package http2

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/bradfitz/http2/hpack"
)

type Transport struct {
	Fallback http.RoundTripper

	// TODO: remove this and make more general with a TLS dial hook, like http
	InsecureTLSDial bool

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

	connMu sync.Mutex
	conns  map[string][]*clientConn // key is host:port
}

func (t *Transport) maxReadFrameSize() uint32 {
	if v := t.MaxReadFrameSize; v >= minMaxFrameSize && v <= maxFrameSize {
		return v
	}
	return defaultMaxReadFrameSize
}

func (t *Transport) maxConcurrentStreams() uint32 {
	if v := t.MaxConcurrentStreams; v > 0 {
		return v
	}
	return defaultMaxStreams
}

type clientConn struct {
	t       *Transport
	conn    *conn
	connKey []string // key(s) this connection is cached in, in t.conns
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		if t.Fallback == nil {
			return nil, errors.New("http2: unsupported scheme and no Fallback")
		}
		return t.Fallback.RoundTrip(req)
	}

	host, port, err := net.SplitHostPort(req.URL.Host)
	if err != nil {
		host = req.URL.Host
		port = "443"
	}

	for {
		cc, err := t.getClientConn(host, port)
		if err != nil {
			return nil, err
		}
		res, err := cc.roundTrip(req)
		if shouldRetryRequest(err) { // TODO: or clientconn is overloaded (too many outstanding requests)?
			continue
		}
		if err != nil {
			return nil, err
		}
		return res, nil
	}
}

// CloseIdleConnections closes any connections which were previously
// connected from previous requests but are now sitting idle.
// It does not interrupt any connections currently in use.
func (t *Transport) CloseIdleConnections() {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	for _, vv := range t.conns {
		for _, cc := range vv {
			cc.closeIfIdle()
		}
	}
}

var errClientConnClosed = errors.New("http2: client conn is closed")

func shouldRetryRequest(err error) bool {
	// TODO: or GOAWAY graceful shutdown stuff
	return err == errClientConnClosed
}

func (t *Transport) removeClientConn(cc *clientConn) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	for _, key := range cc.connKey {
		vv, ok := t.conns[key]
		if !ok {
			continue
		}
		newList := filterOutClientConn(vv, cc)
		if len(newList) > 0 {
			t.conns[key] = newList
		} else {
			delete(t.conns, key)
		}
	}
}

func filterOutClientConn(in []*clientConn, exclude *clientConn) []*clientConn {
	out := in[:0]
	for _, v := range in {
		if v != exclude {
			out = append(out, v)
		}
	}
	return out
}

func (t *Transport) getClientConn(host, port string) (*clientConn, error) {
	t.connMu.Lock()
	defer t.connMu.Unlock()

	key := net.JoinHostPort(host, port)

	for _, cc := range t.conns[key] {
		// TODO(bgentry): this used to check cc.canTakeNewRequest() to check if
		// we're already at max # of streams, but current conn doesn't make that
		// available outside the run loop.
		return cc, nil
	}
	if t.conns == nil {
		t.conns = make(map[string][]*clientConn)
	}
	cc, err := t.newClientConn(host, port, key)
	if err != nil {
		return nil, err
	}
	t.conns[key] = append(t.conns[key], cc)
	go func() {
		defer t.removeClientConn(cc)

		select {
		case <-cc.conn.done:
		case gf := <-cc.conn.recvGoAwayCh:
			log.Printf("Transport received GoAway: %+v", gf)
		}
	}()
	return cc, nil
}

func (t *Transport) newClientConn(host, port, key string) (*clientConn, error) {
	cfg := &tls.Config{
		ServerName:         host,
		NextProtos:         []string{NextProtoTLS},
		InsecureSkipVerify: t.InsecureTLSDial,
	}
	tconn, err := tls.Dial("tcp", host+":"+port, cfg)
	if err != nil {
		return nil, err
	}
	if err := tconn.Handshake(); err != nil {
		return nil, err
	}
	if !t.InsecureTLSDial {
		if err := tconn.VerifyHostname(cfg.ServerName); err != nil {
			return nil, err
		}
	}
	state := tconn.ConnectionState()
	if p := state.NegotiatedProtocol; p != NextProtoTLS {
		// TODO(bradfitz): fall back to Fallback
		return nil, fmt.Errorf("bad protocol: %v", p)
	}
	if !state.NegotiatedProtocolIsMutual {
		return nil, errors.New("could not negotiate protocol mutually")
	}

	cc := &clientConn{
		t: t,
		conn: &conn{
			conn:             tconn,
			tlsState:         &state,
			remoteAddrStr:    tconn.RemoteAddr().String(),
			bw:               newBufferedWriter(tconn),
			streams:          make(map[uint32]*stream),
			readFrameCh:      make(chan frameAndGate),
			readFrameErrCh:   make(chan error, 1), // must be buffered for 1
			wantWriteFrameCh: make(chan frameWriteMsg, 8),
			wroteFrameCh:     make(chan struct{}, 1), // buffered; one send in reading goroutine
			bodyReadCh:       make(chan bodyReadMsg), // buffering doesn't matter either way
			done:             make(chan struct{}),
			advMaxStreams:    1000, // "infinite", per spec. 1000 seems good enough.
			writeSched: writeScheduler{
				maxFrameSize: initialMaxFrameSize, // spec default
			},
			initialWindowSize: initialWindowSize, // spec default
			headerTableSize:   initialHeaderTableSize,
			pushEnabled:       true,
			logger:            nil,
			maxReadFrameSize:  t.maxReadFrameSize(),
			nextStreamID:      1,
			isClient:          true,
			recvGoAwayCh:      make(chan *GoAwayFrame, 1),
		},
		connKey: []string{key}, // TODO: cert's validated hostnames too
	}
	// TODO(bgentry): move these to somewhere shared on the conn, probably along
	// with other conn constructor args above
	cc.conn.flow.add(initialWindowSize)
	cc.conn.inflow.add(initialWindowSize)
	// TODO: figure out henc size
	cc.conn.hpackEncoder = hpack.NewEncoder(&cc.conn.headerWriteBuf)
	cc.conn.hpackDecoder = hpack.NewDecoder(initialHeaderTableSize, cc.conn.onNewHeaderField)

	fr := NewFramer(cc.conn.bw, tconn)
	fr.SetMaxReadFrameSize(cc.conn.maxReadFrameSize)
	cc.conn.framer = fr

	go func() {
		cc.conn.runG = newGoroutineLock()
		cc.conn.run()
	}()
	return cc, nil
}

func (cc *clientConn) closeIfIdle() {
	// TODO: do clients send GOAWAY too? maybe? Just Close:
	// TODO(bgentry): fix this so it doesn't just always close everything!
	cc.conn.CloseConn()
}

func (cc *clientConn) roundTrip(req *http.Request) (*http.Response, error) {
	hasBody := req.Body != nil
	wrh := &writeReqHeaders{
		host:      req.Host,
		method:    req.Method,
		path:      req.URL.Path,
		h:         req.Header,
		endStream: !hasBody,
		// TODO(bgentry): wrh.contentType, wrh.contentLength
	}
	if wrh.host == "" {
		wrh.host = req.URL.Host
	}
	if wrh.path == "" {
		wrh.path = "/"
	}
	resc := make(chan resAndError, 1)
	// TODO(bgentry): recycle ch by tracking them in clientConn / Transport
	ch := make(chan error, 1)
	st, err := cc.conn.createStreamFromExternal(wrh, resc, ch)
	if err != nil {
		return nil, err
	}
	cc.conn.vlogf("created stream %d", st.id)

	if hasBody {
		defer req.Body.Close()
		w := dataFrameWriter{
			conn:         cc.conn,
			st:           st,
			curWrite:     writeData{streamID: st.id},
			frameWriteCh: make(chan error, 1),
		}
		_, err := io.Copy(w, req.Body)
		w.Close()
		if err != nil {
			return nil, err
		}
	}

	re := <-resc
	if re.err != nil {
		return nil, re.err
	}
	res := re.res
	res.Request = req
	return res, nil
}

type resAndError struct {
	res *http.Response
	err error
}

type dataFrameWriter struct {
	conn         *conn
	st           *stream
	curWrite     writeData
	frameWriteCh chan error
}

// Write sends a data frame with the contents of b.
func (df dataFrameWriter) Write(p []byte) (int, error) {
	df.curWrite.p = p
	return df.writeFrame()
}

// Close sends an empty data frame with the EndStream flag set, closing the
// stream.
func (df dataFrameWriter) Close() error {
	df.curWrite.endStream = true
	df.curWrite.p = nil
	_, err := df.writeFrame()
	return err
}

func (df dataFrameWriter) writeFrame() (int, error) {
	n := len(df.curWrite.p)
	if err := df.conn.writeDataFromExternal(df.st, &df.curWrite, df.frameWriteCh); err != nil {
		return 0, err
	}
	return n, nil
}

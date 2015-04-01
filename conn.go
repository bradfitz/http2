package http2

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bradfitz/http2/hpack"
)

// msgParam is the state of the next message (request or response),
// initialized over potentially several frames HEADERS + zero or more
// CONTINUATION frames.
type msgParam struct {
	// stream is non-nil if we're reading (HEADER or CONTINUATION)
	// frames for a message (but not DATA).
	stream            *stream
	parent            *stream // for push promises
	header            http.Header
	method, path      string
	scheme, authority string
	status            int  // for responses
	sawRegularHeader  bool // saw a non-pseudo header already
	invalidHeader     bool // an invalid header was seen
	isPushPromise     bool
}

// stream represents a stream. This is the minimal metadata needed by
// the serve goroutine. Most of the actual stream state is owned by
// the http.Handler's goroutine in the responseWriter. Because the
// responseWriter's responseWriterState is recycled at the end of a
// handler, this struct intentionally has no pointer to the
// *responseWriter{,State} itself, as the Handler ending nils out the
// responseWriter's state field.
type stream struct {
	// immutable:
	id   uint32
	body *pipe              // non-nil if expecting DATA frames
	cw   closeWaiter        // closed wait stream transitions to closed state
	resc chan<- resAndError // non-nil if expecting response

	// owned by conn's run loop:
	bodyBytes     int64   // body bytes seen so far
	declBodyBytes int64   // or -1 if undeclared
	flow          flow    // limits writing to peer
	inflow        flow    // what the peer is allowed to write to us
	parent        *stream // or nil
	weight        uint8
	state         streamState
	sentReset     bool // only true once detached from streams map
	gotReset      bool // only true once detacted from streams map
}

type conn struct {
	// Immutable:
	conn             net.Conn
	bw               *bufferedWriter // writing to conn
	framer           *Framer
	hpackDecoder     *hpack.Decoder
	done             chan struct{}     // closed when conn.run ends
	readFrameCh      chan frameAndGate // written by conn.readFrames
	readFrameErrCh   chan error
	wantWriteFrameCh chan frameWriteMsg   // from handlers or transport -> run
	wroteFrameCh     chan struct{}        // from writeFrameAsync -> run, tickles more frame writes
	bodyReadCh       chan bodyReadMsg     // from handlers or transport -> run
	testHookCh       chan func()          // code to run on the run loop
	flow             flow                 // conn-wide (not stream-specific) outbound flow control
	inflow           flow                 // conn-wide inbound flow control
	tlsState         *tls.ConnectionState // shared for entire conn, like net/http
	remoteAddrStr    string
	logger           *log.Logger
	maxReadFrameSize uint32
	isClient         bool                                 // indicates whether the conn is a client conn or server conn
	onRequest        func(*responseWriter, *http.Request) // server only
	// if recvGoAwayCh is non-nil, GoAway frames received are sent here
	recvGoAwayCh chan *GoAwayFrame

	// Everything following is owned by the serve loop; use runG.check():
	runG                  goroutineLock // used to verify funcs are on run()
	pushEnabled           bool
	sawFirstSettings      bool // got the initial SETTINGS frame after the preface
	needToSendSettingsAck bool
	unackedSettings       int    // how many SETTINGS have we sent without ACKs?
	peerMaxStreams        uint32 // SETTINGS_MAX_CONCURRENT_STREAMS from peer (PUSH_PROMISE or outbound request limit)
	advMaxStreams         uint32 // our SETTINGS_MAX_CONCURRENT_STREAMS advertised to the peer
	curOpenStreams        uint32 // peer's number of open streams
	maxStreamID           uint32 // max ever seen
	nextStreamID          uint32 // ID for next stream we create
	streams               map[uint32]*stream
	initialWindowSize     int32
	headerTableSize       uint32
	maxHeaderListSize     uint32            // zero means unknown (default)
	canonHeader           map[string]string // http2-lower-case -> Go-Canonical-Case
	msg                   msgParam          // non-zero while reading message headers
	writingFrame          bool              // started write goroutine but haven't heard back on wroteFrameCh
	needsFrameFlush       bool              // last frame write wasn't a flush
	writeSched            writeScheduler
	inGoAway              bool // we've started to or sent GOAWAY
	needToSendGoAway      bool // we need to schedule a GOAWAY frame write
	goAwayCode            ErrCode
	recvGoAway            *GoAwayFrame
	shutdownTimerCh       <-chan time.Time // nil until used
	shutdownTimer         *time.Timer      // nil until used

	// Owned by the writeFrameAsync goroutine:
	headerWriteBuf bytes.Buffer
	hpackEncoder   *hpack.Encoder
}

// runs the HTTP/2 loop. Prefaces will be read and verified
func (c *conn) run() {
	defer c.notePanic()
	defer c.conn.Close()
	defer c.closeAllStreamsOnConnClose()
	defer c.stopShutdownTimer()
	defer close(c.done) // unblocks handlers or transport trying to send

	if err := c.writePreface(); err != nil {
		c.condlogf(err, "error writing preface %v: %v", c.conn.RemoteAddr(), err)
		return
	}

	if c.isClient {
		c.sendWindowUpdate(nil, 1<<30) // um, 0x7fffffff doesn't work to Google? it hangs?
	} else {
		if err := c.readClientPreface(); err != nil {
			c.condlogf(err, "error reading client preface %v: %v", c.conn.RemoteAddr(), err)
			return
		}
	}
	settingsTimer := time.NewTimer(firstSettingsTimeout)
	go c.readFrames() // closed by defer c.conn.Close above

	for {
		select {
		case wm := <-c.wantWriteFrameCh:
			c.writeFrame(wm)
		case <-c.wroteFrameCh:
			c.writingFrame = false
			c.scheduleFrameWrite()
		case fg, ok := <-c.readFrameCh:
			if !ok {
				c.readFrameCh = nil
			}
			if !c.processFrameFromReader(fg, ok) {
				return
			}
			if settingsTimer.C != nil {
				settingsTimer.Stop()
				settingsTimer.C = nil
			}
		case m := <-c.bodyReadCh:
			c.noteBodyRead(m.st, m.n)
		case <-settingsTimer.C:
			c.logf("timeout waiting for SETTINGS frames from %v", c.conn.RemoteAddr())
			return
		case <-c.shutdownTimerCh:
			c.vlogf("GOAWAY close timer fired; closing conn from %v", c.conn.RemoteAddr())
			return
		case fn := <-c.testHookCh:
			fn()
		}
	}
}

// readClientPreface reads the ClientPreface greeting from the peer
// or returns an error on timeout or an invalid greeting.
func (c *conn) readClientPreface() error {
	errc := make(chan error, 1)
	go func() {
		// Read the client preface
		buf := make([]byte, len(clientPreface))
		if _, err := io.ReadFull(c.conn, buf); err != nil {
			errc <- err
		} else if !bytes.Equal(buf, clientPreface) {
			errc <- fmt.Errorf("bogus greeting %q", buf)
		} else {
			errc <- nil
		}
	}()
	timer := time.NewTimer(prefaceTimeout) // TODO: configurable on *Server?
	defer timer.Stop()
	select {
	case <-timer.C:
		return errors.New("timeout waiting for client preface")
	case err := <-errc:
		if err == nil {
			c.vlogf("client %v said hello", c.conn.RemoteAddr())
		}
		return err
	}
}

func (c *conn) writePreface() error {
	if c.isClient {
		if _, err := c.bw.Write(clientPreface); err != nil {
			return err
		}
	}
	c.writeFrame(frameWriteMsg{
		write: writeSettings{
			{SettingMaxFrameSize, c.maxReadFrameSize},
			{SettingMaxConcurrentStreams, c.advMaxStreams},

			// TODO: more actual settings, notably
			// SettingInitialWindowSize, but then we also
			// want to bump up the conn window size the
			// same amount here right after the settings
		},
	})
	c.unackedSettings++
	return nil
}

// writeDataFromExternal writes the data described in req to stream.id.
//
// The provided ch is used to avoid allocating new channels for each
// write operation. It's expected that the caller reuses writeData and ch
// over time.
//
// The flow control currently happens in the Handler where it waits
// for 1 or more bytes to be available to then write here.  So at this
// point we know that we have flow control. But this might have to
// change when priority is implemented, so the serve goroutine knows
// the total amount of bytes waiting to be sent and can can have more
// scheduling decisions available.
func (c *conn) writeDataFromExternal(stream *stream, writeData *writeData, ch chan error) error {
	c.writeFrameFromExternal(frameWriteMsg{
		write:  writeData,
		stream: stream,
		done:   ch,
	})
	select {
	case err := <-ch:
		return err
	case <-c.done:
		return errClientDisconnected
	case <-stream.cw:
		return errStreamBroken
	}
}

// writeFrameFromExternal sends wm to c.wantWriteFrameCh, but aborts
// if the connection has gone away.
//
// This must not be run from the serve goroutine itself, else it might
// deadlock writing to c.wantWriteFrameCh (which is only mildly
// buffered and is read by serve itself). If you're on the serve
// goroutine, call writeFrame instead.
func (c *conn) writeFrameFromExternal(wm frameWriteMsg) {
	c.runG.checkNotOn() // NOT
	select {
	case c.wantWriteFrameCh <- wm:
	case <-c.done:
		// Client has closed their connection to the server.
	}
}

// called from transport goroutines (and maybe handlers for PUSH_PROMISE).
func (c *conn) createStreamFromExternal(headerData *writeReqHeaders, resc chan resAndError, ch chan error) (*stream, error) {
	st := &stream{resc: resc}
	st.cw.Init()
	c.writeFrameFromExternal(frameWriteMsg{
		write:  headerData,
		stream: st,
		done:   ch,
	})
	select {
	case err := <-ch:
		return st, err
	case <-c.done:
		return nil, errClientDisconnected
	}
}

// called from handler goroutines.
// h may be nil.
func (c *conn) writeHeadersFromExternal(st *stream, headerData *writeResHeaders, tempCh chan error) {
	c.runG.checkNotOn() // NOT on
	var errc chan error
	if headerData.h != nil {
		// If there's a header map (which we don't own), so we have to block on
		// waiting for this frame to be written, so an http.Flush mid-handler
		// writes out the correct value of keys, before a handler later potentially
		// mutates it.
		errc = tempCh
	}
	c.writeFrameFromExternal(frameWriteMsg{
		write:  headerData,
		stream: st,
		done:   errc,
	})
	if errc != nil {
		select {
		case <-errc:
			// Ignore. Just for synchronization.
			// Any error will be handled in the writing goroutine.
		case <-c.done:
			// Client has closed the connection.
		}
	}
}

// called from handler goroutines.
func (c *conn) write100ContinueHeaders(st *stream) {
	c.writeFrameFromExternal(frameWriteMsg{
		write:  write100ContinueHeadersFrame{st.id},
		stream: st,
	})
}

// called from handler goroutines.
// Notes that the handler for the given stream ID read n bytes of its body
// and schedules flow control tokens to be sent.
func (c *conn) noteBodyReadFromExternal(st *stream, n int) {
	c.runG.checkNotOn() // NOT on
	c.bodyReadCh <- bodyReadMsg{st, n}
}

func (c *conn) Framer() *Framer  { return c.framer }
func (c *conn) CloseConn() error { return c.conn.Close() }
func (c *conn) Flush() error     { return c.bw.Flush() }
func (c *conn) HeaderEncoder() (*hpack.Encoder, *bytes.Buffer) {
	return c.hpackEncoder, &c.headerWriteBuf
}

func (c *conn) state(streamID uint32) (streamState, *stream) {
	c.runG.check()
	// http://http2.github.io/http2-spec/#rfc.section.5.1
	if st, ok := c.streams[streamID]; ok {
		return st.state, st
	}
	// "The first use of a new stream identifier implicitly closes all
	// streams in the "idle" state that might have been initiated by
	// that peer with a lower-valued stream identifier. For example, if
	// a client sends a HEADERS frame on stream 7 without ever sending a
	// frame on stream 5, then stream 5 transitions to the "closed"
	// state when the first frame for stream 7 is sent or received."
	if streamID <= c.maxStreamID {
		return stateClosed, nil
	}
	return stateIdle, nil
}

func (c *conn) vlogf(format string, args ...interface{}) {
	if VerboseLogs {
		c.logf(format, args...)
	}
}

func (c *conn) logf(format string, args ...interface{}) {
	if lg := c.logger; lg != nil {
		lg.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

func (c *conn) condlogf(err error, format string, args ...interface{}) {
	if err == nil {
		return
	}
	str := err.Error()
	if err == io.EOF || strings.Contains(str, "use of closed network connection") {
		// Boring, expected errors.
		c.vlogf(format, args...)
	} else {
		c.logf(format, args...)
	}
}

func (c *conn) onNewHeaderField(f hpack.HeaderField) {
	c.runG.check()
	c.vlogf("got header field %+v", f)
	switch {
	case !validHeader(f.Name):
		c.msg.invalidHeader = true
	case strings.HasPrefix(f.Name, ":"):
		if c.msg.sawRegularHeader {
			c.logf("pseudo-header after regular header")
			c.msg.invalidHeader = true
			return
		}
		// If we're a client and this is not a push promise, treat it as a response.
		if c.isClient && !c.msg.isPushPromise {
			if f.Name != ":status" {
				c.logf("invalid pseudo-header %q", f.Name)
				c.msg.invalidHeader = true
				return
			}
			if c.msg.status != 0 {
				c.logf("duplicate pseudo-header %q sent", f.Name)
				c.msg.invalidHeader = true
				return
			}
			var err error
			if c.msg.status, err = strconv.Atoi(f.Value); err != nil {
				c.logf("invalid :status header %q sent: %s", f.Value, err)
				c.msg.invalidHeader = true
			}
			return
		}

		var dst *string
		switch f.Name {
		case ":method":
			dst = &c.msg.method
		case ":path":
			dst = &c.msg.path
		case ":scheme":
			dst = &c.msg.scheme
		case ":authority":
			dst = &c.msg.authority
		default:
			// 8.1.2.1 Pseudo-Header Fields
			// "Endpoints MUST treat a request or response
			// that contains undefined or invalid
			// pseudo-header fields as malformed (Section
			// 8.1.2.6)."
			c.logf("invalid pseudo-header %q", f.Name)
			c.msg.invalidHeader = true
			return
		}
		if *dst != "" {
			c.logf("duplicate pseudo-header %q sent", f.Name)
			c.msg.invalidHeader = true
			return
		}
		*dst = f.Value
	case f.Name == "cookie":
		c.msg.sawRegularHeader = true
		if s, ok := c.msg.header["Cookie"]; ok && len(s) == 1 {
			s[0] = s[0] + "; " + f.Value
		} else {
			c.msg.header.Add("Cookie", f.Value)
		}
	default:
		c.msg.sawRegularHeader = true
		c.msg.header.Add(c.canonicalHeader(f.Name), f.Value)
	}
}

func (c *conn) canonicalHeader(v string) string {
	c.runG.check()
	cv, ok := commonCanonHeader[v]
	if ok {
		return cv
	}
	cv, ok = c.canonHeader[v]
	if ok {
		return cv
	}
	if c.canonHeader == nil {
		c.canonHeader = make(map[string]string)
	}
	cv = http.CanonicalHeaderKey(v)
	c.canonHeader[v] = cv
	return cv
}

// readFrames is the loop that reads incoming frames.
// It's run on its own goroutine.
func (c *conn) readFrames() {
	g := make(gate, 1)
	for {
		f, err := c.framer.ReadFrame()
		if err != nil {
			c.readFrameErrCh <- err
			close(c.readFrameCh)
			return
		}
		c.readFrameCh <- frameAndGate{f, g}
		// We can't read another frame until this one is
		// processed, as the ReadFrame interface doesn't copy
		// memory.  The Frame accessor methods access the last
		// frame's (shared) buffer. So we wait for the
		// serve goroutine to tell us it's done:
		g.Wait()
	}
}

// writeFrameAsync runs in its own goroutine and writes a single frame
// and then reports when it's done.
// At most one goroutine can be running writeFrameAsync at a time per
// conn.
func (c *conn) writeFrameAsync(wm frameWriteMsg) {
	err := wm.write.writeFrame(c)
	if ch := wm.done; ch != nil {
		select {
		case ch <- err:
		default:
			panic(fmt.Sprintf("unbuffered done channel passed in for type %T", wm.write))
		}
	}
	c.wroteFrameCh <- struct{}{} // tickle frame selection scheduler
}

func (c *conn) closeAllStreamsOnConnClose() {
	c.runG.check()
	for _, st := range c.streams {
		c.closeStream(st, errClientDisconnected)
	}
}

func (c *conn) stopShutdownTimer() {
	c.runG.check()
	if t := c.shutdownTimer; t != nil {
		t.Stop()
	}
}

func (c *conn) notePanic() {
	if testHookOnPanicMu != nil {
		testHookOnPanicMu.Lock()
		defer testHookOnPanicMu.Unlock()
	}
	if testHookOnPanic != nil {
		if e := recover(); e != nil {
			if testHookOnPanic(c, e) {
				panic(e)
			}
		}
	}
}

// writeFrame schedules a frame to write and sends it if there's nothing
// already being written.
//
// There is no pushback here (the serve goroutine never blocks). It's
// the http.Handlers that block, waiting for their previous frames to
// make it onto the wire
//
// If you're not on the serve goroutine, use writeFrameFromExternal instead.
func (c *conn) writeFrame(wm frameWriteMsg) {
	c.runG.check()
	c.writeSched.add(wm)
	c.scheduleFrameWrite()
}

// startFrameWrite starts a goroutine to write wm (in a separate
// goroutine since that might block on the network), and updates the
// serve goroutine's state about the world, updated from info in wm.
func (c *conn) startFrameWrite(wm frameWriteMsg) {
	c.runG.check()
	if c.writingFrame {
		panic("internal error: can only be writing one frame at a time")
	}

	st := wm.stream
	if st != nil {
		if st.id == 0 { // create a new stream
			st.state = stateOpen
			st.id = c.nextStreamID
			wm.stream = st
			c.nextStreamID += 2
			c.initializeFlowControl(st)
			c.streams[st.id] = st

			switch w := wm.write.(type) {
			case *writeReqHeaders:
				w.streamID = st.id
			case *writePushPromise:
				w.streamID = st.id
				// A stream in the "reserved (local)" state is one that
				// has been promised by sending a PUSH_PROMISE frame. A
				// PUSH_PROMISE frame reserves an idle stream by
				// associating the stream with an open stream that was
				// initiated by the remote peer.
				st.state = stateResvLocal
			default:
				panic(fmt.Sprintf("unhandled frame type for stream create: %v", w))
			}
		}

		switch st.state {
		case stateHalfClosedLocal:
			// A stream that is in the "half closed (local)" state
			// cannot be used for sending frames other than
			// WINDOW_UPDATE, PRIORITY and RST_STREAM.
			switch wm.write.(type) {
			case writeWindowUpdate, writePriority, StreamError:
			default:
				panic("internal error: attempt to send frame on half-closed-local stream")
			}
		case stateClosed:
			if st.sentReset || st.gotReset {
				// Skip this frame. But fake the frame write to reschedule:
				c.wroteFrameCh <- struct{}{}
				return
			}
			panic(fmt.Sprintf("internal error: attempt to send a write %v on a closed stream", wm))
		}

		if c.recvGoAway != nil && st.id > c.recvGoAway.LastStreamID {
			wm.done <- errClientConnClosed
			return
		}
	}

	c.writingFrame = true
	c.needsFrameFlush = true
	if endsStream(wm.write) {
		if st == nil {
			panic("internal error: expecting non-nil stream")
		}
		switch st.state {
		case stateOpen:
			st.state = stateHalfClosedLocal // won't last long, but necessary for closeStream via resetStream
			if !c.isClient {
				// Here we would go to stateHalfClosedLocal in
				// theory, but since our handler is done and
				// the net/http package provides no mechanism
				// for finishing writing to a ResponseWriter
				// while still reading data (see possible TODO
				// at top of this file), we go into closed
				// state here anyway, after telling the peer
				// we're hanging up on them.
				errCancel := StreamError{st.id, ErrCodeCancel}
				c.resetStream(errCancel)
			}
		case stateHalfClosedRemote:
			c.closeStream(st, nil)
		}
	}
	go c.writeFrameAsync(wm)
}

// scheduleFrameWrite tickles the frame writing scheduler.
//
// If a frame is already being written, nothing happens. This will be called again
// when the frame is done being written.
//
// If a frame isn't being written we need to send one, the best frame
// to send is selected, preferring first things that aren't
// stream-specific (e.g. ACKing settings), and then finding the
// highest priority stream.
//
// If a frame isn't being written and there's nothing else to send, we
// flush the write buffer.
func (c *conn) scheduleFrameWrite() {
	c.runG.check()
	if c.writingFrame {
		return
	}
	if c.needToSendGoAway {
		c.needToSendGoAway = false
		c.startFrameWrite(frameWriteMsg{
			write: &writeGoAway{
				maxStreamID: c.maxStreamID,
				code:        c.goAwayCode,
			},
		})
		return
	}
	if c.needToSendSettingsAck {
		c.needToSendSettingsAck = false
		c.startFrameWrite(frameWriteMsg{write: writeSettingsAck{}})
		return
	}
	if !c.inGoAway {
		if wm, ok := c.writeSched.take(); ok {
			c.startFrameWrite(wm)
			return
		}
	}
	if c.needsFrameFlush {
		c.startFrameWrite(frameWriteMsg{write: flushFrameWriter{}})
		c.needsFrameFlush = false // after startFrameWrite, since it sets this true
		return
	}
}

func (c *conn) goAway(code ErrCode) {
	c.runG.check()
	if c.inGoAway {
		return
	}
	if code != ErrCodeNo {
		c.shutDownIn(250 * time.Millisecond)
	} else {
		// TODO: configurable
		c.shutDownIn(1 * time.Second)
	}
	c.inGoAway = true
	c.needToSendGoAway = true
	c.goAwayCode = code
	c.scheduleFrameWrite()
}

func (c *conn) shutDownIn(d time.Duration) {
	c.runG.check()
	c.shutdownTimer = time.NewTimer(d)
	c.shutdownTimerCh = c.shutdownTimer.C
}

func (c *conn) resetStream(se StreamError) {
	c.runG.check()
	c.writeFrame(frameWriteMsg{write: se})
	if st, ok := c.streams[se.StreamID]; ok {
		st.sentReset = true
		c.closeStream(st, se)
	}
}

// curHeaderStreamID returns the stream ID of the header block we're
// currently in the middle of reading. If this returns non-zero, the
// next frame must be a CONTINUATION with this stream id.
func (c *conn) curHeaderStreamID() uint32 {
	c.runG.check()
	st := c.msg.stream
	if st == nil {
		return 0
	}
	return st.id
}

// processFrameFromReader processes the run loop's read from readFrameCh from the
// frame-reading goroutine.
// processFrameFromReader returns whether the connection should be kept open.
func (c *conn) processFrameFromReader(fg frameAndGate, fgValid bool) bool {
	c.runG.check()
	var peerGone bool
	var err error
	if !fgValid {
		err = <-c.readFrameErrCh
		if err == ErrFrameTooLarge {
			c.goAway(ErrCodeFrameSize)
			return true // goAway will close the loop
		}
		peerGone = err == io.EOF || strings.Contains(err.Error(), "use of closed network connection")
		if peerGone {
			// TODO: could we also get into this state if
			// the peer does a half close
			// (e.g. CloseWrite) because they're done
			// sending frames but they're still wanting
			// our open replies?  Investigate.
			// TODO: add CloseWrite to crypto/tls.Conn first
			// so we have a way to test this? I suppose
			// just for testing we could have a non-TLS mode.
			return false
		}
	}

	if fgValid {
		f := fg.f
		c.vlogf("client=%t got %v: %#v", c.isClient, f.Header(), f)
		err = c.processFrame(f)
		fg.g.Done() // unblock the readFrames goroutine
		if err == nil {
			return true
		}
	}

	switch ev := err.(type) {
	case StreamError:
		c.resetStream(ev)
		return true
	case goAwayFlowError:
		c.goAway(ErrCodeFlowControl)
		return true
	case ConnectionError:
		c.logf("%v: %v", c.conn.RemoteAddr(), ev)
		c.goAway(ErrCode(ev))
		return true // goAway will handle shutdown
	default:
		if !fgValid {
			c.logf("disconnecting; error reading frame from peer %s: %v", c.conn.RemoteAddr(), err)
		} else {
			c.logf("disconnection due to other error: %v", err)
		}
	}
	return false
}

func (c *conn) processFrame(f Frame) error {
	c.runG.check()

	// First frame received must be SETTINGS.
	if !c.sawFirstSettings {
		if _, ok := f.(*SettingsFrame); !ok {
			return ConnectionError(ErrCodeProtocol)
		}
		c.sawFirstSettings = true
	}

	if s := c.curHeaderStreamID(); s != 0 {
		if cf, ok := f.(*ContinuationFrame); !ok {
			return ConnectionError(ErrCodeProtocol)
		} else if cf.Header().StreamID != s {
			return ConnectionError(ErrCodeProtocol)
		}
	}

	switch f := f.(type) {
	case *SettingsFrame:
		return c.processSettings(f)
	case *GoAwayFrame:
		return c.processGoAway(f)
	case *HeadersFrame:
		return c.processHeaders(f)
	case *ContinuationFrame:
		return c.processContinuation(f)
	case *WindowUpdateFrame:
		return c.processWindowUpdate(f)
	case *PingFrame:
		return c.processPing(f)
	case *DataFrame:
		return c.processData(f)
	case *RSTStreamFrame:
		return c.processResetStream(f)
	case *PriorityFrame:
		return c.processPriority(f)
	case *PushPromiseFrame:
		// TODO(bgentry): allow these frames for clients. Restrict other frame
		// types where appropriate.

		// A client cannot push. Thus, servers MUST treat the receipt of a PUSH_PROMISE
		// frame as a connection error (Section 5.4.1) of type PROTOCOL_ERROR.
		return ConnectionError(ErrCodeProtocol)
	default:
		log.Printf("Ignoring frame: %v", f.Header())
		return nil
	}
}

func (c *conn) processPing(f *PingFrame) error {
	c.runG.check()
	if f.Flags.Has(FlagSettingsAck) {
		// 6.7 PING: " An endpoint MUST NOT respond to PING frames
		// containing this flag."
		return nil
	}
	if f.StreamID != 0 {
		// "PING frames are not associated with any individual
		// stream. If a PING frame is received with a stream
		// identifier field value other than 0x0, the recipient MUST
		// respond with a connection error (Section 5.4.1) of type
		// PROTOCOL_ERROR."
		return ConnectionError(ErrCodeProtocol)
	}
	c.writeFrame(frameWriteMsg{write: writePingAck{f}})
	return nil
}

func (c *conn) processWindowUpdate(f *WindowUpdateFrame) error {
	c.runG.check()
	switch {
	case f.StreamID != 0: // stream-level flow control
		st := c.streams[f.StreamID]
		if st == nil {
			// "WINDOW_UPDATE can be sent by a peer that has sent a
			// frame bearing the END_STREAM flag. This means that a
			// receiver could receive a WINDOW_UPDATE frame on a "half
			// closed (remote)" or "closed" stream. A receiver MUST
			// NOT treat this as an error, see Section 5.1."
			return nil
		}
		if !st.flow.add(int32(f.Increment)) {
			return StreamError{f.StreamID, ErrCodeFlowControl}
		}
	default: // connection-level flow control
		if !c.flow.add(int32(f.Increment)) {
			return goAwayFlowError{}
		}
	}
	c.scheduleFrameWrite()
	return nil
}

func (c *conn) processResetStream(f *RSTStreamFrame) error {
	c.runG.check()

	state, st := c.state(f.StreamID)
	if state == stateIdle {
		// 6.4 "RST_STREAM frames MUST NOT be sent for a
		// stream in the "idle" state. If a RST_STREAM frame
		// identifying an idle stream is received, the
		// recipient MUST treat this as a connection error
		// (Section 5.4.1) of type PROTOCOL_ERROR.
		return ConnectionError(ErrCodeProtocol)
	}
	if st != nil {
		st.gotReset = true
		c.closeStream(st, StreamError{f.StreamID, f.ErrCode})
	}
	return nil
}

func (c *conn) closeStream(st *stream, err error) {
	c.runG.check()
	if st.state == stateIdle || st.state == stateClosed {
		panic(fmt.Sprintf("invariant; can't close stream in state %v", st.state))
	}
	st.state = stateClosed
	c.curOpenStreams--
	delete(c.streams, st.id)
	if p := st.body; p != nil {
		p.Close(err)
	}
	if ch := st.resc; ch != nil {
		select {
		case ch <- resAndError{nil, err}:
		default:
			panic(fmt.Sprintf("unbuffered done channel passed in for resc on stream %d", st.id))
		}
	}
	st.cw.Close() // signals Handler's CloseNotifier, unblocks writes, etc
	c.writeSched.forgetStream(st.id)
}

func (c *conn) processSettings(f *SettingsFrame) error {
	c.runG.check()
	if f.IsAck() {
		c.unackedSettings--
		if c.unackedSettings < 0 {
			// Why is the peer ACKing settings we never sent?
			// The spec doesn't mention this case, but
			// hang up on them anyway.
			return ConnectionError(ErrCodeProtocol)
		}
		return nil
	}
	if err := f.ForeachSetting(c.processSetting); err != nil {
		return err
	}
	c.needToSendSettingsAck = true
	c.scheduleFrameWrite()
	return nil
}

func (c *conn) processSetting(s Setting) error {
	c.runG.check()
	if err := s.Valid(); err != nil {
		return err
	}
	c.vlogf("processing setting %v", s)
	switch s.ID {
	case SettingHeaderTableSize:
		c.headerTableSize = s.Val
		c.hpackEncoder.SetMaxDynamicTableSize(s.Val)
	case SettingEnablePush:
		c.pushEnabled = s.Val != 0
	case SettingMaxConcurrentStreams:
		c.peerMaxStreams = s.Val
	case SettingInitialWindowSize:
		return c.processSettingInitialWindowSize(s.Val)
	case SettingMaxFrameSize:
		c.writeSched.maxFrameSize = s.Val
	case SettingMaxHeaderListSize:
		c.maxHeaderListSize = s.Val
	default:
		// Unknown setting: "An endpoint that receives a SETTINGS
		// frame with any unknown or unsupported identifier MUST
		// ignore that setting."
	}
	return nil
}

func (c *conn) processSettingInitialWindowSize(val uint32) error {
	c.runG.check()
	// Note: val already validated to be within range by
	// processSetting's Valid call.

	// "A SETTINGS frame can alter the initial flow control window
	// size for all current streams. When the value of
	// SETTINGS_INITIAL_WINDOW_SIZE changes, a receiver MUST
	// adjust the size of all stream flow control windows that it
	// maintains by the difference between the new value and the
	// old value."
	old := c.initialWindowSize
	c.initialWindowSize = int32(val)
	growth := c.initialWindowSize - old // may be negative
	for _, st := range c.streams {
		if !st.flow.add(growth) {
			// 6.9.2 Initial Flow Control Window Size
			// "An endpoint MUST treat a change to
			// SETTINGS_INITIAL_WINDOW_SIZE that causes any flow
			// control window to exceed the maximum size as a
			// connection error (Section 5.4.1) of type
			// FLOW_CONTROL_ERROR."
			return ConnectionError(ErrCodeFlowControl)
		}
	}
	return nil
}

func (c *conn) processData(f *DataFrame) error {
	c.runG.check()
	// "If a DATA frame is received whose stream is not in "open"
	// or "half closed (local)" state, the recipient MUST respond
	// with a stream error (Section 5.4.2) of type STREAM_CLOSED."
	id := f.Header().StreamID
	st, ok := c.streams[id]
	if !ok || !(st.state == stateOpen || (c.isClient && st.state == stateHalfClosedLocal)) {
		// This includes sending a RST_STREAM if the stream is
		// in stateHalfClosedLocal (which currently means that
		// the http.Handler returned, so it's done reading &
		// done writing). Try to stop the client from sending
		// more DATA.
		return StreamError{id, ErrCodeStreamClosed}
	}
	if st.body == nil {
		panic("internal error: should have a body in this state")
	}
	data := f.Data()

	// Sender sending more than they'd declared?
	if st.declBodyBytes != -1 && st.bodyBytes+int64(len(data)) > st.declBodyBytes {
		st.body.Close(fmt.Errorf("sender tried to send more than declared Content-Length of %d bytes", st.declBodyBytes))
		return StreamError{id, ErrCodeStreamClosed}
	}
	if len(data) > 0 {
		// Check whether the peer has flow control quota.
		if int(st.inflow.available()) < len(data) {
			return StreamError{id, ErrCodeFlowControl}
		}
		st.inflow.take(int32(len(data)))
		wrote, err := st.body.Write(data)
		if err != nil {
			return StreamError{id, ErrCodeStreamClosed}
		}
		if wrote != len(data) {
			panic("internal error: bad Writer")
		}
		st.bodyBytes += int64(len(data))
	}
	if f.StreamEnded() {
		if st.declBodyBytes != -1 && st.declBodyBytes != st.bodyBytes {
			msgType := "request"
			if c.isClient {
				msgType = "response"
			}
			st.body.Close(fmt.Errorf("%s declared a Content-Length of %d but only wrote %d bytes",
				msgType, st.declBodyBytes, st.bodyBytes))
		} else {
			st.body.Close(io.EOF)
		}
		st.state = stateHalfClosedRemote
	}
	return nil
}

func (c *conn) processGoAway(f *GoAwayFrame) error {
	c.runG.check()
	// The GOAWAY frame (type=0x7) informs the remote peer to stop creating
	// streams on this connection. GOAWAY can be sent by either the client or the
	// server. Once sent, the sender will ignore frames sent on any new streams
	// with identifiers higher than the included last stream identifier. Receivers
	// of a GOAWAY frame MUST NOT open additional streams on the connection,
	// although a new connection can be established for new streams.
	c.recvGoAway = f
	for _, st := range c.streams {
		if st.id > f.LastStreamID {
			if f.ErrCode == ErrCodeNo {
				c.closeStream(st, errClientConnClosed)
			} else {
				c.closeStream(st, ConnectionError(f.ErrCode))
			}
		}
	}

	if ch := c.recvGoAwayCh; ch != nil {
		select {
		case ch <- f:
		default:
			panic("unbuffered recvGoAwayCh passed in for conn")
		}
	}
	if f.ErrCode != 0 {
		// TODO: deal with GOAWAY more. particularly the error code
		c.logf("conn got GOAWAY with error code = %v", f.ErrCode)
	}
	return nil
}

func (c *conn) processHeaders(f *HeadersFrame) error {
	c.runG.check()
	id := f.Header().StreamID
	if c.inGoAway {
		// Ignore.
		return nil
	}

	// http://http2.github.io/http2-spec/#rfc.section.5.1.1
	if (!c.isClient && (id%2 != 1 || c.msg.stream != nil)) ||
		(c.isClient && id%2 != 1 && !c.msg.isPushPromise) ||
		id <= c.maxStreamID ||
		id == 0 {
		// Streams initiated by a client MUST use odd-numbered
		// stream identifiers; those initiated by the server MUST
		// use even-numbered stream identifiers. [...] the stream
		// identifier zero cannot be used to establish a new stream.
		// The identifier of a newly established stream MUST be
		// numerically greater than all streams that the initiating
		// endpoint has opened or reserved. [...] An endpoint that
		// receives an unexpected stream identifier MUST respond
		// with a connection error (Section 5.4.1) of type
		// PROTOCOL_ERROR.
		return ConnectionError(ErrCodeProtocol)
	}
	// We should already know about this stream ID if this is a
	// client receiving a response to a previous request or
	// PUSH_PROMISE. Verify that it's a known stream ID if we're a
	// client.
	var st *stream
	if c.isClient {
		var ok bool
		st, ok = c.streams[id]
		if !ok || !(st.state == stateOpen || st.state == stateHalfClosedLocal) {
			c.logf("Received frame for untracked stream ID %d", id)
			return ConnectionError(ErrCodeProtocol)
		}
	} else {
		st = &stream{
			id:    id,
			state: stateOpen,
		}
		st.cw.Init()
	}
	c.maxStreamID = id
	if f.StreamEnded() {
		st.state = stateHalfClosedRemote
	}
	c.initializeFlowControl(st)
	c.streams[id] = st
	if f.HasPriority() {
		adjustStreamPriority(c.streams, st.id, f.Priority)
	}
	c.curOpenStreams++
	c.msg = msgParam{
		stream: st,
		header: make(http.Header),
	}
	return c.processHeaderBlockFragment(st, f.HeaderBlockFragment(), f.HeadersEnded())
}

func (c *conn) initializeFlowControl(st *stream) {
	c.runG.check()
	st.flow.conn = &c.flow // link to conn-level counter
	st.flow.add(c.initialWindowSize)
	st.inflow.conn = &c.inflow       // link to conn-level counter
	st.inflow.add(initialWindowSize) // TODO: update this when we send a higher initial window size in the initial settings
}

func (c *conn) processContinuation(f *ContinuationFrame) error {
	c.runG.check()
	st := c.streams[f.Header().StreamID]
	if st == nil || c.curHeaderStreamID() != st.id {
		return ConnectionError(ErrCodeProtocol)
	}
	return c.processHeaderBlockFragment(st, f.HeaderBlockFragment(), f.HeadersEnded())
}

func (c *conn) processHeaderBlockFragment(st *stream, frag []byte, end bool) error {
	c.runG.check()
	if _, err := c.hpackDecoder.Write(frag); err != nil {
		// TODO: convert to stream error I assume?
		return err
	}
	if !end {
		return nil
	}
	if err := c.hpackDecoder.Close(); err != nil {
		// TODO: convert to stream error I assume?
		return err
	}

	defer c.resetPendingMessage()
	if c.curOpenStreams > c.advMaxStreams {
		// "Endpoints MUST NOT exceed the limit set by their
		// peer. An endpoint that receives a HEADERS frame
		// that causes their advertised concurrent stream
		// limit to be exceeded MUST treat this as a stream
		// error (Section 5.4.2) of type PROTOCOL_ERROR or
		// REFUSED_STREAM."
		if c.unackedSettings == 0 {
			// They should know better.
			return StreamError{st.id, ErrCodeProtocol}
		}
		// Assume it's a network race, where they just haven't
		// received our last SETTINGS update. But actually
		// this can't happen yet, because we don't yet provide
		// a way for users to adjust server parameters at
		// runtime.
		return StreamError{st.id, ErrCodeRefusedStream}
	}

	if c.isClient && !c.msg.isPushPromise {
		res, err := c.newResponse()
		if st.resc != nil {
			st.resc <- resAndError{res, err}
		}
		st.body = res.Body.(*requestBody).pipe // may be nil
		st.declBodyBytes = res.ContentLength
		return err
	}

	rw, req, err := c.newWriterAndRequest()
	if err != nil {
		return err
	}
	st.body = req.Body.(*requestBody).pipe // may be nil
	st.declBodyBytes = req.ContentLength
	go c.onRequest(rw, req)
	return nil
}

func (c *conn) processPriority(f *PriorityFrame) error {
	adjustStreamPriority(c.streams, f.StreamID, f.PriorityParam)
	return nil
}

func adjustStreamPriority(streams map[uint32]*stream, streamID uint32, priority PriorityParam) {
	st, ok := streams[streamID]
	if !ok {
		// TODO: not quite correct (this streamID might
		// already exist in the dep tree, but be closed), but
		// close enough for now.
		return
	}
	st.weight = priority.Weight
	parent := streams[priority.StreamDep] // might be nil
	if parent == st {
		// if peer tries to set this stream to be the parent of itself
		// ignore and keep going
		return
	}

	// section 5.3.3: If a stream is made dependent on one of its
	// own dependencies, the formerly dependent stream is first
	// moved to be dependent on the reprioritized stream's previous
	// parent. The moved dependency retains its weight.
	for piter := parent; piter != nil; piter = piter.parent {
		if piter == st {
			parent.parent = st.parent
			break
		}
	}
	st.parent = parent
	if priority.Exclusive && (st.parent != nil || priority.StreamDep == 0) {
		for _, openStream := range streams {
			if openStream != st && openStream.parent == st.parent {
				openStream.parent = st
			}
		}
	}
}

// resetPendingMessage zeros out all state related to a HEADERS frame
// and its zero or more CONTINUATION frames sent to start a new
// message (request or response).
func (c *conn) resetPendingMessage() {
	c.runG.check()
	c.msg = msgParam{}
}

func (c *conn) newResponse() (*http.Response, error) {
	c.runG.check()
	rp := &c.msg
	if rp.invalidHeader || rp.status == 0 {
		// For HTTP/2 responses, a single :status pseudo-header field
		// is defined that carries the HTTP status code field. This
		// pseudo-header field MUST be included in all responses,
		// otherwise the response is malformed (Section 8.1.2.6).
		return nil, StreamError{rp.stream.id, ErrCodeProtocol}
	}
	state := rp.stream.state
	bodyOpen := (state == stateOpen || state == stateHalfClosedLocal)
	body := &requestBody{
		conn:   c,
		stream: rp.stream,
	}
	res := &http.Response{
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
		Header:     rp.header,
		StatusCode: rp.status,
		Status:     httpCodeString(rp.status) + " " + http.StatusText(rp.status),
		TLS:        c.tlsState,
		Body:       body,
	}
	if bodyOpen {
		body.pipe = &pipe{
			b: buffer{buf: make([]byte, initialWindowSize)}, // TODO: share/remove XXX
		}
		body.pipe.c.L = &body.pipe.m
		if vv, ok := rp.header["Content-Length"]; ok {
			res.ContentLength, _ = strconv.ParseInt(vv[0], 10, 64)
		} else {
			res.ContentLength = -1
		}
	}
	return res, nil
}

func (c *conn) newWriterAndRequest() (*responseWriter, *http.Request, error) {
	c.runG.check()
	rp := &c.msg
	if rp.invalidHeader || rp.method == "" || rp.path == "" ||
		(rp.scheme != "https" && rp.scheme != "http") {
		// See 8.1.2.6 Malformed Requests and Responses:
		//
		// Malformed requests or responses that are detected
		// MUST be treated as a stream error (Section 5.4.2)
		// of type PROTOCOL_ERROR."
		//
		// 8.1.2.3 Request Pseudo-Header Fields
		// "All HTTP/2 requests MUST include exactly one valid
		// value for the :method, :scheme, and :path
		// pseudo-header fields"
		return nil, nil, StreamError{rp.stream.id, ErrCodeProtocol}
	}
	var tlsState *tls.ConnectionState // nil if not scheme https
	if rp.scheme == "https" {
		tlsState = c.tlsState
	}
	authority := rp.authority
	if authority == "" {
		authority = rp.header.Get("Host")
	}
	needsContinue := rp.header.Get("Expect") == "100-continue"
	if needsContinue {
		rp.header.Del("Expect")
	}
	bodyOpen := rp.stream.state == stateOpen
	body := &requestBody{
		conn:          c,
		stream:        rp.stream,
		needsContinue: needsContinue,
	}
	// TODO: handle asterisk '*' requests + test
	url, err := url.ParseRequestURI(rp.path)
	if err != nil {
		// TODO: find the right error code?
		return nil, nil, StreamError{rp.stream.id, ErrCodeProtocol}
	}
	req := &http.Request{
		Method:     rp.method,
		URL:        url,
		RemoteAddr: c.remoteAddrStr,
		Header:     rp.header,
		RequestURI: rp.path,
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
		ProtoMinor: 0,
		TLS:        tlsState,
		Host:       authority,
		Body:       body,
	}
	if bodyOpen {
		body.pipe = &pipe{
			b: buffer{buf: make([]byte, initialWindowSize)}, // TODO: share/remove XXX
		}
		body.pipe.c.L = &body.pipe.m

		if vv, ok := rp.header["Content-Length"]; ok {
			req.ContentLength, _ = strconv.ParseInt(vv[0], 10, 64)
		} else {
			req.ContentLength = -1
		}
	}

	rws := responseWriterStatePool.Get().(*responseWriterState)
	bwSave := rws.bw
	*rws = responseWriterState{} // zero all the fields
	rws.conn = c
	rws.bw = bwSave
	rws.bw.Reset(chunkWriter{rws})
	rws.stream = rp.stream
	rws.req = req
	rws.body = body
	rws.frameWriteCh = make(chan error, 1)

	rw := &responseWriter{rws: rws}
	return rw, req, nil
}

// A bodyReadMsg tells the run loop that the http.Handler or Client
// read n bytes of the DATA from the peer on the given stream.
type bodyReadMsg struct {
	st *stream
	n  int
}

func (c *conn) noteBodyRead(st *stream, n int) {
	c.runG.check()
	c.sendWindowUpdate(nil, n) // conn-level
	if st.state != stateHalfClosedRemote && st.state != stateClosed {
		// Don't send this WINDOW_UPDATE if the stream is closed
		// remotely.
		c.sendWindowUpdate(st, n)
	}
}

// st may be nil for conn-level
func (c *conn) sendWindowUpdate(st *stream, n int) {
	c.runG.check()
	// "The legal range for the increment to the flow control
	// window is 1 to 2^31-1 (2,147,483,647) octets."
	// A Go Read call on 64-bit machines could in theory read
	// a larger Read than this. Very unlikely, but we handle it here
	// rather than elsewhere for now.
	const maxUint31 = 1<<31 - 1
	for n >= maxUint31 {
		c.sendWindowUpdate32(st, maxUint31)
		n -= maxUint31
	}
	c.sendWindowUpdate32(st, int32(n))
}

// st may be nil for conn-level
func (c *conn) sendWindowUpdate32(st *stream, n int32) {
	c.runG.check()
	if n == 0 {
		return
	}
	if n < 0 {
		panic("negative update")
	}
	var streamID uint32
	if st != nil {
		streamID = st.id
	}
	c.writeFrame(frameWriteMsg{
		write:  writeWindowUpdate{streamID: streamID, n: uint32(n)},
		stream: st,
	})
	var ok bool
	if st == nil {
		ok = c.inflow.add(n)
	} else {
		ok = st.inflow.add(n)
	}
	if !ok {
		panic("internal error; sent too many window updates without decrements?")
	}
}

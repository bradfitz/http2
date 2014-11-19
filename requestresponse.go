// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// requestParam is the state of the next request, initialized over
// potentially several frames HEADERS + zero or more CONTINUATION
// frames.
type requestParam struct {
	// stream is non-nil if we're reading (HEADER or CONTINUATION)
	// frames for a request (but not DATA).
	stream            *stream
	header            http.Header
	method, path      string
	scheme, authority string
	sawRegularHeader  bool // saw a non-pseudo header already
	invalidHeader     bool // an invalid header was seen
}

var responseWriterStatePool = sync.Pool{
	New: func() interface{} {
		rws := &responseWriterState{}
		rws.bw = bufio.NewWriterSize(chunkWriter{rws}, handlerChunkWriteSize)
		return rws
	},
}

type requestBody struct {
	sc            *serverConn
	stream        *stream
	closed        bool
	pipe          *pipe // non-nil if we have a HTTP entity message body
	needsContinue bool  // need to send a 100-continue
}

var errClosedBody = errors.New("body closed by handler")

func (b *requestBody) Close() error {
	if b.pipe != nil {
		b.pipe.Close(errClosedBody)
	}
	b.closed = true
	return nil
}

func (b *requestBody) Read(p []byte) (n int, err error) {
	if b.needsContinue {
		b.needsContinue = false
		b.sc.write100ContinueHeaders(b.stream)
	}
	if b.pipe == nil {
		return 0, io.EOF
	}
	n, err = b.pipe.Read(p)
	if n > 0 {
		b.sc.sendWindowUpdate(b.stream, n)
		// TODO: tell b.sc to send back 'n' flow control quota credits to the sender
	}
	return
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
	// TODO: hijacker for websockets?
)

type responseWriterState struct {
	// immutable within a request:
	stream *stream
	req    *http.Request
	body   *requestBody // to close at end of request, if DATA frames didn't

	// TODO: adjust buffer writing sizes based on server config, frame size updates from peer, etc
	bw *bufio.Writer // writing to a chunkWriter{this *responseWriterState}

	// mutated by http.Handler goroutine:
	handlerHeader http.Header // nil until called
	snapHeader    http.Header // snapshot of handlerHeader at WriteHeader time
	wroteHeader   bool        // WriteHeader called (explicitly or implicitly). Not necessarily sent to user yet.
	status        int         // status code passed to WriteHeader
	sentHeader    bool        // have we sent the header frame?
	handlerDone   bool        // handler has finished

	curChunk        []byte // current chunk we're writing
	curChunkIsFinal bool
	chunkWrittenCh  chan error

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
		rws.stream.conn.writeHeaders(headerWriteReq{
			stream:        rws.stream,
			httpResCode:   rws.status,
			h:             rws.snapHeader,
			endStream:     endStream,
			contentType:   ctype,
			contentLength: clen,
		})
		if endStream {
			return
		}
	}
	if len(p) == 0 {
		if rws.handlerDone {
			rws.curChunk = nil
			rws.curChunkIsFinal = true
			rws.stream.conn.writeFrame(frameWriteMsg{
				write:     (*serverConn).writeDataFrame,
				cost:      0,
				stream:    rws.stream,
				endStream: true,
				v:         rws, // writeDataInLoop uses only rws.curChunk and rws.curChunkIsFinal
			})
		}
		return
	}
	for len(p) > 0 {
		chunk := p
		if len(chunk) > handlerChunkWriteSize {
			chunk = chunk[:handlerChunkWriteSize]
		}
		p = p[len(chunk):]
		rws.curChunk = chunk
		rws.curChunkIsFinal = rws.handlerDone && len(p) == 0

		// TODO: await flow control tokens for both stream and conn
		rws.stream.conn.writeFrame(frameWriteMsg{
			write:     (*serverConn).writeDataFrame,
			cost:      uint32(len(chunk)),
			stream:    rws.stream,
			endStream: rws.curChunkIsFinal,
			done:      rws.chunkWrittenCh,
			v:         rws, // writeDataInLoop uses only rws.curChunk and rws.curChunkIsFinal
		})
		// Block until it's written, or if the client disconnects.
		select {
		case err = <-rws.chunkWrittenCh:
		case <-rws.stream.conn.doneServing:
			// Client disconnected.
			err = errClientDisconnected
		}
		if err != nil {
			break
		}
		n += len(chunk)
	}
	return
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
// TODO: copy/adapt the similar comment from Go's http server.go
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

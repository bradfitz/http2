package http2

import (
  "container/list"
  "io"
  "net/http"
  "sync"
)

type clientH2Stream struct {
  *stream
  req *http.Request
  session *clientSession
  respCh chan interface{}
  respReadyCh chan struct{}
  bb *bodyBuffer
  respData *respMsg
  streamError error
  isPush bool
  ord int
}

type respMsg struct {
  resp *http.Response
  streamEnded bool
}

type bodyMsg struct {
  data []byte
  ord int
}

type errMsg struct {
  err error
}

type pushMsg struct {
  st *clientH2Stream
  req *http.Request
}

func (st *clientH2Stream) start(pushHandler func(p PushPromise)) {
  st.bb = newBodyBuffer()
  st.bb.st = st
  // Kick off response body read coroutine immediately.
  go func(bb *bodyBuffer, pH func(p PushPromise)) {
    // TODO ugly
    defer st.session.t.setReqCanceler(st.req, nil)
    for {
      i := <- st.respCh
      switch m := i.(type) {
      case respMsg:
        st.respData = &m
        st.respReadyCh<- struct{}{}
      case bodyMsg:
        st.bb.pushData(m)
      case errMsg:
        if st.respData == nil {
          st.streamError = m.err
          st.respReadyCh<- struct{}{}
        } else {
          st.bb.closeWithError(m.err)
        }
        return
      case pushMsg:
        if pH != nil {
          go pH(PushPromise{st: m.st, Request: m.req, Associated: st.req})
        } else {
          m.st.cancel()
        }
      }
    }
  }(st.bb, pushHandler)
}

func (st *clientH2Stream) writeHeaders(req *http.Request) error {
  done := make(chan error, 1)
  hasBody := req.Body != nil
  return st.session.writeHeaders(st, &writeReqHeaders{
    method: req.Method,
    scheme: req.URL.Scheme,
    authority: req.URL.Host,
    path: req.URL.RequestURI(),
    h: req.Header,
    endStream: !hasBody,
  }, done)
}

func (st *clientH2Stream) endRequest() error {
  done := make(chan error, 1)
  return st.session.writeData(st, &writeData{
    streamID: st.id,
    endStream: true,
    p: nil,
  }, done)
}

func (st *clientH2Stream) readResponse() (*http.Response, error) {
  if st.streamError != nil {
    return nil, st.streamError
  }

  if st.respData == nil {
    <-st.respReadyCh
  }

  if st.streamError != nil {
    return nil, st.streamError
  }

  resp := st.respData.resp

  if !st.respData.streamEnded {
    resp.Request = st.req
    resp.Body = st.bb
    resp.TLS = st.session.tlsState
  }
  return resp, nil
}

func (st *clientH2Stream) noteBodyRead(n int) {
  st.session.noteBodyReadFromClient(st, n)
}

func (st *clientH2Stream) cancel() error {
  done := make(chan error, 1)
  st.session.cancelCh<- cancelMsg{st: st, done: done}
  err := <-done
  return err
}

type bodyBuffer struct {
  mu sync.Mutex
  cond *sync.Cond
  data *list.List
  bodyErr error
  st *clientH2Stream
}

func newBodyBuffer() *bodyBuffer {
  bb := &bodyBuffer{
    data: list.New(),
  }
  bb.cond = &sync.Cond{ L: &bb.mu, }
  return bb
}

func (bb *bodyBuffer) Read(p []byte) (int, error) {
  bb.mu.Lock()
  defer bb.mu.Unlock()
  if bb.data.Len() == 0 {
    if bb.bodyErr != nil {
      return 0, bb.bodyErr
    }
    bb.cond.Wait()
    if bb.data.Len() == 0 && bb.bodyErr != nil {
      return 0, bb.bodyErr
    }
  }
  front := bb.data.Front()
  b := front.Value.([]byte)
  effectiveLen := copy(p, b)
  if effectiveLen < len(b) {
    front.Value = b[effectiveLen:]
  } else {
    bb.data.Remove(front)
  }
  bb.st.noteBodyRead(effectiveLen)
  return effectiveLen, nil
}

func (bb *bodyBuffer) Close() error {
  bb.closeWithError(io.EOF)
  return nil
}

func (bb *bodyBuffer) pushData(body bodyMsg) {
  bb.mu.Lock()
  if len(body.data) > 0 {
    bb.data.PushBack(body.data)
  }
  bb.mu.Unlock()
  bb.cond.Signal()
}

func (bb *bodyBuffer) closeWithError(err error) {
  bb.mu.Lock()
  bb.bodyErr = err
  bb.mu.Unlock()
  bb.cond.Signal()
}

type reqBodyWriter struct {
  st *clientH2Stream
}

func (rw *reqBodyWriter) Write(b []byte) (int, error) {
  done := make(chan error, 1)
  if err := rw.st.session.writeData(rw.st, &writeData{
    streamID: rw.st.id,
    endStream: false,
    p: b,
  }, done); err != nil {
    return 0, err
  }
  return len(b), nil
}

package http2

import (
  "bytes"
  "container/list"
  "crypto/tls"
  "fmt"
  "io"
  "log"
  "net/http"
  "net/url"
  "strconv"
  "strings"
  "sync"
  "time"
  "github.com/bradfitz/http2/hpack"
)

const (
  maxConcurrencyStreamLimit uint32 = 1000
)

type clientSession struct {
  t *YaTransport
  tconn *tls.Conn
  tlsState *tls.ConnectionState
  bw *bufferedWriter

  key sessionKey

  framer *Framer
  hpackDecoder *hpack.Decoder

  streams map[uint32]*clientH2Stream
  createdStreams map[*stream]*clientH2Stream

  headerWriteBuf bytes.Buffer
  hpackEncoder *hpack.Encoder

  pushEnabled bool
  nextStreamID uint32
  maxStreamID uint32
  maxPushStreamID uint32
  initialWindowSize int32
  headerTableSize uint32
  maxConcurrentStreams uint32
  curOpenStreams uint32
  curPushStreamsTotal uint32
  curPushStreamsActive uint32

  writeSched writeScheduler

  serveG goroutineLock

  readFrameCh chan frameAndGate
  readFrameErrCh chan error
  wantWriteFrameCh chan frameWriteMsg
  wroteFrameCh chan struct{}
  bodyReadCh chan clientBodyReadMsg
  streamReqCh chan *streamRequest
  doneServing chan struct{}
  isIdleCh chan chan bool
  cancelCh chan cancelMsg
  wantCloseCh chan struct{}

  writingFrame bool
  unackedSettings int
  sawFirstSettings bool

  shutdownTimerCh <-chan time.Time
  shutdownTimer *time.Timer

  needToSendSettingsAck bool
  needsFrameFlush bool

  curHeaders headersParam
  canonHeader map[string]string

  flow flow
  inflow flow

  streamRequests *list.List

  goAwayState goAwayStateStruct
}

type streamRequest struct {
  errCh chan error
  stCh chan *clientH2Stream

  // TODO: use atomic?
  cmu sync.Mutex
  isCancelled bool
}

type cancelMsg struct {
  done chan error
  st *clientH2Stream
}

type availablilityState uint8
const (
  stateAvailable availablilityState = 0x0
  stateGoingAway availablilityState = 0x1
  stateDraining availablilityState = 0x2
)

type goAwayStateStruct struct {
  availablility availablilityState
  needToSend bool
  code ErrCode
  allDone bool
}

func (sr *streamRequest) cancel() {
  sr.cmu.Lock()
  defer sr.cmu.Unlock()
  sr.isCancelled = true
}

func (sr *streamRequest) cancelled() bool {
  sr.cmu.Lock()
  defer sr.cmu.Unlock()
  return sr.isCancelled
}

type headersParam struct {
  stream* clientH2Stream
  header http.Header
  status string
  method string
  scheme string
  authority string
  path string
  sawRegularHeader bool
  invalidHeader bool
  associatedStreamID uint32
}

func (cs *clientSession) Framer() *Framer  { return cs.framer }
func (cs *clientSession) CloseConn() error { return cs.tconn.Close() }
func (cs *clientSession) Flush() error     { return cs.bw.Flush() }
func (cs *clientSession) HeaderEncoder() (*hpack.Encoder, *bytes.Buffer) {
  return cs.hpackEncoder, &cs.headerWriteBuf
}

func newClientSession(t *YaTransport, tconn *tls.Conn,
                      state *tls.ConnectionState, initialWindowSize int32,
                      initialMaxFrameSize uint32, initialHeaderTableSize uint32,
                      pushEnabled bool) *clientSession {
  cs := &clientSession{
    t: t,
    tconn: tconn,
    tlsState: state,
    streams: make(map[uint32]*clientH2Stream),
    createdStreams: make(map[*stream]*clientH2Stream),
    nextStreamID: 1,
    initialWindowSize: initialWindowSize,
    writeSched: writeScheduler{
      maxFrameSize: initialMaxFrameSize,
    },
    bw: newBufferedWriter(tconn),
    headerTableSize: initialHeaderTableSize,
    readFrameCh: make(chan frameAndGate),
    readFrameErrCh: make(chan error, 1),
    wantWriteFrameCh: make(chan frameWriteMsg, 8),
    wroteFrameCh: make(chan struct{}, 1),
    bodyReadCh: make(chan clientBodyReadMsg),
    streamReqCh: make(chan *streamRequest),
    doneServing: make(chan struct{}),
    isIdleCh: make(chan chan bool),
    cancelCh: make(chan cancelMsg),
    wantCloseCh: make(chan struct{}),
    streamRequests: list.New(),
    pushEnabled: pushEnabled,
    goAwayState: goAwayStateStruct{availablility: stateAvailable},
  }
  if t.MaxConcurrentStreams != 0 {
    cs.maxConcurrentStreams = t.MaxConcurrentStreams
  } else {
    cs.maxConcurrentStreams = 100
  }
  cs.flow.add(initialWindowSize)
  cs.inflow.add(initialWindowSize)
  cs.hpackEncoder = hpack.NewEncoder(&cs.headerWriteBuf)
  cs.hpackDecoder = hpack.NewDecoder(initialHeaderTableSize, cs.onNewHeaderField)

  fr := NewFramer(cs.bw, tconn)
  if v := t.MaxReadFrameSize; v >= minMaxFrameSize && v <= maxFrameSize {
    fr.SetMaxReadFrameSize(v)
  } else {
    fr.SetMaxReadFrameSize(defaultMaxReadFrameSize)
  }
  cs.framer = fr
  return cs
}

func (cs *clientSession) createStream(req *http.Request) (*clientH2Stream, error) {
  errCh := make(chan error)
  stCh := make(chan *clientH2Stream)
  sr := &streamRequest{ errCh: errCh, stCh: stCh }
  // TODO ugly.
  cs.t.setReqCanceler(req, func() { sr.cancel() })
  cs.streamReqCh<- sr
  select {
  case st := <-stCh:
    return st, nil
  case err := <-errCh:
    return nil, err
  }
}

func (cs *clientSession) writeHeaders(st *clientH2Stream, headerData *writeReqHeaders, tempCh chan error) error {
  cs.serveG.checkNotOn() // NOT on
  var errc chan error
  if headerData.h != nil {
    errc = tempCh
  }
  cs.writeFrameFromClient(frameWriteMsg{
    write:  headerData,
    stream: st.stream,
    done:   errc,
  })
  if errc != nil {
    select {
    case <-errc:
      // Ignore. Just for synchronization.
      // Any error will be handled in the writing goroutine.
    case <-cs.doneServing:
      // Client has closed the connection.
    }
  }
  return nil
}

func (cs *clientSession) writeData(st *clientH2Stream, writeData *writeData, ch chan error) error {
  cs.writeFrameFromClient(frameWriteMsg{
    write:  writeData,
    stream: st.stream,
    done:   ch,
  })
  select {
  case err := <-ch:
    return err
  case <-cs.doneServing:
    return errClientDisconnected
  case <-st.cw:
    return errStreamBroken
  }
}

func (cs *clientSession) writeFrameFromClient(wm frameWriteMsg) {
  cs.serveG.checkNotOn() // NOT
  select {
  case cs.wantWriteFrameCh <-wm:
  case <-cs.doneServing:
    // Client has closed their connection to the server.
  }
}

func (cs *clientSession) mainLoop() {
  defer cs.tconn.Close()
  defer cs.closeAllStreamsOnConnClose()
  defer cs.stopShutdownTimer()
  defer close(cs.doneServing)

  cs.serveG = newGoroutineLock()
  cs.writeFrame(frameWriteMsg{
    write: writeSettings{
      {SettingMaxFrameSize, initialMaxFrameSize},
      {SettingMaxConcurrentStreams, cs.maxConcurrentStreams},
    },
  })
  cs.unackedSettings++

  go cs.readFrames()

  settingsTimer := time.NewTimer(firstSettingsTimeout)
  for {
    if cs.goAwayState.allDone {
      return
    }
    select {
    case sReq := <-cs.streamReqCh:
      cs.processStreamRequest(sReq)
    case cReq := <-cs.cancelCh:
      cs.cancelStream(cReq)
    case wm := <-cs.wantWriteFrameCh:
      cs.writeFrame(wm)
    case <-cs.wroteFrameCh:
      cs.writingFrame = false
      cs.scheduleFrameWrite()
    case resp := <-cs.isIdleCh:
      resp<- len(cs.streams) == 0
    case fg, ok := <-cs.readFrameCh:
      if cs.goAwayState.availablility == stateDraining {
        if ok {
          fg.g.Done()
          continue
        }
      }

      if !ok {
        cs.readFrameCh = nil
      }
      if !cs.processFrameFromReader(fg, ok) {
        return
      }
      if settingsTimer.C != nil {
        settingsTimer.Stop()
        settingsTimer.C = nil
      }
    case m := <-cs.bodyReadCh:
      cs.noteBodyRead(m.st, m.n)
    case <-cs.wantCloseCh:
      cs.goAway(ErrCodeNo)
    case <-settingsTimer.C:
      cs.logf("timeout waiting for SETTINGS frames from %v", cs.tconn.RemoteAddr())
      return
    case <-cs.shutdownTimerCh:
      cs.vlogf("GOAWAY close timer fired; closing conn from %v", cs.tconn.RemoteAddr())
      return
    }
  }
}

func (cs *clientSession) cancelStream(cReq cancelMsg) {
  if st, ok := cs.createdStreams[cReq.st.stream]; ok {
    if st.id != 0 {
      panic("stream with id but not initiated")
    }
    delete(cs.createdStreams, cReq.st.stream)
  } else {
    if cReq.st.id == 0 {
      panic("Active stream without id assigned") 
    }
    cs.resetStream(StreamError{cReq.st.id, ErrCodeCancel})
  }
  cReq.done<- nil
}

func (cs *clientSession) numClientActiveStreams() uint32 {
  cs.serveG.check()
  return cs.curOpenStreams - cs.curPushStreamsTotal
}

func (cs *clientSession) processStreamRequest(req *streamRequest) {
  cs.serveG.check()
  if cs.goAwayState.availablility != stateAvailable {
    req.errCh<- ConnectionError(cs.goAwayState.code)
  }
  if cs.numClientActiveStreams() < cs.maxConcurrentStreams {
    cst := cs.createClientStream(false, 0)
    req.stCh<- cst
  } else {
    cs.streamRequests.PushBack(req)
  }
}

func (cs *clientSession) startGoingAway(lastGoodStreamID uint32, errCode ErrCode) {
  for ; cs.streamRequests.Len() > 0; {
    fr := cs.streamRequests.Front()
    sr := fr.Value.(*streamRequest)
    if !sr.cancelled() {
      sr.errCh<- ConnectionError(cs.goAwayState.code)
    }
    cs.streamRequests.Remove(fr)
  }
  for k, st := range cs.createdStreams {
    st.respCh<- cs.goAwayState.code
    delete(cs.createdStreams, k)
  }
  for _, st := range cs.streams {
    if st.id > lastGoodStreamID {
      cs.resetStream(StreamError{st.id, ErrCodeCancel})
    }
  }
  cs.maybeFinishGoingAway()
}

func (cs *clientSession) maybeFinishGoingAway() {
  if cs.goAwayState.availablility == stateGoingAway && len(cs.streams) == 0 {
    cs.drainSession(ErrCodeNo)
  }
}

func (cs *clientSession) drainSession(errCode ErrCode) {
  if cs.goAwayState.availablility == stateDraining {
    return
  }

  cs.makeUnavailable()
  if errCode != ErrCodeNo && errCode != ErrCodeCancel {
    cs.goAwayState.needToSend = true
    cs.goAwayState.code = errCode
    cs.scheduleFrameWrite()
  }
  cs.goAwayState.availablility = stateDraining

  if errCode != ErrCodeNo {
    cs.startGoingAway(0, errCode)
    cs.shutDownIn(2500 * time.Millisecond)
  } else {
    // TODO: configurable
    cs.shutDownIn(10 * time.Second)
  }
}

func (cs *clientSession) createClientStream(isPush bool, id uint32) *clientH2Stream {
  st := &stream{
    id: id,
    state: stateOpen,
    isPush: isPush,
  }
  st.cw.Init()
  st.flow.conn = &cs.flow
  st.flow.add(cs.initialWindowSize)
  st.inflow.conn = &cs.inflow
  st.inflow.add(initialWindowSize)

  cst := &clientH2Stream{
    stream: st,
    session: cs,
    respCh: make(chan interface{}, 10),
    respReadyCh: make(chan struct{}, 1),
  }

  cs.curOpenStreams++
  if st.isPush {
    cst.state = stateResvRemote
    cst.isPush = true
    cs.curPushStreamsTotal++
    cs.streams[cst.id] = cst
  } else {
    cs.createdStreams[st] = cst
  }
  cst.start(cs.t.PushHandler)
  return cst
}

func (cs *clientSession) processFrameFromReader(fg frameAndGate, fgValid bool) bool {
  cs.serveG.check()
  var clientGone bool
  var err error
  if !fgValid {
    err = <-cs.readFrameErrCh
    if err == ErrFrameTooLarge {
      cs.goAway(ErrCodeFrameSize)
      return true // goAway will close the loop
    }
    clientGone = err == io.EOF || strings.Contains(err.Error(), "use of closed network connection")
    if clientGone {
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
    cs.vlogf("got %v: %#v", f.Header(), f)
    err = cs.processFrame(f)
    fg.g.Done() // unblock the readFrames goroutine
    if err == nil {
      return true
    }
  }

  switch ev := err.(type) {
  case StreamError:
    cs.resetStream(ev)
    return true
  case goAwayFlowError:
    cs.goAway(ErrCodeFlowControl)
    return true
  case ConnectionError:
    cs.logf("%v: %v", cs.tconn.RemoteAddr(), ev)
    cs.goAway(ErrCode(ev))
    return true // goAway will handle shutdown
  default:
    if !fgValid {
      cs.logf("disconnecting; error reading frame from client %s: %v", cs.tconn.RemoteAddr(), err)
    } else {
      cs.logf("disconnection due to other error: %v", err)
    }
  }
  return false
}

func (cs *clientSession) processFrame(f Frame) error {
  cs.serveG.check()

  // First frame received must be SETTINGS.
  if !cs.sawFirstSettings {
    if _, ok := f.(*SettingsFrame); !ok {
      return ConnectionError(ErrCodeProtocol)
    }
    cs.sawFirstSettings = true
  }

  if s := cs.curHeaderStreamID(); s != 0 {
    if cf, ok := f.(*ContinuationFrame); !ok {
      return ConnectionError(ErrCodeProtocol)
    } else if cf.Header().StreamID != s {
      return ConnectionError(ErrCodeProtocol)
    }
  }

  switch f := f.(type) {
  case *SettingsFrame:
    return cs.processSettings(f)
  case *HeadersFrame:
    return cs.processHeaders(f)
  case *ContinuationFrame:
    return cs.processContinuation(f)
  case *WindowUpdateFrame:
    return cs.processWindowUpdate(f)
  case *PingFrame:
    return cs.processPing(f)
  case *DataFrame:
    return cs.processData(f)
  case *RSTStreamFrame:
    return cs.processResetStream(f)
  case *PriorityFrame:
    //return cs.processPriority(f)
  case *PushPromiseFrame:
    return cs.processPushPromise(f)
  case *GoAwayFrame:
    return cs.processGoAway(f)
  default:
    log.Printf("Ignoring frame: %v", f.Header())
    return nil
  }
  return nil
}

func (cs *clientSession) processSettings(f *SettingsFrame) error {
  cs.serveG.check()
  if f.IsAck() {
    cs.unackedSettings--
    if cs.unackedSettings < 0 {
      // Why is the peer ACKing settings we never sent?
      // The spec doesn't mention this case, but
      // hang up on them anyway.
      return ConnectionError(ErrCodeProtocol)
    }
    return nil
  }
  if err := f.ForeachSetting(cs.processSetting); err != nil {
    return err
  }
  cs.needToSendSettingsAck = true
  cs.scheduleFrameWrite()
  return nil
}

func (cs *clientSession) processSetting(s Setting) error {
  cs.serveG.check()
  if err := s.Valid(); err != nil {
    return err
  }
  cs.vlogf("processing setting %v", s)
  switch s.ID {
  case SettingHeaderTableSize:
    cs.headerTableSize = s.Val
    cs.hpackEncoder.SetMaxDynamicTableSize(s.Val)
  case SettingMaxConcurrentStreams:
    if s.Val > maxConcurrencyStreamLimit {
      cs.maxConcurrentStreams = maxConcurrencyStreamLimit
    } else {
      cs.maxConcurrentStreams = s.Val
    }
    cs.processPendingStreamRequests()
  case SettingInitialWindowSize:
    return cs.processSettingInitialWindowSize(s.Val)
  case SettingMaxFrameSize:
    cs.writeSched.maxFrameSize = s.Val
  default:
    // Unknown setting: "An endpoint that receives a SETTINGS
    // frame with any unknown or unsupported identifier MUST
    // ignore that setting."
  }
  return nil
}

func (cs *clientSession) processSettingInitialWindowSize(val uint32) error {
  cs.serveG.check()
  // Note: val already validated to be within range by
  // processSetting's Valid call.

  // "A SETTINGS frame can alter the initial flow control window
  // size for all current streams. When the value of
  // SETTINGS_INITIAL_WINDOW_SIZE changes, a receiver MUST
  // adjust the size of all stream flow control windows that it
  // maintains by the difference between the new value and the
  // old value."
  old := cs.initialWindowSize
  cs.initialWindowSize = int32(val)
  growth := cs.initialWindowSize - old // may be negative
  for _, st := range cs.streams {
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

func (cs *clientSession) processPing(f *PingFrame) error {
  cs.serveG.check()
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
  cs.writeFrame(frameWriteMsg{write: writePingAck{f}})
  return nil
}

func (cs *clientSession) processWindowUpdate(f *WindowUpdateFrame) error {
  cs.serveG.check()
  switch {
  case f.StreamID != 0: // stream-level flow control
    st := cs.streams[f.StreamID]
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
    if !cs.flow.add(int32(f.Increment)) {
      return goAwayFlowError{}
    }
  }
  cs.scheduleFrameWrite()
  return nil
}

func (cs *clientSession) makeUnavailable() {
  if cs.goAwayState.availablility == stateAvailable {
    cs.goAwayState.availablility = stateGoingAway
    cs.t.makeSessionUnavailable(cs)
  }
}

func (cs *clientSession) processGoAway(f *GoAwayFrame) error {
  cs.makeUnavailable()
  cs.startGoingAway(f.LastStreamID, ErrCodeCancel)
  cs.maybeFinishGoingAway()
  return nil
}

func (cs *clientSession) processResetStream(f *RSTStreamFrame) error {
  cs.serveG.check()

  state, st := cs.state(f.StreamID)
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
    err := StreamError{f.StreamID, f.ErrCode}
    cs.closeClientStream(st, err)
  }
  return nil
}

func (cs *clientSession) processData(f *DataFrame) error {
  cs.serveG.check()
  id := f.Header().StreamID
  st, ok := cs.streams[id]
  if !ok {
    // By the time data comes in, the stream may already be inactive.
    return nil
  }
  if st.state != stateHalfClosedLocal {
    return StreamError{id, ErrCodeStreamClosed}
  }

  data := make([]byte, len(f.Data()))
  copy(data, f.Data())

  // Sender sending more than they'd declared?
  if st.declBodyBytes != -1 && st.bodyBytes+int64(len(data)) > st.declBodyBytes {
    st.respCh<- errMsg{err: fmt.Errorf("sender tried to send more than declared Content-Length of %d bytes", st.declBodyBytes)}
    return StreamError{id, ErrCodeStreamClosed}
  }
  if len(data) > 0 {
    // Check whether the client has flow control quota.
    if int(st.inflow.available()) < len(data) {
      return StreamError{id, ErrCodeFlowControl}
    }
    st.inflow.take(int32(len(data)))
    st.bodyBytes += int64(len(data))
    st.ord++
    st.respCh<- bodyMsg{ data: data, ord: st.ord }
  }
  if f.StreamEnded() {
    if st.declBodyBytes != -1 && st.declBodyBytes != st.bodyBytes {
      st.respCh<- errMsg{err: fmt.Errorf("request declared a Content-Length of %d but only wrote %d bytes",
        st.declBodyBytes, st.bodyBytes)}
    } else {
      cs.closeClientStream(st, io.EOF)
    }
  }
  return nil
}

func (cs *clientSession) processHeaders(f *HeadersFrame) error {
  cs.serveG.check()
  id := f.Header().StreamID

  if cs.curHeaders.stream != nil {
    return ConnectionError(ErrCodeProtocol)
  }

  st, ok := cs.streams[id]
  if !ok {
    // It may just be that the stream was cancelled.
    return nil
  }

  if st.isPush {
    if st.state != stateResvRemote {
      return StreamError{id, ErrCodeProtocol}
    } else {
      st.state = stateHalfClosedLocal
      cs.curPushStreamsActive++
    }
  } else {
    if id%2 != 1 {
      return ConnectionError(ErrCodeProtocol)
    }
    if st.state != stateHalfClosedLocal {
      return StreamError{id, ErrCodeStreamClosed}
    }
  }

  if f.StreamEnded() {
    st.state = stateClosed
  }

  cs.curHeaders = headersParam{
    stream: st,
    header: make(http.Header),
  }
  return cs.processHeaderBlockFragment(st, f.HeaderBlockFragment(), f.HeadersEnded())
}

func (cs *clientSession) processPushPromise(f *PushPromiseFrame) error {
  cs.serveG.check()
  id := f.PromiseID
  if cs.goAwayState.availablility == stateGoingAway {
    // Chrome does so.
    se := StreamError{id, ErrCodeRefusedStream}
    cs.writeFrame(frameWriteMsg{write: se})
    return nil
  }

  if id%2 != 0 || id < cs.maxPushStreamID || cs.curHeaders.stream != nil {
    return ConnectionError(ErrCodeProtocol)
  }

  st, ok := cs.streams[id]
  if ok {
    panic("Some strange things happen, duplicate push PromiseID")
  }

  st = cs.createClientStream(true, id)

  _, ok = cs.streams[f.Header().StreamID]
  if !ok {
    se := StreamError{st.id, ErrCodeProtocol}
    cs.resetStream(se)
    return se
  }

  cs.curHeaders = headersParam{
    stream: st,
    header: make(http.Header),
    associatedStreamID: f.Header().StreamID,
  }

  return cs.processHeaderBlockFragment(st, f.HeaderBlockFragment(), f.HeadersEnded()) 
}

func (cs *clientSession) processContinuation(f *ContinuationFrame) error {
  cs.serveG.check()
  st, ok := cs.streams[f.Header().StreamID]
  if !ok {
    // It may just be that the stream was cancelled.
    return nil
  }
  if cs.curHeaderStreamID() != st.id {
    return StreamError{st.id, ErrCodeStreamClosed}
  }
  return cs.processHeaderBlockFragment(st, f.HeaderBlockFragment(), f.HeadersEnded())
}

func (cs *clientSession) processHeaderBlockFragment(st *clientH2Stream, frag []byte, end bool) error {
  cs.serveG.check()
  if _, err := cs.hpackDecoder.Write(frag); err != nil {
    // TODO: convert to stream error I assume?
    return err
  }
  if !end {
    return nil
  }
  if err := cs.hpackDecoder.Close(); err != nil {
    // TODO: convert to stream error I assume?
    return err
  }
  defer cs.resetHeadersParam()
  if st.isPush && st.state == stateResvRemote {
    return cs.processPushHeaders(st)
  } else {
    return cs.processResponseHeaders(st)
  }
}

func (cs *clientSession) processResponseHeaders(st *clientH2Stream) error {
  cs.serveG.check()
  res, err := cs.newResponse()
  if err != nil {
    return err
  }
  st.declBodyBytes = res.ContentLength
  st.respCh<- respMsg{ resp: res, streamEnded: st.state == stateClosed }

  if st.state == stateClosed {
    st.state = stateHalfClosedLocal
    cs.closeClientStream(st, io.EOF)
  }
  return nil
}

func (cs *clientSession) processPushHeaders(st *clientH2Stream) error {
  cs.serveG.check()
  if !cs.pushEnabled {
    return StreamError{st.id, ErrCodeRefusedStream}
  }

  req, err := cs.newPushRequest()
  if err != nil {
    return err
  }

  if assoc, ok := cs.streams[cs.curHeaders.associatedStreamID]; ok {
    assoc.respCh<- pushMsg{st: st, req: req}
  } else {
    return StreamError{st.id, ErrCodeProtocol}
  }
  return nil
}

func (cs *clientSession) newResponse() (*http.Response, error) {
  cs.serveG.check()
  rp := &cs.curHeaders
  if rp.invalidHeader || rp.status == "" {
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
    return nil, StreamError{rp.stream.id, ErrCodeProtocol}
  }
  statusCode, err := strconv.Atoi(rp.status)
  if err != nil {
    return nil, StreamError{rp.stream.id, ErrCodeProtocol}
  }
  bodyOpen := rp.stream.state == stateHalfClosedLocal
  res := &http.Response{
    Proto: "HTTP/2.0",
    ProtoMajor: 2,
    Header: rp.header,
    Status: strconv.Itoa(statusCode) + " " + http.StatusText(statusCode),
    StatusCode: statusCode,
  }
  if bodyOpen {
    if vv, ok := rp.header["Content-Length"]; ok {
      res.ContentLength, _ = strconv.ParseInt(vv[0], 10, 64)
    } else {
      res.ContentLength = -1
    }
  }
  return res, nil
}

func (cs *clientSession) newPushRequest() (*http.Request, error) {
  cs.serveG.check()
  rp := &cs.curHeaders
  if rp.invalidHeader || rp.method == "" || rp.scheme == "" || rp.authority == "" || rp.path == "" {
    return nil, StreamError{rp.stream.id, ErrCodeProtocol}
  }
  u, err := url.Parse(rp.scheme + "://" + rp.authority + rp.path)
  if err != nil {
    return nil, StreamError{rp.stream.id, ErrCodeProtocol}
  }
  req := &http.Request{
    Method: rp.method,
    URL: u,
    Header: rp.header,
  }
  return req, nil
}

func (cs *clientSession) resetHeadersParam() {
  cs.serveG.check()
  cs.curHeaders = headersParam{}
}

func (cs *clientSession) writeFrame(wm frameWriteMsg) {
  cs.serveG.check()
  if cs.goAwayState.availablility == stateDraining {
    return
  }
  cs.writeSched.add(wm)
  cs.scheduleFrameWrite()
}

func (cs *clientSession) scheduleFrameWrite() {
  cs.serveG.check()
  if cs.writingFrame {
    return
  }
  if cs.goAwayState.needToSend {
    cs.goAwayState.needToSend = false
    cs.startFrameWrite(frameWriteMsg{
      write: &writeGoAway{
        maxStreamID: cs.maxPushStreamID,
        code:        cs.goAwayState.code,
      },
    })
    return
  }
  // TODO Move this to standard frame queue?
  if cs.needToSendSettingsAck {
    cs.needToSendSettingsAck = false
    cs.startFrameWrite(frameWriteMsg{write: writeSettingsAck{}})
    return
  }

  if cs.goAwayState.availablility == stateDraining && cs.writeSched.empty() {
    cs.goAwayState.allDone = true
  }

  if wm, ok := cs.writeSched.take(); ok {
    cs.startFrameWrite(wm)
    return
  }
  if cs.needsFrameFlush {
    cs.startFrameWrite(frameWriteMsg{write: flushFrameWriter{}})
    cs.needsFrameFlush = false // after startFrameWrite, since it sets this true
    return
  }
}

func (cs *clientSession) startFrameWrite(wm frameWriteMsg) {
  cs.serveG.check()
  if cs.writingFrame {
    panic("internal error: can only be writing one frame at a time")
  }

  st := wm.stream
  if st != nil {
    switch wr := wm.write.(type) {
    case *writeReqHeaders:
      wr.streamID = cs.nextStreamID
      st.id = wr.streamID
      cs.nextStreamID += 2
      cs.maxStreamID = st.id
      cst := cs.createdStreams[st]
      delete(cs.createdStreams, st)
      cs.streams[st.id] = cst
      if st.state == stateHalfClosedLocal {
        panic("internal error: attempt to send frame on half-closed-local stream")
      }
    case *writeData:
      if st.state == stateHalfClosedLocal {
        panic("internal error: attempt to send frame on half-closed-local stream")
      }
    }

    switch st.state {
    case stateClosed:
      if st.sentReset || st.gotReset {
        // Skip this frame. But fake the frame write to reschedule:
        cs.wroteFrameCh <- struct{}{}
        return
      }
      panic(fmt.Sprintf("internal error: attempt to send a write %v on a closed stream", wm))
    }
  }

  cs.writingFrame = true
  cs.needsFrameFlush = true
  if endsStream(wm.write) {
    if st == nil {
      panic("internal error: expecting non-nil stream")
    }
    switch st.state {
    case stateOpen:
      st.state = stateHalfClosedLocal
    case stateHalfClosedRemote:
      cs.closeStream(st, nil)
    }
  }
  go cs.writeFrameAsync(wm)
}

func (cs *clientSession) writeFrameAsync(wm frameWriteMsg) {
  err := wm.write.writeFrame(cs)
  if ch := wm.done; ch != nil {
    select {
    case ch <- err:
    default:
      panic(fmt.Sprintf("unbuffered done channel passed in for type %T", wm.write))
    }
  }
  cs.wroteFrameCh <- struct{}{} // tickle frame selection scheduler
}

func (cs *clientSession) onNewHeaderField(f hpack.HeaderField) {
  cs.serveG.check()
  cs.vlogf("got header field %+v", f)
  switch {
  case !validHeader(f.Name):
    cs.curHeaders.invalidHeader = true
  case strings.HasPrefix(f.Name, ":"):
    if cs.curHeaders.sawRegularHeader {
      cs.logf("pseudo-header after regular header")
      cs.curHeaders.invalidHeader = true
      return
    }
    var dst *string
    switch f.Name {
    case ":status":
      dst = &cs.curHeaders.status
    case ":method":
      dst = &cs.curHeaders.method
    case ":path":
      dst = &cs.curHeaders.path
    case ":scheme":
      dst = &cs.curHeaders.scheme
    case ":authority":
      dst = &cs.curHeaders.authority
    default:
      // 8.1.2.1 Pseudo-Header Fields
      // "Endpoints MUST treat a request or response
      // that contains undefined or invalid
      // pseudo-header fields as malformed (Section
      // 8.1.2.6)."
      cs.logf("invalid pseudo-header %q", f.Name)
      cs.curHeaders.invalidHeader = true
      return
    }
    if *dst != "" {
      cs.logf("duplicate pseudo-header %q sent", f.Name)
      cs.curHeaders.invalidHeader = true
      return
    }
    *dst = f.Value
  case f.Name == "cookie":
    cs.curHeaders.sawRegularHeader = true
    if s, ok := cs.curHeaders.header["Cookie"]; ok && len(s) == 1 {
      s[0] = s[0] + "; " + f.Value
    } else {
      cs.curHeaders.header.Add("Cookie", f.Value)
    }
  default:
    cs.curHeaders.sawRegularHeader = true
    cs.curHeaders.header.Add(cs.canonicalHeader(f.Name), f.Value)
  }
}

func (cs *clientSession) readFrames() {
  g := make(gate, 1)
  for {
    f, err := cs.framer.ReadFrame()
    if err != nil {
      cs.readFrameErrCh <- err
      close(cs.readFrameCh)
      return
    }
    cs.readFrameCh <- frameAndGate{f, g}
    // We can't read another frame until this one is
    // processed, as the ReadFrame interface doesn't copy
    // memory.  The Frame accessor methods access the last
    // frame's (shared) buffer. So we wait for the
    // serve goroutine to tell us it's done:
    g.Wait()
  }
}

func (cs *clientSession) curHeaderStreamID() uint32 {
  cs.serveG.check()
  st := cs.curHeaders.stream
  if st == nil {
    return 0
  }
  return st.id
}

func (cs *clientSession) resetStream(se StreamError) {
  cs.serveG.check()
  cs.writeFrame(frameWriteMsg{write: se})
  if st, ok := cs.streams[se.StreamID]; ok {
    st.sentReset = true
    cs.closeClientStream(st, se)
  }
}

func (cs *clientSession) goAway(code ErrCode) {
  cs.serveG.check()
  cs.drainSession(code)
}

type clientBodyReadMsg struct {
  st *clientH2Stream
  n  int
}

func (cs *clientSession) noteBodyReadFromClient(st *clientH2Stream, n int) {
  cs.serveG.checkNotOn() // NOT on
  cs.bodyReadCh <- clientBodyReadMsg{st, n}
}

func (cs *clientSession) noteBodyRead(st *clientH2Stream, n int) {
  cs.serveG.check()
  cs.sendWindowUpdate(nil, n) // conn-level
  if st.state != stateHalfClosedRemote && st.state != stateClosed {
    // Don't send this WINDOW_UPDATE if the stream is closed
    // remotely.
    cs.sendWindowUpdate(st.stream, n)
  }
}

// st may be nil for conn-level
func (cs *clientSession) sendWindowUpdate(st *stream, n int) {
  cs.serveG.check()
  // "The legal range for the increment to the flow control
  // window is 1 to 2^31-1 (2,147,483,647) octets."
  // A Go Read call on 64-bit machines could in theory read
  // a larger Read than this. Very unlikely, but we handle it here
  // rather than elsewhere for now.
  const maxUint31 = 1<<31 - 1
  for n >= maxUint31 {
    cs.sendWindowUpdate32(st, maxUint31)
    n -= maxUint31
  }
  cs.sendWindowUpdate32(st, int32(n))
}

// st may be nil for conn-level
func (cs *clientSession) sendWindowUpdate32(st *stream, n int32) {
  cs.serveG.check()
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
  cs.writeFrame(frameWriteMsg{
    write:  writeWindowUpdate{streamID: streamID, n: uint32(n)},
    stream: st,
  })
  var ok bool
  if st == nil {
    ok = cs.inflow.add(n)
  } else {
    ok = st.inflow.add(n)
  }
  if !ok {
    panic("internal error; sent too many window updates without decrements?")
  }
}

func (cs *clientSession) closeClientStream(st *clientH2Stream, err error) {
  st.respCh<- errMsg{err: err}
  cs.closeStream(st.stream, err)
}

func (cs *clientSession) closeStream(st *stream, err error) {
  cs.serveG.check()
  if st.state == stateIdle || st.state == stateClosed {
    panic(fmt.Sprintf("invariant; can't close stream in state %v", st.state))
  }
  st.state = stateClosed
  cs.curOpenStreams--
  if st.isPush {
    cs.curPushStreamsTotal--
    if st.state != stateResvRemote {
      cs.curPushStreamsActive--
    }
  }
  delete(cs.streams, st.id)
  if p := st.body; p != nil {
    p.Close(err)
  }
  st.cw.Close() // signals Handler's CloseNotifier, unblocks writes, etc
  cs.writeSched.forgetStream(st.id)
  cs.processPendingStreamRequests()
  cs.maybeFinishGoingAway()
}

func (cs *clientSession) processPendingStreamRequests() {
  for ; cs.numClientActiveStreams() < cs.maxConcurrentStreams && cs.streamRequests.Len() > 0; {
    sr := cs.streamRequests.Front().Value.(*streamRequest)
    cs.streamRequests.Remove(cs.streamRequests.Front())
    if !sr.cancelled() {
      cs.processStreamRequest(sr)
    }
  }
}

func (cs *clientSession) shutDownIn(d time.Duration) {
  cs.serveG.check()
  cs.shutdownTimer = time.NewTimer(d)
  cs.shutdownTimerCh = cs.shutdownTimer.C
}

func (cs *clientSession) closeAllStreamsOnConnClose() {
  cs.serveG.check()
  for _, st := range cs.streams {
    cs.closeStream(st.stream, errClientDisconnected)
  }
}

func (cs *clientSession) stopShutdownTimer() {
  cs.serveG.check()
  if t := cs.shutdownTimer; t != nil {
    t.Stop()
  }
}

func (cs *clientSession) canonicalHeader(v string) string {
  cs.serveG.check()
  cv, ok := commonCanonHeader[v]
  if ok {
    return cv
  }
  cv, ok = cs.canonHeader[v]
  if ok {
    return cv
  }
  if cs.canonHeader == nil {
    cs.canonHeader = make(map[string]string)
  }
  cv = http.CanonicalHeaderKey(v)
  cs.canonHeader[v] = cv
  return cv
}

func (cs *clientSession) idle() bool {
  done := make(chan bool)
  cs.isIdleCh<- done
  isIdle := <-done
  return isIdle
}

func (cs *clientSession) close() {
  cs.wantCloseCh<- struct{}{}
}

func (cs *clientSession) state(streamID uint32) (streamState, *clientH2Stream) {
  cs.serveG.check()
  // http://http2.github.io/http2-spec/#rfc.section.5.1
  if st, ok := cs.streams[streamID]; ok {
    return st.state, st
  }
  // "The first use of a new stream identifier implicitly closes all
  // streams in the "idle" state that might have been initiated by
  // that peer with a lower-valued stream identifier. For example, if
  // a client sends a HEADERS frame on stream 7 without ever sending a
  // frame on stream 5, then stream 5 transitions to the "closed"
  // state when the first frame for stream 7 is sent or received."
  if streamID <= cs.maxStreamID {
    return stateClosed, nil
  }
  return stateIdle, nil
}

func (cs *clientSession) vlogf(format string, args ...interface{}) {
  if VerboseLogs {
    cs.logf(format, args...)
  }
}

func (cs *clientSession) logf(format string, args ...interface{}) {
  log.Printf(format, args...)
}

func (cs *clientSession) condlogf(err error, format string, args ...interface{}) {
  if err == nil {
    return
  }
  str := err.Error()
  if err == io.EOF || strings.Contains(str, "use of closed network connection") {
    // Boring, expected errors.
    cs.vlogf(format, args...)
  } else {
    cs.logf(format, args...)
  }
}

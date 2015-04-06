package http2

import (
  "compress/gzip"
  "crypto/tls"
  "errors"
  "fmt"
  "io"
  "net"
  "net/http"
  "net/url"
  "strconv"
  "sync"
  "time"
)

func closeBody(r *http.Request) {
  if r.Body != nil {
    r.Body.Close()
  }
}

type Transport struct {
  sessMu sync.Mutex
  availableSessions map[sessionKey]*clientSession
  sessionRequests map[sessionKey]map[*sessionRequest]bool

  reqMu sync.Mutex
  reqCancellers map[*http.Request]func()

  Proxy func(*http.Request) (*url.URL, error)
  TLSClientConfig *tls.Config
  TLSHandshakeTimeout time.Duration
  DisableCompression bool
  ResponseHeaderTimeout time.Duration
  MaxConcurrentStreams uint32
  MaxReadFrameSize uint32
  PushHandler func(p PushPromise)
  Fallback *http.Transport
}

type PushPromise struct {
  Request *http.Request
  Associated *http.Request
  st *clientH2Stream
}

func (p *PushPromise) Resolve(nextHandler func(p PushPromise)) (*http.Response, error) {
  resp, err := p.st.readResponse()
  if err == nil {
    resp = prepareResponse(resp)
  }
  return resp, err
}

func (p *PushPromise) Cancel() {
  p.st.cancel()
}

func (t* Transport) RoundTrip(req *http.Request) (*http.Response, error) {
  if req.URL == nil {
    closeBody(req)
    return nil, errors.New("http: nil Request.URL")
  }

  if req.Header == nil {
    closeBody(req)
    return nil, errors.New("http: nil Request.Header")
  }

  if req.URL.Host == "" {
    closeBody(req)
    return nil, errors.New("http: no Host in request URL")
  }

  maybeRetryWithFallback := func(err error) (*http.Response, error) {
    if t.Fallback != nil {
      return t.Fallback.RoundTrip(req)
    } else {
      closeBody(req)
      return nil, err
    }
  }

  if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
    return maybeRetryWithFallback(errors.New("unsupported protocol scheme: " + req.URL.Scheme))
  }

  sk, err := t.sessionKeyForRequest(req)
  if err != nil {
    return maybeRetryWithFallback(err)
  }

  if (sk.proxy.empty() && sk.origin.scheme != Https) ||
     (!sk.proxy.empty() && sk.proxy.scheme != Https) {
    return maybeRetryWithFallback(errors.New("plaintext endpoints not supported yet"))
  }

  rt, err := t.getRoundTripper(req, sk)
  if err != nil {
    t.setReqCanceler(req, nil)
    return maybeRetryWithFallback(err)
  }

  t.setReqCanceler(req, func() { rt.stream.cancel() })
  resp, err := rt.roundTrip()
  if err != nil {
    t.setReqCanceler(req, nil)
    closeBody(req)
    return nil, err
  }
  return resp, nil
}

func (t *Transport) CloseIdleConnections() {
  var toClose []*clientSession
  t.sessMu.Lock()
  for key, sess := range t.availableSessions {
    if sess.idle() {
      toClose = append(toClose, sess)
      delete(t.availableSessions, key)
    }
  }
  t.sessMu.Unlock()
  for _, sess := range toClose {
    sess.close()
  }
}

func (t *Transport) CancelRequest(req *http.Request) {
  t.reqMu.Lock()
  cancel := t.reqCancellers[req]
  t.reqMu.Unlock()
  if cancel != nil {
    cancel()
  }
}

type Scheme uint8

const (
  Http  Scheme = 0x0
  Https Scheme = 0x1
)

type endpoint struct {
  host    string
  port    uint16
  scheme  Scheme
}

func (e *endpoint) empty() bool {
  return len(e.host) == 0
}

func endpointFromURL(u *url.URL) (ep endpoint) {
  if u.Scheme == "http" {
    ep.scheme = Http
  } else {
    ep.scheme = Https
  }

  host, port, err := net.SplitHostPort(u.Host)
  if err != nil {
    ep.host = u.Host
    ep.port = 443
  } else {
    ep.host = host
    if p, err := strconv.Atoi(port); err == nil {
      ep.port = uint16(p)
    } else {
      ep.port = 443
    }
  }
  return
}

type sessionKey struct {
  origin endpoint
  proxy  endpoint
}

func (t *Transport) sessionKeyForRequest(req *http.Request) (sessionKey, error) {
  origin := endpointFromURL(req.URL)
  var proxy endpoint
  if t.Proxy != nil {
    proxyURL, err := t.Proxy(req)
    if err != nil {
      return sessionKey{}, err
    }
    proxy = endpointFromURL(proxyURL)
  }
  return newSessionKey(origin, proxy), nil
}

type roundTripper struct {
  t *Transport
  req *http.Request
  stream *clientH2Stream
}

type gzipReader struct {
  body io.ReadCloser // underlying Response.Body
  zr   io.Reader     // lazily-initialized gzip reader
}

func (gz *gzipReader) Read(p []byte) (n int, err error) {
  if gz.zr == nil {
    gz.zr, err = gzip.NewReader(gz.body)
    if err != nil {
      return 0, err
    }
  }
  return gz.zr.Read(p)
}

func (gz *gzipReader) Close() error {
  return gz.body.Close()
}

func (rt *roundTripper) roundTrip() (resp *http.Response, err error) {
  if !rt.t.DisableCompression && rt.req.Header.Get("Accept-Encoding") == "" &&
     rt.req.Header.Get("Range") == "" && rt.req.Method != "HEAD" {
    rt.req.Header.Set("Accept-Encoding", "gzip")
  }
  if err = rt.stream.writeHeaders(rt.req); err != nil {
    return
  }
  if rt.req.Body != nil {
    if _, err := io.Copy(&reqBodyWriter{st: rt.stream}, rt.req.Body); err != nil {
      return nil, err
    }
    if err := rt.stream.endRequest(); err != nil {
      return nil, err
    }
  }
  resp, err = rt.stream.readResponse()
  if err == nil {
    resp = prepareResponse(resp)
  }
  return
}

func (t *Transport) getRoundTripper(req *http.Request, sk sessionKey) (*roundTripper, error) {
  cs, err := t.getSession(req, sk)
  if err != nil {
    return nil, err
  }

  st, err := cs.createStream(req)
  if (err != nil) {
    return nil, err
  }
  return &roundTripper{
    t: t,
    req: req,
    stream: st,
  }, nil
}

func prepareResponse(resp *http.Response) *http.Response {
  if resp.Body != nil && resp.Header.Get("Content-Encoding") == "gzip" {
    resp.Header.Del("Content-Encoding")
    resp.Header.Del("Content-Length")
    resp.ContentLength = -1
    resp.Body = &gzipReader{body: resp.Body}
  }
  return resp
}

type sessionRequest struct {
  csCh chan *clientSession
  errCh chan error
  cancelCh chan struct{}
}

func newSessionRequest() *sessionRequest {
  return &sessionRequest {
    csCh: make(chan *clientSession),
    errCh: make(chan error),
    cancelCh: make(chan struct{}),
  }
}

func (t *Transport) getSession(req *http.Request, sk sessionKey) (*clientSession, error) {
  return t.findOrCreateConnectingSession(req, sk)
}

func (t *Transport) findAvailableSession(sk sessionKey) *clientSession {
  if t.availableSessions == nil {
    t.availableSessions = make(map[sessionKey]*clientSession)
  }
  if cs, ok := t.availableSessions[sk]; ok {
    return cs
  }
  return nil
}

func (t *Transport) setReqCanceler(r *http.Request, fn func()) {
  t.reqMu.Lock()
  defer t.reqMu.Unlock()
  if t.reqCancellers == nil {
    t.reqCancellers = make(map[*http.Request]func())
  }
  if fn != nil {
    t.reqCancellers[r] = fn
  } else {
    delete(t.reqCancellers, r)
  }
}

func (t *Transport) findOrCreateConnectingSession(req *http.Request, sk sessionKey) (*clientSession, error) {
  sr := newSessionRequest()
  t.setReqCanceler(req, func() {
    close(sr.cancelCh)
    t.sessMu.Lock()
    delete(t.sessionRequests[sk], sr)
    t.sessMu.Unlock()
  })

  shouldDial := false
  t.sessMu.Lock()
  if cs := t.findAvailableSession(sk); cs != nil {
    t.sessMu.Unlock()
    return cs, nil
  }

  if t.sessionRequests == nil {
    t.sessionRequests = make(map[sessionKey]map[*sessionRequest]bool)
  }
  if _, ok := t.sessionRequests[sk]; !ok {
    t.sessionRequests[sk] = make(map[*sessionRequest]bool)
    shouldDial = true
  }
  t.sessionRequests[sk][sr] = true
  t.sessMu.Unlock()
  if shouldDial {
    go func() {
      cs, err := t.createSessionForKey(sk)
      t.sessMu.Lock()
      defer t.sessMu.Unlock()
      for sreq, _ := range t.sessionRequests[sk] {
        if err != nil {
          sreq.errCh<- err
        } else {
          sreq.csCh<- cs
        }
      }
    }()
  }
  select {
  case cs := <-sr.csCh:
    t.sessMu.Lock()
    defer t.sessMu.Unlock()
    t.availableSessions[sk] = cs
    return cs, nil
  case err := <-sr.errCh:
    return nil, err
  case <-sr.cancelCh:
    return nil, errors.New("Request canceled while waiting for connection")
  }
}

func (t *Transport) makeSessionUnavailable(cs *clientSession) {
  t.sessMu.Lock()
  defer t.sessMu.Unlock()
  delete(t.availableSessions, cs.key)
}

func (t *Transport) createSessionForKey(sk sessionKey) (*clientSession, error) {
  cfg := t.TLSClientConfig
  var host string
  var port uint16
  if !sk.proxy.empty() {
    host = sk.proxy.host
    port = sk.proxy.port
  } else {
    host = sk.origin.host
    port = sk.origin.port
  }
  if cfg == nil || cfg.ServerName == "" {
    if cfg == nil {
      cfg = &tls.Config{ServerName: host}
    } else {
      clone := *cfg
      clone.ServerName = host
      cfg = &clone
    }
  }
  cfg.NextProtos = []string{NextProtoTLS}

  tconn, err := tls.Dial("tcp", host + ":" + fmt.Sprintf("%d", port), cfg)
  if err != nil {
    return nil, err
  }
  errc := make(chan error, 2)
  var timer *time.Timer
  if d := t.TLSHandshakeTimeout; d != 0 {
    timer = time.AfterFunc(d, func() {
      errc <- errors.New("TLS handshake timeout")  
    })
  }
  go func() {
    err := tconn.Handshake()
    if timer != nil {
      timer.Stop()
    }
    errc<- err
  }()

  if err := <-errc; err != nil {
    return nil, err
  }

  if !cfg.InsecureSkipVerify {
    if err := tconn.VerifyHostname(cfg.ServerName); err != nil {
      return nil, err
    }
  }

  state := tconn.ConnectionState()
  if p := state.NegotiatedProtocol; p != NextProtoTLS {
    return nil, fmt.Errorf("bad protocol: %v", p)
  }
  if !state.NegotiatedProtocolIsMutual {
    return nil, errors.New("could not negotiate protocol mutually")
  }
  if _, err := tconn.Write(clientPreface); err != nil {
    return nil, err
  }

  cs := newClientSession(t, tconn, &state, initialWindowSize, initialMaxFrameSize, initialHeaderTableSize, t.PushHandler != nil)

  go cs.mainLoop()
  return cs, nil
}

func newSessionKey(origin, proxy endpoint) sessionKey {
  return sessionKey{ origin: origin, proxy: proxy }
}

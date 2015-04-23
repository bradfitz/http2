// Adds HTTP/1.1 Upgrade mode to github.com/bradfitz/http2
//
// Upgrade mode works similarly to http2 in tls npn or alpn modes.
// Use the UpgradeServer() function instead of ConfigureServer to
// make your standard net/http.Server's http/2 compatible (via
// the "upgrade" mechanism).

package http2

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var (
	http2SettingsHeader  = http.CanonicalHeaderKey("HTTP2-Settings")
	ignoreUpgradeHeaders = map[string]struct{}{
		"upgrade":        struct{}{},
		"connection":     struct{}{},
		"http2-settings": struct{}{},
	}
)

type upgradeMux struct {
	handler http.Handler
	srv     *http.Server
	conf    *Server
}

type mrcloser struct {
	io.Reader

	closers []io.Closer
}

type hdr struct {
	sc       *serverConn
	key, val string
}

func (h hdr) String() string {
	return fmt.Sprintf("{Name:%v, Value:%v}", h.sc.canonicalHeader(h.key), h.val)
}

func decodeSettings(b []byte) ([]Setting, error) {
	dst := make([]byte, base64.URLEncoding.DecodedLen(len(b)))
	n, err := base64.URLEncoding.Decode(dst, b)
	if err != nil {
		return nil, err
	}
	fh := FrameHeader{
		valid:  true,
		Type:   FrameSettings,
		Length: uint32(n),
	}

	f, err := parseSettingsFrame(fh, dst[:n])
	if err == nil {
		settings := make([]Setting, 0, 4)
		f.(*SettingsFrame).ForeachSetting(func(s Setting) error {
			settings = append(settings, s)
			return nil
		})
		return settings, nil
	}
	return nil, err
}

func (mrc *mrcloser) Close() (err error) {
	for _, c := range mrc.closers {
		defer func(c io.Closer) {
			if e := c.Close(); e != nil && err == nil {
				err = e
			}
		}(c)
	}
	return
}

func multiReadCloser(readers ...io.Reader) io.ReadCloser {
	mrc := &mrcloser{
		Reader:  io.MultiReader(readers...),
		closers: make([]io.Closer, 0, len(readers)),
	}

	for _, r := range readers {
		if c, ok := r.(io.Closer); ok {
			mrc.closers = append(mrc.closers, c)
		}
	}

	return mrc
}

func (m *upgradeMux) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if upg := r.Header.Get("Upgrade"); upg == "h2c-14" || upg == "h2c" {
		hj, ok := rw.(http.Hijacker)
		if !ok {
			panic("cannot hijack this server -- upgrade impossible")
		}
		h2sh := r.Header.Get(http2SettingsHeader)
		if h2sh == "" {
			http.Error(rw, "missing HTTP2-Settings header", 501)
			return
		}
		settings, err := decodeSettings(bytes.TrimSpace([]byte(h2sh)))
		if err != nil {
			http.Error(rw, "invalid HTTP2-Settings header", 501)
			return
		}

		for _, connH := range strings.Split(r.Header.Get("Connection"), ",") {
			connH = strings.TrimSpace(connH)
			if _, ok := r.Header[http.CanonicalHeaderKey(connH)]; !ok {
				http.Error(rw, "invalid connection header", 501)
				return
			}
		}

		data := []byte{0}
		n, err := r.Body.Read(data)
		if err != nil && err != io.EOF {
			http.Error(rw, err.Error(), 501)
			return
		}

		// Currently, request bodies in the upgrade request are not supported,
		// so hand this off to http/1.1 if there's one or more bytes pending.
		if n > 0 {
			r.Body = multiReadCloser(bytes.NewBuffer(data), r.Body)
			m.handler.ServeHTTP(rw, r)
			return
		}
		r.Body.Close()
		h := rw.Header()

		for k, _ := range h {
			h.Del(k)
		}
		h.Set("Connection", "Upgrade")
		h.Set("Upgrade", upg)

		rw.WriteHeader(101)
		if flusher, ok := rw.(http.Flusher); ok {
			flusher.Flush()
		}
		c, _, err := hj.Hijack()
		if err != nil {
			panic(err.Error())
		}
		defer c.Close()
		var sc *serverConn
		sc, err = m.conf.newConn(m.srv, c, m.handler, settings, func() {
			defer close(sc.upgradeCh)
			if sc.maxStreamID < 1 {
				sc.maxStreamID = 1
			}
			st := &stream{
				id:    1,
				state: stateHalfClosedRemote,
			}

			st.cw.Init()

			st.flow.conn = &sc.flow
			st.flow.add(sc.initialWindowSize)
			st.inflow.conn = &sc.inflow
			st.inflow.add(initialWindowSize)

			sc.streams[1] = st
			sc.curOpenStreams++
			sc.req = requestParam{
				stream:           st,
				method:           r.Method,
				path:             r.URL.RequestURI(),
				scheme:           r.URL.Scheme,
				authority:        r.Host,
				sawRegularHeader: true,
				header:           make(http.Header),
			}

			if sc.req.scheme == "" {
				sc.req.scheme = "http"
			}
			if VerboseLogs {
				sc.vlogf("got header field %+v", &hdr{sc, ":scheme", sc.req.scheme})
				if sc.req.method != "" {
					sc.vlogf("got header field %+v", &hdr{sc, ":method", sc.req.method})
				}
				if sc.req.path != "" {
					sc.vlogf("got header field %+v", &hdr{sc, ":path", sc.req.path})
				}
				if sc.req.authority != "" {
					sc.vlogf("got header field %+v", &hdr{sc, ":authority", sc.req.authority})
				}
			}
			for k, vals := range r.Header {
				if k == "Cookie" {
					sc.req.header.Set(sc.canonicalHeader(k), strings.Join(vals, "; "))
					continue
				}
				if _, ok := ignoreUpgradeHeaders[strings.ToLower(k)]; !ok {
					for _, v := range vals {
						if VerboseLogs {
							sc.vlogf("got header field %+v", &hdr{sc, k, v})
						}
						sc.req.header.Add(sc.canonicalHeader(k), v)
					}
				}
			}
			if err := sc.processHeaderBlockFragment(st, nil, true); err != nil {
				sc.vlogf("error processing header %v", err)
			}
		})
		if err == nil {
			m.conf.handleConn(m.srv, c, m.handler, sc)
		}
	} else {
		m.handler.ServeHTTP(rw, r)
	}
}

// UpgradeServer operates similarly to ConfigureServer() with the exception that
// it returns a clone copy of the original server that will be used to
// exclusively serve HTTP/1.1 and HTTP/1.0 requests. The "HTTP/2 Configured"
// server passed to UpgradeServer() should not be modified once this call has
// been made, nor should it actively be listening for new connections.
//
// When an existing HTTP/1.1 connection requests an upgrade to h2c or h2c-14 mode,
// the connection will be hijacked from the "clone server" (as returned by
// UpgradeServer() and handed off to the internal http2 configured server.
//
// Ex usage:
//
// http.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
// 				rw.Header().Set("Content-Type", "text/plain")
// 				fmt.Fprintf(rw, "response to %v\n", r)
// })
//
// s := http2.UpgradeServer(&http.Server{Addr:":8080"},nil)
// s.ListenAndServe()
//
// NB: Do *not* alter the returned server's Handler, as it is hooked in order to
// perform the protocol switch. Any changes to uri handling must be made in the
// "inner" server *before* it is passed to UpgradeServer(). The standard
// http.DefaultServeMux will always be used as a last resort.
//
// NB: Note that HTTP/1.1 requests with bodies (i.e. HTTP POST) are not
// supported in the initial Upgrade request but will work once the connection
// is fully HTTP/2. Requests that have content entities will automatically
// fallback to HTTP/1.1 (the upgrade silently fails but the normal handler
// is invoked).
func UpgradeServer(s *http.Server, conf *Server) *http.Server {
	if conf == nil {
		conf = new(Server)
	}

	handler := s.Handler
	if handler == nil {
		handler = http.DefaultServeMux
	}

	ConfigureServer(s, conf)
	h1s := new(http.Server)
	*h1s = *s
	h1s.Handler = &upgradeMux{
		handler: handler,
		srv:     s,
		conf:    conf,
	}

	return h1s
}

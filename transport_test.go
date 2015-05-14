// Copyright 2015 The Go Authors.
// See https://go.googlesource.com/go/+/master/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://go.googlesource.com/go/+/master/LICENSE

package http2

import (
	"errors"
	"flag"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	extNet        = flag.Bool("extnet", false, "do external network tests")
	transportHost = flag.String("transporthost", "http2.golang.org", "hostname to use for TestTransport")
	insecure      = flag.Bool("insecure", false, "insecure TLS dials")
)

func TestTransportExternal(t *testing.T) {
	if !*extNet {
		t.Skip("skipping external network test")
	}
	req, _ := http.NewRequest("GET", "https://"+*transportHost+"/", nil)
	rt := &Transport{
		InsecureTLSDial: *insecure,
	}
	res, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("%v", err)
	}
	res.Write(os.Stdout)
}

type errReader struct{}

func (r errReader) Read(_ []byte) (int, error) { return 0, errors.New("errReader: error") }

func TestTransport(t *testing.T) {
	tests := []struct {
		method  string
		body    io.Reader
		wantErr bool
	}{
		{"GET", nil, false},
		{"POST", strings.NewReader("the body"), false},
		{"POST", errReader{}, true},
	}

	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatal("ReadAll:", err)
		}
		w.Write(body)
	})
	defer st.Close()

	tr := &Transport{InsecureTLSDial: true}
	defer tr.CloseIdleConnections()

	for i, tt := range tests {
		t.Log("i:", i)
		req, err := http.NewRequest("GET", st.ts.URL, tt.body)
		if err != nil {
			t.Fatal(err)
		}
		res, err := tr.RoundTrip(req)
		if err != nil {
			if !tt.wantErr {
				t.Error("Unexpected RoundTrip error:", err)
			}
			continue
		}
		defer res.Body.Close()

		t.Logf("Got res: %+v", res)
		if g, w := res.StatusCode, 200; g != w {
			t.Errorf("StatusCode = %v; want %v", g, w)
		}
		if g, w := res.Status, "200 OK"; g != w {
			t.Errorf("Status = %q; want %q", g, w)
		}

		sr, _ := tt.body.(*strings.Reader)
		if i == 1 && sr == nil {
			panic("not a strings reader")
		}

		var (
			wantContentLength int
			wantBody          string
		)
		if sr != nil {
			sr.Seek(0, 0)
			wantContentLength = sr.Len()
			slurp, _ := ioutil.ReadAll(tt.body)
			wantBody = string(slurp)
		}

		wantHeader := http.Header{
			"Content-Length": []string{strconv.Itoa(wantContentLength)},
			"Content-Type":   []string{"text/plain; charset=utf-8"},
		}
		if !reflect.DeepEqual(res.Header, wantHeader) {
			t.Errorf("res Header = %v; want %v", res.Header, wantHeader)
		}
		if res.Request != req {
			t.Errorf("Response.Request = %p; want %p", res.Request, req)
		}
		if res.TLS == nil {
			t.Error("Response.TLS = nil; want non-nil")
		}

		slurp, err := ioutil.ReadAll(res.Body)
		if err != nil {
			t.Errorf("Unexpected Body ReadAll error: %v", err)
		} else if string(slurp) != wantBody {
			t.Errorf("Body = %q; want %q", slurp, wantBody)
		}
	}
}

func TestTransportReusesConns(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.RemoteAddr)
	}, optOnlyServer)
	defer st.Close()
	tr := &Transport{InsecureTLSDial: true}
	defer tr.CloseIdleConnections()
	get := func() string {
		req, err := http.NewRequest("GET", st.ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		slurp, err := ioutil.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("Body read: %v", err)
		}
		addr := strings.TrimSpace(string(slurp))
		if addr == "" {
			t.Fatalf("didn't get an addr in response")
		}
		return addr
	}
	first := get()
	second := get()
	if first != second {
		t.Errorf("first and second responses were on different connections: %q vs %q", first, second)
	}
}

func TestTransportAbortClosesPipes(t *testing.T) {
	shutdown := make(chan struct{})
	st := newServerTester(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			<-shutdown
		},
		optOnlyServer,
	)
	defer st.Close()
	defer close(shutdown) // we must shutdown before st.Close() to avoid hanging

	done := make(chan struct{})
	requestMade := make(chan struct{})
	go func() {
		defer close(done)
		tr := &Transport{
			InsecureTLSDial: true,
		}
		req, err := http.NewRequest("GET", st.ts.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		close(requestMade)
		_, err = ioutil.ReadAll(res.Body)
		if err == nil {
			t.Error("expected error from res.Body.Read")
		}
	}()

	<-requestMade
	// Now force the serve loop to end, via closing the connection.
	st.closeConn()
	// deadlock? that's a bug.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

// Copyright 2015 The Go Authors.
// See https://go.googlesource.com/go/+/master/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://go.googlesource.com/go/+/master/LICENSE

package http2

import (
	"bytes"
	"flag"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"reflect"
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

func TestTransport(t *testing.T) {
	const body = "sup"
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	})
	defer st.Close()

	tr := &Transport{InsecureTLSDial: true}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequest("GET", st.ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	t.Logf("Got res: %+v", res)
	if g, w := res.StatusCode, 200; g != w {
		t.Errorf("StatusCode = %v; want %v", g, w)
	}
	if g, w := res.Status, "200 OK"; g != w {
		t.Errorf("Status = %q; want %q", g, w)
	}
	wantHeader := http.Header{
		"Content-Length": []string{"3"},
		"Content-Type":   []string{"text/plain; charset=utf-8"},
	}
	if !reflect.DeepEqual(res.Header, wantHeader) {
		t.Errorf("res Header = %v; want %v", res.Header, wantHeader)
	}
	if res.Request != req {
		t.Errorf("Response.Request = %p; want %p", res.Request, req)
	}
	if res.TLS == nil {
		t.Errorf("Response.TLS = nil; want non-nil", res.TLS)
	}
	slurp, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Error("Body read: %v", err)
	} else if string(slurp) != body {
		t.Errorf("Body = %q; want %q", slurp, body)
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

func TestTransportPostBody(t *testing.T) {
	const responseBody = `response ok`
	const requestBody = `a not so very long request body`

	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(body); requestBody != got {
			t.Errorf("want request body %q, got %q", requestBody, got)
		}
		io.WriteString(w, responseBody)
	})
	defer st.Close()

	tr := &Transport{InsecureTLSDial: true}
	defer tr.CloseIdleConnections()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequest("POST", st.ts.URL, bytes.NewBufferString(requestBody))
		if err != nil {
			t.Fatal(err)
		}
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()

		if g, w := res.StatusCode, 200; g != w {
			t.Errorf("StatusCode = %v; want %v", g, w)
		}
		slurp, err := ioutil.ReadAll(res.Body)
		if err != nil {
			t.Error("response body read: %v", err)
		} else if string(slurp) != responseBody {
			t.Errorf("response body = %q; want %q", slurp, responseBody)
		}
	}()

	// deadlock? that's a bug.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
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

func TestTransport_LargeBodies_FlowControlled(t *testing.T) {
	shutdown := make(chan struct{})
	const size = 10*initialWindowSize + 1
	requestBody := bytes.Repeat([]byte("a"), size)
	responseBody := bytes.Repeat([]byte("b"), size)
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		slurp, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		} else if string(slurp) != string(requestBody) {
			t.Errorf("request body len=%d; want %d", len(slurp), len(requestBody))
		}

		doneWriting := make(chan struct{})
		go func() {
			defer close(doneWriting)
			if _, err := w.Write(responseBody); err != nil {
				t.Fatal(err)
			}
		}()
		select {
		case <-doneWriting:
		case <-shutdown:
		}
	})
	defer st.Close()
	defer close(shutdown) // we must shutdown before st.Close() to avoid hanging

	done := make(chan struct{})
	go func() {
		defer close(done)
		tr := &Transport{
			InsecureTLSDial: true,
		}
		reqBody := bytes.NewReader(requestBody)
		req, err := http.NewRequest("GET", st.ts.URL, reqBody)
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
			t.Fatal(err)
		} else if string(slurp) != string(responseBody) {
			t.Errorf("response body len=%d; want %d", len(slurp), len(responseBody))
		}
	}()

	// deadlock? that's a bug.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

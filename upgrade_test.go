// unit tests for http/1.1 -> http/2 upgrading (Section 3.2)

package http2

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testUpgradeMode int

const (
	testUpgradeNoTLS testUpgradeMode = iota
	testUpgradeTLSOnly
	testUpgradeDualTLS
)

func TestUpgradeWithCurl(t *testing.T)     { testUpgradeWithCurl(t, testUpgradeNoTLS, false) }
func TestUpgradeTLSWithCurl(t *testing.T)  { testUpgradeWithCurl(t, testUpgradeTLSOnly, true) }
func TestUpgradeDualWithCurl(t *testing.T) { testUpgradeWithCurl(t, testUpgradeDualTLS, true) }

func testUpgradeWithCurl(t *testing.T, mode testUpgradeMode, lenient bool) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Docker test when not on Linux; requires --net which won't work with boot2docker anyway")
	}
	requireCurl(t)
	const msg = "Hello from curl!\n"
	var ts2 *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Foo", "Bar")
		w.Header().Set("Client-Proto", r.Proto)
		io.WriteString(w, msg)
	})

	ts := httptest.NewUnstartedServer(handler)
	switch mode {
	case testUpgradeNoTLS:
		ts.TLS = nil
	default:
		ts2 = httptest.NewUnstartedServer(handler)
		ts2.TLS = nil
	}
	wrapped := UpgradeServer(ts.Config, &Server{
		PermitProhibitedCipherSuites: lenient,
	})
	modes := make([]testUpgradeMode, 0, 2)
	switch mode {
	case testUpgradeNoTLS:
		modes = append(modes, mode)
		ts.Config = wrapped
		ts.Start()
		t.Logf("Running test server for curl to hit at: %s", ts.URL)
	case testUpgradeDualTLS:
		modes = append(modes, testUpgradeNoTLS)
		fallthrough
	default:
		modes = append(modes, testUpgradeTLSOnly)
		ts2.Config = wrapped
		ts.TLS = ts.Config.TLSConfig // the httptest.Server has its own copy of this TLS config
		ts.StartTLS()
		ts2.Start()
		defer ts2.Close()
		t.Logf("Running test server for curl to hit at: %s and %s", ts.URL, ts2.URL)
	}
	defer ts.Close()
	defer func() { testHookOnConn = nil }()
	for _, mode := range modes {
		var gotConn int32

		testHookOnConn = func() { atomic.StoreInt32(&gotConn, 1) }

		T := ts
		if mode == testUpgradeNoTLS && ts2 != nil {
			T = ts2
		}
		container := curl(t, "-D-", "--silent", "--http2", "--insecure", T.URL)
		defer kill(container)
		resc := make(chan interface{}, 1)
		go func() {
			res, err := dockerLogs(container)
			if err != nil {
				resc <- err
			} else {
				resc <- res
			}
		}()
		select {
		case res := <-resc:
			if err, ok := res.(error); ok {
				t.Fatal(err)
			}
			testDualUpgradeOutput(t, string(res.([]byte)), (T.TLS == nil),
				"HTTP/2.0 200",
				"foo:Bar",
				"client-proto:HTTP/2",
				msg)
		case <-time.After(3 * time.Second):
			t.Errorf("timeout waiting for curl")
		}

		if atomic.LoadInt32(&gotConn) == 0 {
			t.Error("never saw an http2 connection")
		}
	}
}

func testDualUpgradeOutput(t *testing.T, out string, isUpgrade bool, must ...string) {
	if isUpgrade {
		if !strings.HasPrefix(out, "HTTP/1.1 101 Switching Protocols") {
			t.Error("didn't see prefix 'HTTP/1.1 101 Switching Protocols'")
			t.Logf("Got: %s", out)
		}
		if !strings.Contains(out, "Upgrade: h2c") {
			t.Error("didn't see HTTP/1.1 Upgrade header")
			t.Logf("Got: %s", out)
		}
	}
	for _, s := range must {
		if !strings.Contains(out, s) {
			t.Errorf("didn't see %q", s)
			t.Logf("Got: %s", out)
		}
	}
}

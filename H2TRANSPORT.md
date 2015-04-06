# Quick reference to http2 transport

This fork includes my simple-dirty first approach to http2 `RoundTripper` for go http.

Supports:
  * https origins, https proxies to connect to http origins (CONNECT not supported)
  * fallback to another transport if HTTP2 connection cannot be established
  * gzip compression
  * push streams

Simple example:
```golang

tr := http2.Transport{
  // You can define https proxy.
  Proxy: func(*http.Request) (*url.URL, error) {
    return url.Parse("https://myproxy.com"), nil
  },

  // Use standard client if connection fails
  Fallback: http.DefaultTransport,

  // Override tls config
  TLSClientConfig: &tls.Config{ ... },

  // if you don't want to use gzip
  DisableCompression: true,

  // timeout?
  ResponseHeaderTimeout: time.Seconds(5),

  // Handshake timeout?
  TLSHandshakeTimeout: time.Seconds(2),

  // Max streams per connection. Other requests will be queued to wait for a free slot.
  // Default is 100.
  MaxConcurrentStreams: 200,

  // If you want to limit frame size.
  MaxReadFrameSize: 32768,

  // Callback for push streams. It is called on separate goroutine.
  // NOTE: p.Associated is a pointer to original *http.Request, so many concurrency
  // problems may appear here, but for now it is left as is, since it is only first implementation :)
  PushHandler: func(p PushPromise) {
    if p.Request.URL.Path != "/expectedpush" {
      p.Cancel()
    } else if p.Associated.URL.Host != "mypushinghost" {
      p.Cancel()
    } else {
      // nil means that pushes associated with that push stream will be rejected.
      // Pass same func here to handle nested pushes.
      res, err := p.Resolve(nil)
    }
  },
}
client := http.Client{Transport: tr}
defer tr.CloseIdleConnections()

res, err := client.Get("https://http2.golang.org/")
```

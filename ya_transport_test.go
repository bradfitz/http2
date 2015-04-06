package http2

import (
  "fmt"
  "io/ioutil"
  "net/http"
  "strings"
  "sync"
  "sync/atomic"
  "testing"
)

func TestThatItWorksWithGET(t *testing.T) {
  tr := &YaTransport{}
  c := &http.Client{Transport: tr}
  res, err := c.Get("https://http2.golang.org/")
  if err != nil {
    t.Fatalf("%v", err)
  }

  if g, w := res.StatusCode, 200; g != w {
    t.Errorf("StatusCode = %v; want %v", g, w)
  }
  fmt.Println(res.Header)
  b, err := ioutil.ReadAll(res.Body)
  if int64(len(b)) != res.ContentLength {
    t.Errorf("Content-Length = %v; want %v", len(b), res.ContentLength) 
  }
  fmt.Println(string(b))
}

func TestThatItWorksWithPUT(t *testing.T) {
  tr := &YaTransport{}
  c := &http.Client{Transport: tr}
  req, err := http.NewRequest("PUT", "https://http2.golang.org/crc32", strings.NewReader("Hello, world!"))
  if err != nil {
    t.Fatalf("%v", err)
  }
  res, err := c.Do(req)
  if err != nil {
    t.Fatalf("%v", err)
  }

  if g, w := res.StatusCode, 200; g != w {
    t.Errorf("StatusCode = %v; want %v", g, w)
  }
  fmt.Println(res.Header)
  b, err := ioutil.ReadAll(res.Body)
  if int64(len(b)) != res.ContentLength {
    t.Errorf("Content-Length = %v; want %v", len(b), res.ContentLength) 
  }
  fmt.Println(string(b))
}

func TestThatItWorksConcurrently(t *testing.T) {
  tr := &YaTransport{DisableCompression: false}
  c := &http.Client{Transport: tr}
  wg := sync.WaitGroup{}
  numReq := 300
  var counter int32
  counter = 0
  wg.Add(numReq)
  for i := 0; i < numReq; i++ {
    go func(i int) {
      defer wg.Done()
      res, err := c.Get("https://http2.golang.org/")
      if err != nil {
        t.Fatalf("%v", err)
      }
      if g, w := res.StatusCode, 200; g != w {
        t.Errorf("StatusCode = %v; want %v", g, w)
      }
      b, err := ioutil.ReadAll(res.Body)
      if int64(len(b)) != res.ContentLength {
        t.Errorf("Content-Length = %v; want %v", len(b), res.ContentLength) 
      }
      cnt := atomic.AddInt32(&counter, 1)
      fmt.Printf("%v: %v\n", res.Header, cnt)
    }(i)
  }
  wg.Wait()
}

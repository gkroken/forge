package format

import (
	"bytes"
	"io"
	"net/http"
)

// Capture is a minimal http.ResponseWriter that records a response so it can
// be replayed to another writer. Group handlers use this to probe each member
// and forward the first successful response to the real client.
type Capture struct {
	HeaderMap http.Header
	Code      int
	Body      bytes.Buffer
}

func NewCapture() *Capture { return &Capture{HeaderMap: make(http.Header)} }

func (c *Capture) Header() http.Header         { return c.HeaderMap }
func (c *Capture) Write(b []byte) (int, error) { return c.Body.Write(b) }
func (c *Capture) WriteHeader(code int)        { c.Code = code }

// OK reports whether the captured response was successful (2xx or implicit 200).
func (c *Capture) OK() bool { return c.Code == 0 || (c.Code >= 200 && c.Code < 300) }

// Replay copies the captured headers and body to w.
func (c *Capture) Replay(w http.ResponseWriter) {
	for k, vs := range c.HeaderMap {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	io.Copy(w, &c.Body)
}

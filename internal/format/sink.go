package format

import "net/http"

// Sink forwards a member repository's response to the real client, but only
// once that member has answered successfully. The status — explicit, or the
// 200 implied by the first Write — decides, and from that moment bytes stream
// straight through.
//
// It replaces a recorder that buffered the whole response to decide whether to
// replay it: a group serving a 700 MB artifact held all of it in memory, per
// concurrent request, to answer a question the status line already answered. A
// failed member's body is discarded rather than buffered, which is what the
// caller did with it anyway.
type Sink struct {
	w         http.ResponseWriter
	hdr       http.Header
	committed bool
	failed    bool
}

func NewSink(w http.ResponseWriter) *Sink {
	return &Sink{w: w, hdr: make(http.Header)}
}

func (s *Sink) Header() http.Header { return s.hdr }

func (s *Sink) WriteHeader(code int) {
	if s.committed || s.failed {
		return
	}
	if code < 200 || code >= 300 {
		s.failed = true
		return
	}
	s.commit(code)
}

func (s *Sink) Write(b []byte) (int, error) {
	if s.failed {
		return len(b), nil // swallowed: this member is not the one serving
	}
	if !s.committed {
		s.commit(http.StatusOK)
	}
	return s.w.Write(b)
}

func (s *Sink) commit(code int) {
	for k, vs := range s.hdr {
		for _, v := range vs {
			s.w.Header().Add(k, v)
		}
	}
	s.w.WriteHeader(code)
	s.committed = true
}

// Flush passes through so a member streaming a large body is not held up.
func (s *Sink) Flush() {
	if f, ok := s.w.(http.Flusher); ok && s.committed {
		f.Flush()
	}
}

// Served reports whether this member answered and its response reached the
// client. A handler that wrote nothing at all is treated as a success with an
// empty body, matching Capture's reading of an unset status code.
func (s *Sink) Served() bool {
	if s.failed {
		return false
	}
	if !s.committed {
		s.commit(http.StatusOK)
	}
	return true
}

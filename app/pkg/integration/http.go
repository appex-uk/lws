package integration

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

func slogRequest(r *http.Request, rec *statusRecorder, d time.Duration) {
	slog.InfoContext(r.Context(), "request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", rec.status,
		"bytes", rec.bytes,
		"durationMs", float64(d.Microseconds())/1000,
		"remote", r.RemoteAddr,
		"userAgent", r.UserAgent(),
	)
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader && code >= 200 {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// ReadFrom preserves the sendfile fast path of the underlying writer.
func (s *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	s.wroteHeader = true
	var n int64
	var err error
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(r)
	} else {
		n, err = io.Copy(s.ResponseWriter, r)
	}
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

package master

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tumult/gosmokeping/internal/cluster"
	"github.com/tumult/gosmokeping/internal/config"
)

// errReader fails the way a body that stopped arriving does: some bytes land,
// then the read errors. A truncated JSON document alone would not reproduce it
// — encoding/json reports io.ErrUnexpectedEOF for that, while a reset
// connection or a read deadline surfaces the transport's own error.
type errReader struct {
	prefix string
	n      int
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.n < len(r.prefix) {
		n := copy(p, r.prefix[r.n:])
		r.n += n
		return n, nil
	}
	return 0, errors.New("connection reset by peer")
}

func (r *errReader) Close() error { return nil }

// The slave's drop/retry split keys on status and on HeaderRefusal, so which
// one each decode failure produces decides between a requeue and permanent data
// loss. TestMasterDecodeStatusesLandOnTheIntendedSide asserts the slave half of
// that contract; this asserts the master half, which is the side that chooses.
func TestRefuseDecodeDistinguishesTransportFromContent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        io.ReadCloser
		wantStatus  int
		wantRefusal bool
		why         string
	}{
		{
			name:        "transport error requeues",
			body:        &errReader{prefix: `{"source":"s1","cycles":[`},
			wantStatus:  http.StatusRequestTimeout,
			wantRefusal: false,
			why:         "a body that stopped arriving must be retried, and 408 is the only status in retryable4xx that says so",
		},
		{
			name:        "malformed json is permanent",
			body:        io.NopCloser(strings.NewReader(`{"source":"s1","cycles":[ not json ]}`)),
			wantStatus:  http.StatusBadRequest,
			wantRefusal: true,
			why:         "bytes the master will refuse identically forever must carry the marker so the slave stops resending them",
		},
		{
			name:        "wrong type is permanent",
			body:        io.NopCloser(strings.NewReader(`{"source":42,"cycles":[]}`)),
			wantStatus:  http.StatusBadRequest,
			wantRefusal: true,
			why:         "an UnmarshalTypeError is a content error, not a transport one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer()
			req := httptest.NewRequest(http.MethodPost, "/cycles", tc.body)
			req.Header.Set("Authorization", "Bearer tok")
			req.Header.Set("X-Slave-Name", "s1")
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d — %s", rec.Code, tc.wantStatus, tc.why)
			}
			gotRefusal := rec.Header().Get(cluster.HeaderRefusal) == cluster.RefusalPermanent
			if gotRefusal != tc.wantRefusal {
				t.Errorf("permanent-refusal marker = %v, want %v — %s", gotRefusal, tc.wantRefusal, tc.why)
			}
		})
	}
}

// Driven directly rather than through the handler: tripping MaxBytesReader for
// real means pushing maxCyclesBody (100 MiB) through a test, and the branch
// under test is the classification, not the reader.
func TestRefuseDecodeAnswersOversizeWithoutTheMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	refuseDecode(rec, &http.MaxBytesError{Limit: maxCyclesBody})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for a body past the ingest cap", rec.Code)
	}
	// 413 sits outside retryable4xx, so the slave already drops the batch; adding
	// the marker would escalate that to exiting the process at boot.
	if rec.Header().Get(cluster.HeaderRefusal) == cluster.RefusalPermanent {
		t.Error("oversize body carried the permanent marker, which exits the slave rather than dropping one batch")
	}
}

// A registration body fails the same way a cycle body does, and registerForever
// exits the process on a permanent refusal, so the split has to hold on both
// handlers rather than only on the one that carries measurements.
func TestRefuseDecodeAppliesToRegisterToo(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/register", &errReader{prefix: `{"name":"s1"`})
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestTimeout {
		t.Errorf("status = %d, want 408: a dropped registration body must be retried, not exit the slave", rec.Code)
	}
	if rec.Header().Get(cluster.HeaderRefusal) == cluster.RefusalPermanent {
		t.Error("transport failure carried the permanent marker, which exits the slave non-zero at boot")
	}
}

// cluster.source is read through store.Current() on every request, so a SIGHUP
// can make a running slave's name collide without that slave changing anything.
// The refusal has to be one the peer recovers from on its own.
func TestNameCollisionWithMasterSourceIsRetryable(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := config.NewStore("", &config.Config{
		Cluster: &config.Cluster{Token: "tok", Source: "eu-fra"},
	})
	srv := NewServer(log, store, NewRegistry(slog.New(slog.DiscardHandler)), nopSink{}, nil)

	for _, path := range []string{"/register", "/cycles"} {
		body := `{"name":"eu-fra"}`
		if path == "/cycles" {
			body = `{"source":"eu-fra","cycles":[]}`
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("X-Slave-Name", "eu-fra")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 so the slave retries until an operator renames one side",
				path, rec.Code)
		}
		if rec.Header().Get(cluster.HeaderRefusal) == cluster.RefusalPermanent {
			t.Errorf("%s: collision carried the permanent marker, which exits a slave a master-side SIGHUP just renamed into",
				path)
		}
	}
}

// The collision refusal must not swallow the invalid-name one it was split out
// of: an unusable name is still answered permanently, because no retry fixes it.
func TestInvalidSlaveNameStaysPermanent(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"name":"bad\nname"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a name the master refuses identically forever", rec.Code)
	}
	if rec.Header().Get(cluster.HeaderRefusal) != cluster.RefusalPermanent {
		t.Error("invalid name lost the permanent marker, so the slave retries bytes that can never succeed")
	}
}

package problem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decode(t *testing.T, rec *httptest.ResponseRecorder) Details {
	t.Helper()
	var got Details
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return got
}

func assertHeaders(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d", rec.Code, status)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, ContentType)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestHelpers(t *testing.T) {
	tests := []struct {
		name     string
		write    func(http.ResponseWriter, *http.Request, string)
		status   int
		wantType string
	}{
		{"bad request", BadRequest, http.StatusBadRequest, TypeBadRequest},
		{"not found", NotFound, http.StatusNotFound, TypeNotFound},
		{"conflict", Conflict, http.StatusConflict, TypeConflict},
		{"internal server error", InternalServerError, http.StatusInternalServerError, TypeInternalServer},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/sessions/sess_01HXYZ", nil)
			rec := httptest.NewRecorder()

			tc.write(rec, req, "something went wrong")

			assertHeaders(t, rec, tc.status)
			want := Details{
				Type:     tc.wantType,
				Title:    http.StatusText(tc.status),
				Status:   tc.status,
				Detail:   "something went wrong",
				Instance: "/sessions/sess_01HXYZ",
			}
			if got := decode(t, rec); got != want {
				t.Errorf("body = %+v, want %+v", got, want)
			}
		})
	}
}

func TestWriteProblemSessionBusyExample(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess_01HXYZ/prompts?x=1", nil)
	rec := httptest.NewRecorder()

	WriteProblem(rec, req, http.StatusConflict,
		"https://grokbot.local/problems/session-busy",
		"Session Turn Already Running",
		"Session sess_01HXYZ is currently executing an active turn.")

	assertHeaders(t, rec, http.StatusConflict)
	want := Details{
		Type:     "https://grokbot.local/problems/session-busy",
		Title:    "Session Turn Already Running",
		Status:   http.StatusConflict,
		Detail:   "Session sess_01HXYZ is currently executing an active turn.",
		Instance: "/sessions/sess_01HXYZ/prompts",
	}
	if got := decode(t, rec); got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, field := range []string{"type", "title", "status", "detail", "instance"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("missing field %q in %s", field, rec.Body.String())
		}
	}
}

func TestWriteProblemNilRequest(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteProblem(rec, nil, http.StatusInternalServerError, TypeInternalServer, "Internal Server Error", "boom")

	assertHeaders(t, rec, http.StatusInternalServerError)
	if got := decode(t, rec); got.Instance != "" {
		t.Errorf("instance = %q, want empty", got.Instance)
	}
}

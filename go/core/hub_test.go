package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTransportOutcomes(t *testing.T) {
	for _, status := range []int{200, 301, 400, 401, 403, 409, 413, 422, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/rpc" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
					t.Error("transport binding")
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"ok":true,"result":{"value":1}}`)
			}))
			defer server.Close()
			hub, e := NewHTTPHub(server.URL, strings.Repeat("x", 32), true)
			if e != nil {
				t.Fatal(e)
			}
			_, e = hub.Call(context.Background(), RPCRequest{Op: "list"})
			want := status == 400 || status == 403 || status == 409 || status == 413 || status == 422
			if Definite(e) != want || (status == 200) != (e == nil) {
				t.Fatalf("status %d error %v", status, e)
			}
		})
	}
}
func TestInvalidResponsesRetain(t *testing.T) {
	for _, body := range []string{`{`, `[]`, `{"ok":true}`, strings.Repeat(" ", MaxResponse+1)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		hub, _ := NewHTTPHub(server.URL, strings.Repeat("x", 32), true)
		_, e := hub.Call(context.Background(), RPCRequest{Op: "list"})
		server.Close()
		if e == nil || Definite(e) {
			t.Fatal("invalid response must retain command")
		}
	}
}
func TestRequestWindow(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	for _, offset := range []time.Duration{-RequestWindow, -RequestWindow + time.Millisecond, ClockSkew, ClockSkew + time.Millisecond} {
		id, _ := NewRequestID(now.Add(offset))
		e := ValidateRequestWindow(id, now, time.Time{}, time.Time{})
		if (e == nil) != (offset > -RequestWindow && offset <= ClockSkew) {
			t.Fatalf("window %v %v", offset, e)
		}
	}
	id, _ := NewRequestID(now)
	if ValidateRequestWindow(id, now, now.Add(RequestWindow), time.Time{}) == nil {
		t.Fatal("forgotten floor")
	}
	if ValidateRequestWindow("00000000-0000-4000-8000-000000000001", now, time.Time{}, now) == nil {
		t.Fatal("legacy expiry")
	}
}
func TestOrigin(t *testing.T) {
	for _, s := range []string{"http://example.invalid", "https://example.invalid/path", "https://user@example.invalid", "https://example.invalid/?x", "https://example.invalid/#"} {
		if _, e := ValidateOrigin(s, true); e == nil {
			t.Fatal("accepted invalid origin")
		}
	}
	b, _ := json.Marshal(RPCRequest{Op: "list"})
	if len(b) == 0 {
		t.Fatal("encoding")
	}
}

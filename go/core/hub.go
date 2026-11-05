package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const MaxResponse = 32 << 20

// RemoteError is a definite rejection; every other error retains the command.
type RemoteError struct{ Text string }

func (e *RemoteError) Error() string { return e.Text }
func Definite(err error) bool        { var e *RemoteError; return errors.As(err, &e) }

type HTTPHub struct {
	origin, token string
	client        *http.Client
}

func ValidateOrigin(origin string, loopback bool) (string, error) {
	u, e := url.Parse(origin)
	if e != nil {
		return "", errors.New("hub URL must be an origin without credentials, path, query or fragment")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(origin, "#") || (u.Path != "" && u.Path != "/") {
		return "", errors.New("hub URL must be an origin without credentials, path, query or fragment")
	}
	if u.Hostname() == "" {
		return "", errors.New("hub URL needs a hostname")
	}
	host := u.Hostname()
	if u.Scheme != "https" && !(loopback && u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return "", errors.New("HTTPS required (HTTP allowed only for explicit loopback development)")
	}
	return strings.TrimRight(origin, "/"), nil
}
func NewHTTPHub(origin, token string, loopback bool) (*HTTPHub, error) {
	origin, e := ValidateOrigin(origin, loopback)
	if e != nil {
		return nil, e
	}
	if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("invalid node or token (minimum 32 non-whitespace characters)")
	}
	return &HTTPHub{origin, token, &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (h *HTTPHub) Call(ctx context.Context, r RPCRequest) (json.RawMessage, error) {
	if len(r.Data) == 0 {
		r.Data = json.RawMessage(`{}`)
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(r.Data, &data) != nil || data == nil {
		return nil, &RemoteError{"invalid request fields"}
	}
	body, e := json.Marshal(r)
	if e != nil {
		return nil, &RemoteError{"invalid request fields"}
	}
	if len(body) > 262144 {
		return nil, &RemoteError{"request too large"}
	}
	req, e := http.NewRequestWithContext(ctx, "POST", h.origin+"/v2/rpc", bytes.NewReader(body))
	if e != nil {
		return nil, errors.New("hub unavailable; requests remain queued")
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	response, e := h.client.Do(req)
	if e != nil {
		return nil, errors.New("hub unavailable; requests remain queued")
	}
	defer response.Body.Close()
	definite := response.StatusCode == 400 || response.StatusCode == 403 || response.StatusCode == 409 || response.StatusCode == 413 || response.StatusCode == 422
	if response.StatusCode != 200 && !definite {
		return nil, fmt.Errorf("hub HTTP %d; requests remain queued", response.StatusCode)
	}
	bound := int64(MaxResponse)
	if definite {
		bound = 65536
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, bound+1))
	if e != nil {
		return nil, errors.New("hub response interrupted; original request retained")
	}
	if definite {
		var v struct{ Error string }
		_ = json.Unmarshal(raw, &v)
		if v.Error == "" {
			v.Error = "request rejected"
		}
		return nil, &RemoteError{v.Error}
	}
	if int64(len(raw)) > bound {
		return nil, errors.New("hub response too large")
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, errors.New("invalid hub response")
	}
	if string(value["ok"]) != "true" {
		var text string
		_ = json.Unmarshal(value["error"], &text)
		if text == "" {
			text = "request rejected"
		}
		return nil, &RemoteError{text}
	}
	result, ok := value["result"]
	if !ok {
		return nil, errors.New("invalid hub response")
	}
	return result, nil
}
func RPC(ctx context.Context, h HubClient, op string, data any, out any) error {
	b, e := json.Marshal(data)
	if e != nil {
		return e
	}
	r, e := h.Call(ctx, RPCRequest{Op: op, Data: b})
	if e != nil {
		return e
	}
	if out != nil {
		if json.Unmarshal(r, out) != nil {
			return errors.New("invalid hub response")
		}
	}
	return nil
}

// ValidateRequestWindow does not refresh legacy IDs or retry identities.
func ValidateRequestWindow(id string, now time.Time, floor, legacyDeadline time.Time) error {
	created, e := RequestCreated(id)
	if e != nil {
		return e
	}
	if created == nil {
		if !now.Before(legacyDeadline) {
			return errors.New("legacy request grace period ended; upgrade the client and reconcile queued requests")
		}
		return nil
	}
	expiry := created.Add(RequestWindow)
	if !now.Before(expiry) || created.After(now.Add(ClockSkew)) || !floor.Before(expiry) {
		return errors.New("request ID is expired or from a future clock; reconcile before creating a new request")
	}
	return nil
}

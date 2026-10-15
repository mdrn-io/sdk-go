package mdrn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Auth is who is calling: the signed-in visitor of a request MDRN delivered.
type Auth struct {
	c *Client
}

// User is the signed-in visitor of a request: their id and the short-lived identity token
// that stands for their session.
type User struct {
	ID    string
	Token string
}

// Profile is what MDRN Identity knows about the visitor. Email and Name are set when known;
// UserMetadata and AppMetadata are the application's JSON when MetadataState is "available".
type Profile struct {
	ID            string          `json:"id"`
	Email         string          `json:"email,omitempty"`
	Name          string          `json:"name,omitempty"`
	UserMetadata  json.RawMessage `json:"user_metadata,omitempty"`
	AppMetadata   json.RawMessage `json:"app_metadata,omitempty"`
	MetadataState string          `json:"metadataState"`
}

var userID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// UserFrom is the signed-in visitor of a request with these headers, or nil when nobody is.
// MDRN's Proxy deletes every x-mdrn-* header a client sent before adding x-mdrn-user-id and
// x-mdrn-identity-token for a signed-in visitor, so they can be trusted on a request MDRN
// delivered (not on one that reached a development server directly).
func UserFrom(h http.Header) *User {
	id, token := h.Get("x-mdrn-user-id"), h.Get("x-mdrn-identity-token")
	if id == "" || token == "" || !userID.MatchString(id) {
		return nil
	}
	return &User{ID: id, Token: token}
}

// Me is the profile of the request's signed-in visitor, from MDRN Identity (GET /v1/auth/me
// at the request's origin, with the visitor's token), or nil when nobody is signed in or the
// session has ended (a 401 or 404). h are the request's headers and requestURL its URL.
func (a *Auth) Me(ctx context.Context, h http.Header, requestURL string) (*Profile, error) {
	user := UserFrom(h)
	if user == nil {
		return nil, nil
	}
	u, err := url.Parse(requestURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, argument("requestURL must be an absolute URL")
	}
	r := request{method: http.MethodGet, url: u.Scheme + "://" + u.Host + "/v1/auth/me", token: user.Token, project: a.c.Project, environment: a.c.Environment}
	resp, err := a.c.exchange(ctx, a.c.http, r.build)
	if err != nil {
		return nil, err
	}
	if resp.status == http.StatusUnauthorized || resp.status == http.StatusNotFound {
		return nil, nil
	}
	var profile Profile
	if err := resp.decode(&profile); err != nil {
		return nil, err
	}
	return &profile, nil
}

// ScheduledRun is one run of a schedule declared in mdrn.jsonc, as MDRN's request for it
// carries it. RunID is the same on every attempt of one run, so use it as the idempotency key
// of the work. Attempt counts from 1.
type ScheduledRun struct {
	Schedule      string
	RunID         string
	ScheduledTime time.Time
	Attempt       int
}

// ScheduledRunFrom is the schedule run a request with these headers is, or nil for any other
// request. MDRN strips every x-mdrn-* header from public requests, so only MDRN can send
// these.
func ScheduledRunFrom(h http.Header) *ScheduledRun {
	schedule, runID := h.Get("x-mdrn-schedule"), h.Get("x-mdrn-run-id")
	if schedule == "" || runID == "" {
		return nil
	}
	run := &ScheduledRun{Schedule: schedule, RunID: runID, Attempt: 1}
	run.ScheduledTime, _ = time.Parse(time.RFC3339, h.Get("x-mdrn-scheduled-time"))
	if attempt, err := strconv.Atoi(h.Get("x-mdrn-attempt")); err == nil && attempt > 0 {
		run.Attempt = attempt
	}
	return run
}

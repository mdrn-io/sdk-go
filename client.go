// Package mdrn is the MDRN SDK for Go: one client for the products a deployed MDRN app has
// (Database, Files, Mail) and for who is calling it (Auth). It needs only the standard library.
// The wire contract is sdk/contracts/client.openapi.json; the method names are
// sdk/contracts/operations.json, shared with the JavaScript, Python and Rust SDKs, and every SDK
// produces the same transcript against sdk/conformance.
//
// A client comes from the environment MDRN gives a release (FromEnv) or from a Config (New).
// Every failed call returns an *Error with the HTTP status (0 when no response was received)
// and a code: the code of MDRN's error envelope, HTTP_ERROR when the body is not one,
// UNEXPECTED_REDIRECT for a 3xx (never followed, so the token cannot reach another origin),
// PRODUCT_NOT_ENABLED for a product the release does not have, TIMEOUT and NETWORK_ERROR for
// the transport. A call the SDK refuses before sending anything returns an *ArgumentError.
// Product URLs must be https; plain http is accepted only to localhost, 127.0.0.1 or [::1].
package mdrn

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Tokens are the product tokens of a release: MDRN_DATABASE_TOKEN, MDRN_STORAGE_TOKEN (Files)
// and MDRN_MAIL_TOKEN. A product without one is not enabled. They are workload credentials,
// never for a browser.
type Tokens struct {
	Database, Storage, Mail string
}

// URLs are product URLs that replace the ones derived from the origin: MDRN_DATABASE_URL,
// MDRN_STORAGE_URL and MDRN_MAIL_URL, as FromEnv passes them.
type URLs struct {
	Database, Storage, Mail string
}

// Config is what New builds a client from.
type Config struct {
	// Origin is the release's origin (https://<your app>); each product's URL is derived from
	// it (/_mdrn/v1/database, /v1/storage, /v1/mail). A token without it, and without that
	// product's entry in URLs, enables nothing.
	Origin string
	// Project is MDRN_PROJECT_ID; required.
	Project string
	// Environment is MDRN_ENVIRONMENT: "sandbox" or "production".
	Environment string
	// Tokens are the product tokens; a product without one is not enabled.
	Tokens Tokens
	// URLs replace the product URLs derived from Origin.
	URLs URLs
	// HTTP sends every product request (Database, Files, Mail, Auth.Me). The default is an
	// http.Client that never follows a redirect. A Doer of your own must not follow one either.
	HTTP Doer
	// Transfer sends bytes to and from grant URLs (the Files gateway): always a plain HTTP
	// client, never one that adds a product token, and never a Worker binding. The default
	// is a plain http.Client.
	Transfer Doer
	// Timeout is the deadline of each request, 10 s when zero. It is applied through the
	// request's context, so it holds for any Doer.
	Timeout time.Duration
}

// Client is an MDRN app's client. DB, Files, Mail and Auth are its products; a call on a
// product the client does not have returns PRODUCT_NOT_ENABLED without sending anything.
type Client struct {
	Project     string
	Environment string
	DB          *DB
	Files       *Files
	Mail        *Mail
	Auth        *Auth

	db, files, mail *product
	http, transfer  Doer
	timeout         time.Duration
}

var environments = map[string]bool{"sandbox": true, "production": true}

// The token is a bearer credential: it only travels over https, except to a development server
// on this machine (localhost, 127.0.0.1 or [::1]).
var loopback = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// secureURL is value without a trailing slash, or an ArgumentError when it is not an https URL
// (or http to loopback).
func secureURL(value, what string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && loopback[u.Hostname()])) {
		return "", argument("%s must be an https URL (http is allowed only for localhost, 127.0.0.1 or [::1])", what)
	}
	return strings.TrimRight(value, "/"), nil
}

// New is a client for the products the config has tokens for. It refuses, with an
// ArgumentError, a missing project, an environment that is not sandbox or production, and a
// product URL that is not https (except http to localhost, 127.0.0.1 or [::1]).
func New(cfg Config) (*Client, error) {
	if cfg.Project == "" {
		return nil, argument("project is required")
	}
	if !environments[cfg.Environment] {
		return nil, argument("environment must be sandbox or production")
	}
	if cfg.Timeout < 0 {
		return nil, argument("timeout must not be negative")
	}
	base := ""
	if cfg.Origin != "" {
		var err error
		if base, err = secureURL(cfg.Origin, "origin"); err != nil {
			return nil, err
		}
	}
	c := &Client{Project: cfg.Project, Environment: cfg.Environment, http: cfg.HTTP, transfer: cfg.Transfer, timeout: cfg.Timeout}
	if c.timeout == 0 {
		c.timeout = defaultTimeout
	}
	if c.http == nil {
		c.http = defaultClient()
	}
	if c.transfer == nil {
		c.transfer = defaultClient()
	}
	var err error
	if c.db, err = newProduct("Database", "DATABASE", "/_mdrn/v1/database", base, cfg.URLs.Database, cfg.Tokens.Database); err != nil {
		return nil, err
	}
	if c.files, err = newProduct("Files", "STORAGE", "/v1/storage", base, cfg.URLs.Storage, cfg.Tokens.Storage); err != nil {
		return nil, err
	}
	if c.mail, err = newProduct("Mail", "MAIL", "/v1/mail", base, cfg.URLs.Mail, cfg.Tokens.Mail); err != nil {
		return nil, err
	}
	c.DB = &DB{c: c}
	c.Files = &Files{c: c, Uploads: &Uploads{c: c}}
	c.Mail = &Mail{c: c}
	c.Auth = &Auth{c: c}
	return c, nil
}

// newProduct is one product's URL and token. A token without a URL (no origin, no URLs entry)
// enables nothing: the product is not enabled, and a call on it says which MDRN_*_URL is
// missing.
func newProduct(label, variable, path, base, given, token string) (*product, error) {
	u := given
	if u == "" && token != "" && base != "" {
		u = base + path
	}
	if u != "" {
		var err error
		if u, err = secureURL(u, "MDRN_"+variable+"_URL"); err != nil {
			return nil, err
		}
	}
	missing := "URL"
	if token == "" {
		missing = "TOKEN"
	}
	p := &product{unavailable: label + " is not enabled for this environment (MDRN_" + variable + "_" + missing + " is missing)"}
	if u != "" && token != "" {
		p.url, p.token = u, token
	}
	return p, nil
}

// defaultClient never follows a redirect: a 3xx comes back as it is, and is refused.
func defaultClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// FromEnv is the client of a deployed release, from what MDRN put into its environment:
// MDRN_PROJECT_ID, MDRN_ENVIRONMENT, and one MDRN_<PRODUCT>_URL and MDRN_<PRODUCT>_TOKEN per
// product switched on for the app (DATABASE, STORAGE for Files, MAIL).
func FromEnv() (*Client, error) {
	return New(Config{
		Project:     os.Getenv("MDRN_PROJECT_ID"),
		Environment: os.Getenv("MDRN_ENVIRONMENT"),
		Tokens:      Tokens{Database: os.Getenv("MDRN_DATABASE_TOKEN"), Storage: os.Getenv("MDRN_STORAGE_TOKEN"), Mail: os.Getenv("MDRN_MAIL_TOKEN")},
		URLs:        URLs{Database: os.Getenv("MDRN_DATABASE_URL"), Storage: os.Getenv("MDRN_STORAGE_URL"), Mail: os.Getenv("MDRN_MAIL_URL")},
	})
}

// Enabled lists the products the client has, among "db", "files" and "mail".
func (c *Client) Enabled() []string {
	enabled := []string{}
	for _, p := range []struct {
		name string
		p    *product
	}{{"db", c.db}, {"files", c.files}, {"mail", c.mail}} {
		if p.p.url != "" {
			enabled = append(enabled, p.name)
		}
	}
	return enabled
}

package mdrn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// DB is the Database product: JSON records in named collections.
type DB struct {
	c *Client
}

// Record is a JSON object in a collection, with an id MDRN assigns and a version that grows by
// one on every change. Data is the object as stored; Decode reads it into a value of yours.
type Record struct {
	ID         string          `json:"id"`
	Collection string          `json:"collection"`
	Version    uint64          `json:"version"`
	Data       json.RawMessage `json:"data"`
	CreatedAt  time.Time       `json:"createdAt"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// Decode reads the record's data into v, as json.Unmarshal does.
func (r *Record) Decode(v any) error { return json.Unmarshal(r.Data, v) }

type recordInput struct {
	Data any `json:"data"`
}

func recordsPath(collection string) string {
	return "/collections/" + segment(collection) + "/records"
}

func recordPath(collection, id string) string {
	return recordsPath(collection) + "/" + segment(id)
}

func checkCollection(collection string) error {
	if collection == "" {
		return argument("collection is required")
	}
	return nil
}

func checkRecord(collection, id string) error {
	if err := checkCollection(collection); err != nil {
		return err
	}
	if id == "" {
		return argument("id is required")
	}
	return nil
}

// Create stores data (any value that encodes as a JSON object) as a new record of the
// collection. Options: Key.
func (d *DB) Create(ctx context.Context, collection string, data any, opts ...Option) (*Record, error) {
	o, err := options(opts)
	if err != nil {
		return nil, err
	}
	if err := checkCollection(collection); err != nil {
		return nil, err
	}
	var record Record
	if err := d.c.call(ctx, d.c.db, http.MethodPost, recordsPath(collection), recordInput{Data: data}, o.keyOrNew(), 0, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

// Get is the record, or nil (and no error) when there is none.
func (d *DB) Get(ctx context.Context, collection, id string) (*Record, error) {
	if err := checkRecord(collection, id); err != nil {
		return nil, err
	}
	var record Record
	found, err := d.c.read(ctx, d.c.db, recordPath(collection, id), &record)
	if err != nil || !found {
		return nil, err
	}
	return &record, nil
}

// List is one page of the collection's records.
func (d *DB) List(ctx context.Context, collection string, page PageOptions) (*Page[Record], error) {
	if err := checkCollection(collection); err != nil {
		return nil, err
	}
	var result Page[Record]
	if err := d.c.call(ctx, d.c.db, http.MethodGet, recordsPath(collection)+page.query(), nil, "", 0, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// Update merges patch (a JSON object) into the record's data, a shallow merge, and answers the
// record as it is now. Options: Key, AtVersion. With AtVersion the change is sent once with
// that If-Match, and a 409 CONFLICT is yours to handle. Without it the record is read first and
// the change sent with the version read; if the record changed in between (409) the pair is
// repeated, up to three more times.
func (d *DB) Update(ctx context.Context, collection, id string, patch any, opts ...Option) (*Record, error) {
	o, err := options(opts)
	if err != nil {
		return nil, err
	}
	if err := checkRecord(collection, id); err != nil {
		return nil, err
	}
	var record Record
	err = d.mutate(ctx, collection, id, o, func(version uint64, key string) error {
		return d.c.call(ctx, d.c.db, http.MethodPatch, recordPath(collection, id), recordInput{Data: patch}, key, version, &record)
	})
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// Delete removes the record. Options: Key, AtVersion, with the same conflict handling as
// Update.
func (d *DB) Delete(ctx context.Context, collection, id string, opts ...Option) error {
	o, err := options(opts)
	if err != nil {
		return err
	}
	if err := checkRecord(collection, id); err != nil {
		return err
	}
	return d.mutate(ctx, collection, id, o, func(version uint64, key string) error {
		return d.c.call(ctx, d.c.db, http.MethodDelete, recordPath(collection, id), nil, key, version, nil)
	})
}

// mutate is an update or delete. With a version: one request with that If-Match, never
// retried. Without: the record is read and the write sent with the version read, and on a 409
// (the record changed in between) the pair is repeated up to conflictRetries more times. Each
// attempt is its own change, so a retry gets a fresh idempotency key (MDRN refuses a key reused
// with another If-Match); the caller's key, if any, is used for the first attempt.
func (d *DB) mutate(ctx context.Context, collection, id string, o callOptions, attempt func(version uint64, key string) error) error {
	if o.hasVersion {
		return attempt(o.version, o.keyOrNew())
	}
	for retries := 0; ; retries++ {
		var current Record
		if err := d.c.call(ctx, d.c.db, http.MethodGet, recordPath(collection, id), nil, "", 0, &current); err != nil {
			return err
		}
		key := newKey()
		if retries == 0 && o.key != "" {
			key = o.key
		}
		err := attempt(current.Version, key)
		var e *Error
		if err == nil || !(errors.As(err, &e) && e.Status == http.StatusConflict && retries < conflictRetries) {
			return err
		}
	}
}

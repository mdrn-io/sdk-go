package mdrn

import (
	"crypto/rand"
	"fmt"
)

// Option adjusts one call: Key, AtVersion, NoWait. An option a call does not take is ignored.
type Option func(*callOptions)

type callOptions struct {
	key        string
	version    uint64
	hasVersion bool
	noWait     bool
}

// Key gives a write its idempotency key. A fresh UUID is generated when none is given; give
// one when you retry a change yourself and want it to happen once.
func Key(key string) Option {
	return func(o *callOptions) { o.key = key }
}

// AtVersion sends an update or delete once, with this record version as If-Match, and makes a
// 409 CONFLICT yours to handle. Without it the record is read first and a conflict retried.
// A zero version is refused with an ArgumentError.
func AtVersion(version uint64) Option {
	return func(o *callOptions) { o.version, o.hasVersion = version, true }
}

// NoWait makes Files.Upload return after one read of the object instead of waiting for MDRN
// to verify its bytes; the file is then usually still "verifying".
func NoWait() Option {
	return func(o *callOptions) { o.noWait = true }
}

// options applies the options and checks them: an invalid one is an ArgumentError, raised
// before any request.
func options(opts []Option) (callOptions, error) {
	var o callOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.hasVersion && o.version == 0 {
		return o, argument("version must be a positive integer")
	}
	return o, nil
}

// keyOrNew is the caller's idempotency key, or a fresh one.
func (o callOptions) keyOrNew() string {
	if o.key != "" {
		return o.key
	}
	return newKey()
}

// newKey is a UUID v4 from crypto/rand.
func newKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("mdrn: crypto/rand failed: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

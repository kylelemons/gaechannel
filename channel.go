package gaechannel

import (
	"strings"
)

type commonChannel struct {
	// constant-ish
	Host        string // "localhost:8080" or "gopaste.kevlar-go-test.appspot.com"
	ChannelPath string // "/_ah/channel/"

	// per-channel
	ClientID string
	Token    string
}

func newCommon(base, clientID, token string) *commonChannel {
	return &commonChannel{
		Host:        base,
		ChannelPath: "/_ah/channel/",
		ClientID:    clientID,
		Token:       token,
	}
}

// A Channel represents an implementation of the Channel API.
type Channel interface {
	// Stream streams messages from the channel over the
	// data chan.  Errors in setup, I/O, or decoding
	// may cause the stream to stop and return the offending
	// error.  Stream should usually be called as a goroutine.
	Stream(data chan<- string) error

	// Close requests that the Stream shut down, and will
	// not return until it has done so.  This operation is
	// not preemptive, and will wait until the long poll
	// exits.  It is not usually necessary to call Close.
	Close() error
}

// New returns the channel API implementation appropriate
// for the given host.  The token must have already been
// obtained from the server.
//
// Currently, the development implementation will only
// be returned if host contains "localhost", otherwise
// the production implementation will be used.
func New(host, clientID, token string) Channel {
	common := newCommon(host, clientID, token)
	if strings.Contains(host, "localhost") {
		return newDevChannel(common)
	}
	return newProdChannel(common)
}

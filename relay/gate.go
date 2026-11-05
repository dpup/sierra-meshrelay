package relay

import (
	"context"
	"net/http"
)

// Gate admits requests to the relay's routes. Closing it is how a deploy
// handoff stops the relay admitting new work before it drains and releases
// its database. sierra-data's internal/handoff Gate satisfies it.
type Gate interface {
	// HandlerFunc wraps a plain HTTP handler, refusing requests while closed.
	HandlerFunc(func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request)
	// Enter admits one long-lived request (a websocket), returning a context
	// that ends if the gate cancels in-flight work, and the function that
	// retires it. ok is false while closed.
	Enter(parent context.Context) (ctx context.Context, done func(), ok bool)
}

// openGate admits everything: the default when no handoff is in use.
type openGate struct{}

func (openGate) HandlerFunc(f func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return f
}

func (openGate) Enter(parent context.Context) (context.Context, func(), bool) {
	return parent, func() {}, true
}

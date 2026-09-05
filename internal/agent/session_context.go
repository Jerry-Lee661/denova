package agent

import (
	"context"

	"denova/internal/session"
)

type sessionContextKey struct{}

// WithSession stores the session in the context so that middleware (e.g. tool
// orchestrator) can persist externalize decisions without a direct reference.
func WithSession(ctx context.Context, sess *session.Session) context.Context {
	if sess == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionContextKey{}, sess)
}

// SessionFromContext retrieves the session from the context, or nil if absent.
func SessionFromContext(ctx context.Context) *session.Session {
	if ctx == nil {
		return nil
	}
	sess, _ := ctx.Value(sessionContextKey{}).(*session.Session)
	return sess
}

// SessionProvider is implemented by conversations that expose their underlying
// session, allowing the runtime to attach it to the run context.
type SessionProvider interface {
	Session() *session.Session
}

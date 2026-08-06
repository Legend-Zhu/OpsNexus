package authz

import (
	"context"
	"crypto/subtle"
	"strings"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/audit"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// GRPCUnaryInterceptor returns a gRPC server unary interceptor that enforces
// the same bearer-token auth as the HTTP middleware. When auth is disabled the
// interceptor is a pass-through.
func (m *Middleware) GRPCUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !m.enabled {
			return handler(ctx, req)
		}
		actor, ok := m.authorizeGRPC(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "invalid or missing bearer token")
		}
		return handler(audit.ContextWithActor(ctx, actor), req)
	}
}

// GRPCStreamInterceptor is the streaming counterpart (SubscribeEvents / logs).
func (m *Middleware) GRPCStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !m.enabled {
			return handler(srv, ss)
		}
		actor, ok := m.authorizeGRPC(ss.Context())
		if !ok {
			return status.Error(codes.Unauthenticated, "invalid or missing bearer token")
		}
		// Wrap the stream so the actor is visible to handlers reading the ctx.
		return handler(srv, &actorStream{ServerStream: ss, ctx: audit.ContextWithActor(ss.Context(), actor)})
	}
}

// authorizeGRPC extracts the bearer token from the "authorization" metadata
// header and constant-time-compares it against configured tokens. Returns the
// token name (audit actor) on success.
func (m *Middleware) authorizeGRPC(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return "", false
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	for name, secret := range m.tokens {
		if len(secret) == len(token) && subtle.ConstantTimeCompare([]byte(secret), []byte(token)) == 1 {
			return name, true
		}
	}
	return "", false
}

// actorStream wraps a grpc.ServerStream to override Context() with an
// augmented context carrying the audit actor.
type actorStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *actorStream) Context() context.Context { return s.ctx }

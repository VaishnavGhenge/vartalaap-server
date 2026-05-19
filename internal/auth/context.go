package auth

import "context"

type contextKey struct{}

type roomIDKey struct{}

func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, contextKey{}, userID)
}

func UserIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(contextKey{}).(string)
	return id, ok && id != ""
}

// WithRoomID attaches a room scope to the context. Used when a guest JWT
// carries a RoomID claim — the SFU handler enforces the scope.
func WithRoomID(ctx context.Context, roomID string) context.Context {
	return context.WithValue(ctx, roomIDKey{}, roomID)
}

// RoomIDFromContext returns the room ID from the context, or "" when not set.
func RoomIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(roomIDKey{}).(string); ok {
		return v
	}
	return ""
}

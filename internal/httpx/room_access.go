package httpx

import (
	"errors"

	"github.com/vaishnavghenge/vartalaap-server/internal/roomaccess"
)

func roomAccessCode(err error) string {
	switch {
	case errors.Is(err, roomaccess.ErrNotStarted):
		return "ROOM_NOT_STARTED"
	case errors.Is(err, roomaccess.ErrExpired):
		return "ROOM_EXPIRED"
	case errors.Is(err, roomaccess.ErrUnknownRoom):
		return "ROOM_NOT_FOUND"
	default:
		return "ROOM_UNAVAILABLE"
	}
}

func roomAccessMessage(err error) string {
	switch roomAccessCode(err) {
	case "ROOM_NOT_STARTED":
		return "This meeting has not started yet."
	case "ROOM_EXPIRED":
		return "This meeting is no longer active."
	case "ROOM_NOT_FOUND":
		return "This meeting is not active."
	default:
		return "This meeting is not available right now."
	}
}

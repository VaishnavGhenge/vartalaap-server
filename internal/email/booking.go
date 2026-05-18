package email

import (
	"bytes"
	"fmt"
	"html"
	"net/mail"
	"strings"
	"time"
)

// BookingInput is the slice of booking data the renderer needs. Kept narrow
// so the email package doesn't import store/* (and so test fixtures stay
// tiny). The caller in httpx fills this from store.Booking + lookups.
type BookingInput struct {
	GuestName    string
	GuestEmail   string
	HostName     string
	HostEmail    string
	HostTimezone string
	EventTitle   string
	EventMinutes int
	StartsAt     time.Time
	EndsAt       time.Time
	MeetCode     string
	// CancelToken is the magic-link credential included on the booking-page
	// URL the guest receives. Optional in case the caller doesn't have one
	// (e.g. host notification where we don't expose the guest's cancel link).
	CancelToken string
	// PublicAppURL is the absolute base URL of the booking site (e.g.
	// https://getsessionly.com). The room link is built as
	// `<PublicAppURL>/room/<MeetCode>` and the confirmation link as
	// `<PublicAppURL>/m/<MeetCode>?t=<CancelToken>`.
	PublicAppURL string
}

// RenderBookingConfirmation produces the message a guest receives right
// after booking. Body is intentionally short — most guests scan it for the
// time + meet link and close it.
func RenderBookingConfirmation(in BookingInput, from string) Message {
	roomURL := joinURL(in.PublicAppURL, "/room/"+in.MeetCode)
	confirmURL := joinURL(in.PublicAppURL, "/m/"+in.MeetCode)
	if in.CancelToken != "" {
		// The guest's link carries the cancel token so the same page can
		// drive both "manage" and "cancel" without a second credential.
		confirmURL += "?t=" + in.CancelToken
	}
	when := formatWhen(in.StartsAt, in.EndsAt, in.HostTimezone)

	text := strings.Join([]string{
		fmt.Sprintf("You're booked with %s.", in.HostName),
		"",
		fmt.Sprintf("Event: %s (%d min)", in.EventTitle, in.EventMinutes),
		fmt.Sprintf("When:  %s", when),
		fmt.Sprintf("Where: %s", roomURL),
		"",
		fmt.Sprintf("Manage or cancel: %s", confirmURL),
		fmt.Sprintf("Meet code: %s", in.MeetCode),
		"",
		"The room opens at the booked time. Bookmark this email — the meet link",
		"is the same one you'll open to join.",
	}, "\n")

	htmlBody := renderHTML(map[string]string{
		"GuestName":  html.EscapeString(in.GuestName),
		"HostName":   html.EscapeString(in.HostName),
		"EventTitle": html.EscapeString(in.EventTitle),
		"Duration":   fmt.Sprintf("%d min", in.EventMinutes),
		"When":       html.EscapeString(when),
		"RoomURL":    html.EscapeString(roomURL),
		"ConfirmURL": html.EscapeString(confirmURL),
		"MeetCode":   html.EscapeString(in.MeetCode),
	}, guestHTML)

	return Message{
		To:       []string{addressLine(in.GuestName, in.GuestEmail)},
		From:     from,
		Subject:  fmt.Sprintf("Booked: %s with %s", in.EventTitle, in.HostName),
		TextBody: text,
		HTMLBody: htmlBody,
		Attachments: []Attachment{{
			Filename:    "booking.ics",
			ContentType: "text/calendar; method=PUBLISH; charset=UTF-8",
			Body:        BuildICS(in, roomURL),
		}},
	}
}

// RenderBookingNotification is the host-side counterpart — same shape, copy
// tuned for the side of the table that's getting work scheduled, not booking
// it. Same .ics attachment so the host can add it to their calendar with one
// click.
func RenderBookingNotification(in BookingInput, from string) Message {
	roomURL := joinURL(in.PublicAppURL, "/room/"+in.MeetCode)
	when := formatWhen(in.StartsAt, in.EndsAt, in.HostTimezone)
	text := strings.Join([]string{
		fmt.Sprintf("New booking from %s.", in.GuestName),
		"",
		fmt.Sprintf("Guest: %s <%s>", in.GuestName, in.GuestEmail),
		fmt.Sprintf("Event: %s (%d min)", in.EventTitle, in.EventMinutes),
		fmt.Sprintf("When:  %s", when),
		fmt.Sprintf("Room:  %s", roomURL),
	}, "\n")
	htmlBody := renderHTML(map[string]string{
		"GuestName":  html.EscapeString(in.GuestName),
		"GuestEmail": html.EscapeString(in.GuestEmail),
		"EventTitle": html.EscapeString(in.EventTitle),
		"Duration":   fmt.Sprintf("%d min", in.EventMinutes),
		"When":       html.EscapeString(when),
		"RoomURL":    html.EscapeString(roomURL),
	}, hostHTML)
	return Message{
		To:       []string{addressLine(in.HostName, in.HostEmail)},
		From:     from,
		Subject:  fmt.Sprintf("New booking: %s with %s", in.EventTitle, in.GuestName),
		TextBody: text,
		HTMLBody: htmlBody,
		Attachments: []Attachment{{
			Filename:    "booking.ics",
			ContentType: "text/calendar; method=PUBLISH; charset=UTF-8",
			Body:        BuildICS(in, roomURL),
		}},
	}
}

// RenderBookingCancellation is sent to both parties when a booking is
// cancelled. `cancelledBy` is "host" or "guest" so the message frames it from
// the right side ("Pat cancelled" vs "you cancelled"). One message is built
// per recipient so the To header is right; the body is shared.
func RenderBookingCancellation(in BookingInput, from, cancelledBy string) Message {
	when := formatWhen(in.StartsAt, in.EndsAt, in.HostTimezone)
	byLabel := cancelledBy
	if byLabel == "" {
		byLabel = "someone"
	}
	text := strings.Join([]string{
		fmt.Sprintf("This booking was cancelled by the %s.", byLabel),
		"",
		fmt.Sprintf("Event: %s (%d min)", in.EventTitle, in.EventMinutes),
		fmt.Sprintf("When:  %s", when),
		fmt.Sprintf("With:  %s and %s", in.HostName, in.GuestName),
	}, "\n")
	htmlBody := renderHTML(map[string]string{
		"CancelledBy": html.EscapeString(byLabel),
		"EventTitle":  html.EscapeString(in.EventTitle),
		"Duration":    fmt.Sprintf("%d min", in.EventMinutes),
		"When":        html.EscapeString(when),
		"HostName":    html.EscapeString(in.HostName),
		"GuestName":   html.EscapeString(in.GuestName),
	}, cancelHTML)
	// Recipient = both. Most SMTP providers handle multi-recipient sends
	// fine; if your provider can't, split into two Send calls upstream.
	to := []string{
		addressLine(in.HostName, in.HostEmail),
		addressLine(in.GuestName, in.GuestEmail),
	}
	return Message{
		To:       to,
		From:     from,
		Subject:  fmt.Sprintf("Cancelled: %s", in.EventTitle),
		TextBody: text,
		HTMLBody: htmlBody,
	}
}

const cancelHTML = `<!doctype html>
<html><body style="font-family:system-ui,sans-serif;line-height:1.5;color:#1a1a1a">
<table cellpadding="0" cellspacing="0" style="max-width:520px;margin:24px auto;padding:24px;border:1px solid #eee;border-radius:12px">
<tr><td>
<h2 style="margin:0 0 8px;font-weight:600">Booking cancelled</h2>
<p style="margin:0 0 16px;color:#666">{{.EventTitle}} · {{.Duration}}</p>
<p style="margin:0 0 4px"><strong>When:</strong> {{.When}}</p>
<p style="margin:0 0 4px"><strong>With:</strong> {{.HostName}} and {{.GuestName}}</p>
<p style="margin:16px 0 0;color:#888;font-size:13px">Cancelled by the {{.CancelledBy}}.</p>
</td></tr>
</table>
</body></html>`

// BuildICS produces a minimal RFC 5545 VEVENT. We keep it static — no
// VTIMEZONE block — by emitting DTSTART/DTEND in UTC. Calendar apps render
// the local time from the absolute timestamp, so "9am NY" still shows as
// "9am NY" for any user in any timezone.
func BuildICS(in BookingInput, location string) []byte {
	var buf bytes.Buffer
	w := func(line string) { buf.WriteString(line); buf.WriteString("\r\n") }
	w("BEGIN:VCALENDAR")
	w("VERSION:2.0")
	w("PRODID:-//Sessionly//Booking//EN")
	w("CALSCALE:GREGORIAN")
	w("METHOD:PUBLISH")
	w("BEGIN:VEVENT")
	w("UID:" + in.MeetCode + "@sessionly")
	w("DTSTAMP:" + time.Now().UTC().Format("20060102T150405Z"))
	w("DTSTART:" + in.StartsAt.UTC().Format("20060102T150405Z"))
	w("DTEND:" + in.EndsAt.UTC().Format("20060102T150405Z"))
	w("SUMMARY:" + icsEscape(in.EventTitle))
	w("DESCRIPTION:" + icsEscape(fmt.Sprintf(
		"Booked with %s. Join: %s", in.HostName, location,
	)))
	w("LOCATION:" + icsEscape(location))
	w("ORGANIZER;CN=" + icsEscape(in.HostName) + ":mailto:" + in.HostEmail)
	w("ATTENDEE;CN=" + icsEscape(in.GuestName) + ":mailto:" + in.GuestEmail)
	w("END:VEVENT")
	w("END:VCALENDAR")
	return buf.Bytes()
}

func icsEscape(s string) string {
	// Per RFC 5545 §3.3.11: escape backslash, comma, semicolon; replace
	// newlines with literal \n.
	r := strings.NewReplacer("\\", "\\\\", ";", "\\;", ",", "\\,", "\n", "\\n", "\r", "")
	return r.Replace(s)
}

// formatWhen returns "Sat, May 18 · 9:00 AM – 9:30 AM UTC". When the host's
// timezone is known we render in their tz so the host's local reading of the
// booking is unambiguous; the guest can mentally translate using their own
// device clock.
func formatWhen(start, end time.Time, hostTZ string) string {
	loc := time.UTC
	if hostTZ != "" {
		if l, err := time.LoadLocation(hostTZ); err == nil {
			loc = l
		}
	}
	s := start.In(loc)
	e := end.In(loc)
	return fmt.Sprintf("%s · %s – %s %s",
		s.Format("Mon, Jan 2"),
		s.Format("3:04 PM"),
		e.Format("3:04 PM"),
		s.Format("MST"),
	)
}

func joinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func addressLine(name, email string) string {
	if name == "" {
		return sanitizeHeaderValue(email)
	}
	return (&mail.Address{
		Name:    sanitizeHeaderValue(name),
		Address: sanitizeHeaderValue(email),
	}).String()
}

func renderHTML(vars map[string]string, template string) string {
	out := template
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{."+k+"}}", v)
	}
	return out
}

const guestHTML = `<!doctype html>
<html><body style="font-family:system-ui,sans-serif;line-height:1.5;color:#1a1a1a">
<table cellpadding="0" cellspacing="0" style="max-width:520px;margin:24px auto;padding:24px;border:1px solid #eee;border-radius:12px">
<tr><td>
<h2 style="margin:0 0 8px;font-weight:600">You're booked with {{.HostName}}</h2>
<p style="margin:0 0 16px;color:#666">{{.EventTitle}} · {{.Duration}}</p>
<p style="margin:0 0 4px"><strong>When:</strong> {{.When}}</p>
<p style="margin:0 0 16px"><strong>Where:</strong> <a href="{{.RoomURL}}">{{.RoomURL}}</a></p>
<p style="margin:0 0 4px;color:#666">Meet code: <code>{{.MeetCode}}</code></p>
<p style="margin:16px 0 0;color:#888;font-size:13px">The room opens at the booked time. Need to <a href="{{.ConfirmURL}}">manage or cancel</a> this booking? Use that link — it's tied to this email.</p>
</td></tr>
</table>
</body></html>`

const hostHTML = `<!doctype html>
<html><body style="font-family:system-ui,sans-serif;line-height:1.5;color:#1a1a1a">
<table cellpadding="0" cellspacing="0" style="max-width:520px;margin:24px auto;padding:24px;border:1px solid #eee;border-radius:12px">
<tr><td>
<h2 style="margin:0 0 8px;font-weight:600">New booking</h2>
<p style="margin:0 0 16px;color:#666">{{.EventTitle}} · {{.Duration}}</p>
<p style="margin:0 0 4px"><strong>Guest:</strong> {{.GuestName}} &lt;{{.GuestEmail}}&gt;</p>
<p style="margin:0 0 4px"><strong>When:</strong> {{.When}}</p>
<p style="margin:0 0 16px"><strong>Room:</strong> <a href="{{.RoomURL}}">{{.RoomURL}}</a></p>
</td></tr>
</table>
</body></html>`

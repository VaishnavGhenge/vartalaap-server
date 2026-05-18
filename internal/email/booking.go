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
	GuestName          string
	GuestEmail         string
	HostName           string
	HostEmail          string
	HostTimezone       string
	EventTitle         string
	EventMinutes       int
	StartsAt           time.Time
	EndsAt             time.Time
	MeetCode           string
	CancellationReason string
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
		fmt.Sprintf("Reason: %s", cancellationReason(in)),
	}, "\n")
	htmlBody := renderHTML(map[string]string{
		"CancelledBy": html.EscapeString(byLabel),
		"EventTitle":  html.EscapeString(in.EventTitle),
		"Duration":    fmt.Sprintf("%d min", in.EventMinutes),
		"When":        html.EscapeString(when),
		"HostName":    html.EscapeString(in.HostName),
		"GuestName":   html.EscapeString(in.GuestName),
		"Reason":      html.EscapeString(cancellationReason(in)),
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

var cancelHTML = emailWrap(`
<tr>
  <td style="padding:20px 32px 8px">
    <p style="margin:0 0 6px;font-size:13px;font-weight:600;letter-spacing:0.06em;text-transform:uppercase;color:#ef4444">Cancelled</p>
    <h2 style="margin:0 0 6px;font-size:22px;font-weight:700;color:#111827;line-height:1.3">{{.EventTitle}}</h2>
    <p style="margin:0;font-size:14px;color:#6b7280">{{.Duration}} &middot; cancelled by the {{.CancelledBy}}</p>
  </td>
</tr>
<tr>
  <td style="padding:16px 32px 24px">
    <table cellpadding="0" cellspacing="0" width="100%" style="background:#f9fafb;border-radius:10px;border:1px solid #e5e7eb">
      <tr>
        <td style="padding:16px 20px">
          <table cellpadding="0" cellspacing="0" width="100%">
            <tr>
              <td style="padding:4px 0"><span style="font-size:12px;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:0.05em">WHEN</span><br><span style="font-size:14px;color:#111827">{{.When}}</span></td>
            </tr>
            <tr>
              <td style="padding:10px 0 4px;border-top:1px solid #e5e7eb"><span style="font-size:12px;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:0.05em">PARTICIPANTS</span><br><span style="font-size:14px;color:#111827">{{.HostName}} &amp; {{.GuestName}}</span></td>
            </tr>
            <tr>
              <td style="padding:10px 0 4px;border-top:1px solid #e5e7eb"><span style="font-size:12px;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:0.05em">REASON</span><br><span style="font-size:14px;color:#111827">{{.Reason}}</span></td>
            </tr>
          </table>
        </td>
      </tr>
    </table>
  </td>
</tr>
`)

func cancellationReason(in BookingInput) string {
	if in.CancellationReason == "" {
		return "No reason provided"
	}
	return in.CancellationReason
}

const logoHeader = `
  <!-- Sessionly logo header -->
  <tr>
    <td style="padding:24px 32px 20px;border-bottom:1px solid #f3f4f6">
      <table cellpadding="0" cellspacing="0"><tr>
        <td width="36" height="36" align="center" valign="middle" style="background:#4f46e5;border-radius:9px;line-height:0">
          <svg width="23" height="23" viewBox="0 0 32 32" fill="none" xmlns="http://www.w3.org/2000/svg">
            <path d="M9.25 11.25h13.5" stroke="white" stroke-linecap="round" stroke-width="2.25" opacity="0.7"/>
            <path d="M12 8.5v5M20 8.5v5" stroke="white" stroke-linecap="round" stroke-width="2.25" opacity="0.78"/>
            <path d="M8.5 17.2c0-1.2.97-2.17 2.17-2.17h7.35c1.2 0 2.17.97 2.17 2.17v.3l3.88-2.12c.63-.34 1.4.11 1.4.83v7.08c0 .72-.77 1.17-1.4.83L20.19 22v.3c0 1.2-.97 2.17-2.17 2.17h-7.35A2.17 2.17 0 0 1 8.5 22.3v-5.1Z" fill="white"/>
          </svg>
        </td>
        <td style="padding-left:10px;vertical-align:middle">
          <span style="font-size:18px;font-weight:700;color:#4f46e5;letter-spacing:-0.02em;line-height:1">Session<span style="opacity:0.4">ly</span></span>
        </td>
      </tr></table>
    </td>
  </tr>`

const logoFooter = `
  <!-- Footer -->
  <tr>
    <td style="padding:14px 32px;border-top:1px solid #f3f4f6;background-color:#fafafa;text-align:center">
      <a href="https://getsessionly.com" style="font-size:12px;color:#9ca3af;text-decoration:none">getsessionly.com</a>
    </td>
  </tr>`

func emailWrap(body string) string {
	return `<!doctype html>
<html lang="en">
<head><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="Content-Type" content="text/html; charset=UTF-8"></head>
<body style="margin:0;padding:0;background-color:#f3f4f6;font-family:system-ui,-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;-webkit-font-smoothing:antialiased">
<table cellpadding="0" cellspacing="0" width="100%" style="background-color:#f3f4f6;padding:32px 16px">
<tr><td align="center">
<table cellpadding="0" cellspacing="0" width="520" style="max-width:520px;background:#ffffff;border-radius:16px;border:1px solid #e5e7eb;overflow:hidden">
` + logoHeader + body + logoFooter + `
</table>
</td></tr>
</table>
</body></html>`
}

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

var guestHTML = emailWrap(`
<tr>
  <td style="padding:20px 32px 8px">
    <p style="margin:0 0 6px;font-size:13px;font-weight:600;letter-spacing:0.06em;text-transform:uppercase;color:#4f46e5">Confirmed</p>
    <h2 style="margin:0 0 6px;font-size:22px;font-weight:700;color:#111827;line-height:1.3">You're booked with {{.HostName}}</h2>
    <p style="margin:0;font-size:14px;color:#6b7280">{{.EventTitle}} &middot; {{.Duration}}</p>
  </td>
</tr>
<tr>
  <td style="padding:16px 32px 20px">
    <table cellpadding="0" cellspacing="0" width="100%" style="background:#f9fafb;border-radius:10px;border:1px solid #e5e7eb">
      <tr><td style="padding:16px 20px">
        <table cellpadding="0" cellspacing="0" width="100%">
          <tr><td style="padding:4px 0 10px">
            <span style="font-size:11px;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:0.06em">WHEN</span><br>
            <span style="font-size:14px;color:#111827">{{.When}}</span>
          </td></tr>
          <tr><td style="padding:10px 0 4px;border-top:1px solid #e5e7eb">
            <span style="font-size:11px;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:0.06em">MEET CODE</span><br>
            <span style="font-size:14px;font-family:monospace;color:#111827;letter-spacing:0.08em">{{.MeetCode}}</span>
          </td></tr>
        </table>
      </td></tr>
    </table>
  </td>
</tr>
<tr>
  <td style="padding:0 32px 24px;text-align:center">
    <table cellpadding="0" cellspacing="0" style="margin:0 auto">
      <tr><td style="background:#4f46e5;border-radius:8px;padding:13px 28px">
        <a href="{{.RoomURL}}" style="color:#ffffff;font-size:15px;font-weight:600;text-decoration:none;letter-spacing:-0.01em">Join meeting &#8594;</a>
      </td></tr>
    </table>
    <p style="margin:14px 0 0;font-size:12px;color:#9ca3af">
      The room opens at the booked time. Need to <a href="{{.ConfirmURL}}" style="color:#6b7280;text-decoration:underline">manage or cancel</a>?
    </p>
  </td>
</tr>
`)

var hostHTML = emailWrap(`
<tr>
  <td style="padding:20px 32px 8px">
    <p style="margin:0 0 6px;font-size:13px;font-weight:600;letter-spacing:0.06em;text-transform:uppercase;color:#4f46e5">New booking</p>
    <h2 style="margin:0 0 6px;font-size:22px;font-weight:700;color:#111827;line-height:1.3">{{.EventTitle}}</h2>
    <p style="margin:0;font-size:14px;color:#6b7280">{{.Duration}} with {{.GuestName}}</p>
  </td>
</tr>
<tr>
  <td style="padding:16px 32px 20px">
    <table cellpadding="0" cellspacing="0" width="100%" style="background:#f9fafb;border-radius:10px;border:1px solid #e5e7eb">
      <tr><td style="padding:16px 20px">
        <table cellpadding="0" cellspacing="0" width="100%">
          <tr><td style="padding:4px 0 10px">
            <span style="font-size:11px;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:0.06em">WHEN</span><br>
            <span style="font-size:14px;color:#111827">{{.When}}</span>
          </td></tr>
          <tr><td style="padding:10px 0 4px;border-top:1px solid #e5e7eb">
            <span style="font-size:11px;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:0.06em">GUEST</span><br>
            <span style="font-size:14px;color:#111827">{{.GuestName}} &lt;<a href="mailto:{{.GuestEmail}}" style="color:#4f46e5;text-decoration:none">{{.GuestEmail}}</a>&gt;</span>
          </td></tr>
        </table>
      </td></tr>
    </table>
  </td>
</tr>
<tr>
  <td style="padding:0 32px 24px;text-align:center">
    <table cellpadding="0" cellspacing="0" style="margin:0 auto">
      <tr><td style="background:#4f46e5;border-radius:8px;padding:13px 28px">
        <a href="{{.RoomURL}}" style="color:#ffffff;font-size:15px;font-weight:600;text-decoration:none;letter-spacing:-0.01em">Open room &#8594;</a>
      </td></tr>
    </table>
  </td>
</tr>
`)

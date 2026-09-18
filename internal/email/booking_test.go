package email

import (
	"context"
	"strings"
	"testing"
	"time"
)

func sampleInput() BookingInput {
	start := time.Date(2026, 5, 18, 13, 0, 0, 0, time.UTC) // 9am NY in May (EDT)
	return BookingInput{
		GuestName:    "Pat Guest",
		GuestEmail:   "pat@example.com",
		HostName:     "Alex Host",
		HostEmail:    "alex@example.com",
		HostTimezone: "America/New_York",
		EventTitle:   "Intro call",
		EventMinutes: 30,
		StartsAt:     start,
		EndsAt:       start.Add(30 * time.Minute),
		MeetCode:     "i55-iemv-qzx",
		PublicAppURL: "https://getsessionly.com",
	}
}

func TestRenderBookingConfirmation_ContainsKeyData(t *testing.T) {
	msg := RenderBookingConfirmation(sampleInput(), "Sessionly <no-reply@sessionly.test>")
	if msg.Subject != "Booked: Intro call with Alex Host" {
		t.Fatalf("subject: %q", msg.Subject)
	}
	if got := msg.To[0]; got != `"Pat Guest" <pat@example.com>` {
		t.Fatalf("To header: %q", got)
	}
	// The room URL must appear in both the text and HTML body so older mail
	// clients that prefer text don't lose the link.
	wantURL := "https://getsessionly.com/room/i55-iemv-qzx"
	for _, body := range []string{msg.TextBody, msg.HTMLBody} {
		if !strings.Contains(body, wantURL) {
			t.Fatalf("body missing room URL %q\nbody=%s", wantURL, body)
		}
	}
	// "9:00 AM EDT" — host's NY timezone, May = EDT.
	if !strings.Contains(msg.TextBody, "9:00 AM") || !strings.Contains(msg.TextBody, "EDT") {
		t.Fatalf("expected '9:00 AM EDT' in text body, got %q", msg.TextBody)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Filename != "booking.ics" {
		t.Fatalf("expected one booking.ics attachment, got %+v", msg.Attachments)
	}
}

func TestRenderBookingNotification_GoesToHost(t *testing.T) {
	msg := RenderBookingNotification(sampleInput(), "Sessionly <no-reply@sessionly.test>")
	if msg.To[0] != `"Alex Host" <alex@example.com>` {
		t.Fatalf("notification should go to host, got %v", msg.To)
	}
	if !strings.Contains(msg.TextBody, "Pat Guest") {
		t.Fatalf("notification should mention guest, got %q", msg.TextBody)
	}
}

func TestRenderBookingRescheduled_IsClearAndKeepsManageLink(t *testing.T) {
	in := sampleInput()
	in.CancelToken = "manage-token"
	guest := RenderBookingRescheduled(in, "Sessionly <no-reply@sessionly.test>", false)
	if guest.Subject != "Rescheduled: Intro call with Alex Host" || !strings.Contains(guest.TextBody, "has been rescheduled") {
		t.Fatalf("guest update is unclear: subject=%q body=%q", guest.Subject, guest.TextBody)
	}
	if !strings.Contains(guest.TextBody, "https://getsessionly.com/m/i55-iemv-qzx?t=manage-token") {
		t.Fatalf("guest update lost manage link: %q", guest.TextBody)
	}
	host := RenderBookingRescheduled(in, "Sessionly <no-reply@sessionly.test>", true)
	if host.Subject != "Rescheduled: Intro call with Pat Guest" || host.To[0] != `"Alex Host" <alex@example.com>` {
		t.Fatalf("host update is wrong: %+v", host)
	}
}

func TestRenderBookingReminder_HasRightRecipientAndLinks(t *testing.T) {
	in := sampleInput()
	in.CancelToken = "manage-token"
	guest := RenderBookingReminder(in, "Sessionly <no-reply@sessionly.test>", false, time.Hour)
	if guest.Subject != "Reminder: Intro call in 1 hour" || guest.To[0] != `"Pat Guest" <pat@example.com>` {
		t.Fatalf("guest reminder headers are wrong: %+v", guest)
	}
	for _, want := range []string{
		"starts in 1 hour",
		"https://getsessionly.com/room/i55-iemv-qzx?gt=manage-token",
		"https://getsessionly.com/m/i55-iemv-qzx?t=manage-token",
	} {
		if !strings.Contains(guest.TextBody, want) {
			t.Fatalf("guest reminder missing %q: %q", want, guest.TextBody)
		}
	}
	host := RenderBookingReminder(in, "Sessionly <no-reply@sessionly.test>", true, 24*time.Hour)
	if host.Subject != "Reminder: Intro call in 24 hours" || host.To[0] != `"Alex Host" <alex@example.com>` {
		t.Fatalf("host reminder headers are wrong: %+v", host)
	}
	if strings.Contains(host.TextBody, "manage-token") || !strings.Contains(host.TextBody, "With:  Pat Guest") {
		t.Fatalf("host reminder leaked guest authority or lost guest context: %q", host.TextBody)
	}
}

func TestBuildICS_HasRequiredHeaders(t *testing.T) {
	body := string(BuildICS(sampleInput(), "https://getsessionly.com/room/i55-iemv-qzx"))
	required := []string{
		"BEGIN:VCALENDAR",
		"END:VCALENDAR",
		"BEGIN:VEVENT",
		"END:VEVENT",
		"UID:i55-iemv-qzx@sessionly",
		"SEQUENCE:0",
		"DTSTART:20260518T130000Z",
		"DTEND:20260518T133000Z",
		"SUMMARY:Intro call",
		"ORGANIZER;CN=Alex Host:mailto:alex@example.com",
		"ATTENDEE;CN=Pat Guest:mailto:pat@example.com",
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Fatalf("ICS missing %q. Full body:\n%s", want, body)
		}
	}
}

func TestBuildICS_UsesBookingSequence(t *testing.T) {
	in := sampleInput()
	in.Sequence = 2
	body := string(BuildICS(in, "https://getsessionly.com/room/i55-iemv-qzx"))
	if !strings.Contains(body, "\r\nSEQUENCE:2\r\n") {
		t.Fatalf("ICS missing reschedule sequence. Full body:\n%s", body)
	}
}

func TestICSEscape_HandlesCommasSemicolonsBackslash(t *testing.T) {
	in := BookingInput{
		GuestName: "Pat", GuestEmail: "pat@example.com",
		HostName: "Alex", HostEmail: "alex@example.com",
		EventTitle:   "Quick chat, with notes; please",
		EventMinutes: 30,
		StartsAt:     time.Date(2026, 5, 18, 9, 0, 0, 0, time.UTC),
		EndsAt:       time.Date(2026, 5, 18, 9, 30, 0, 0, time.UTC),
		MeetCode:     "test",
		PublicAppURL: "https://app",
	}
	body := string(BuildICS(in, "https://app/room/test"))
	if !strings.Contains(body, `SUMMARY:Quick chat\, with notes\; please`) {
		t.Fatalf("expected escaped summary; got body=\n%s", body)
	}
}

func TestEncodeMessage_RoundTripsMixedMultipart(t *testing.T) {
	msg := RenderBookingConfirmation(sampleInput(), "Sessionly <no-reply@sessionly.test>")
	body, err := encodeMessage(msg, msg.From)
	if err != nil {
		t.Fatalf("encodeMessage: %v", err)
	}
	want := []string{
		"From: Sessionly <no-reply@sessionly.test>",
		`To: "Pat Guest" <pat@example.com>`,
		"Content-Type: multipart/mixed",
		"Content-Type: multipart/alternative",
		"Content-Type: text/plain",
		"Content-Type: text/html",
		"Content-Type: text/calendar",
	}
	for _, w := range want {
		if !strings.Contains(string(body), w) {
			t.Fatalf("missing %q in encoded body. body=\n%s", w, string(body))
		}
	}
}

func TestEncodeMessage_StripsHeaderInjection(t *testing.T) {
	in := sampleInput()
	in.GuestName = "Pat\r\nBcc: attacker@example.com"
	in.EventTitle = "Intro\r\nX-Injected: yes"

	msg := RenderBookingConfirmation(in, "Sessionly <no-reply@sessionly.test>")
	body, err := encodeMessage(msg, msg.From)
	if err != nil {
		t.Fatalf("encodeMessage: %v", err)
	}
	headers := strings.SplitN(string(body), "\r\n\r\n", 2)[0]
	if strings.Contains(headers, "\r\nBcc:") || strings.Contains(headers, "\r\nX-Injected:") {
		t.Fatalf("header injection was not stripped:\n%s", headers)
	}
	if !strings.Contains(headers, "Pat  Bcc: attacker@example.com") {
		t.Fatalf("expected injected guest name to be flattened, got:\n%s", headers)
	}
}

func TestLogMailer_NeverFails(t *testing.T) {
	var m LogMailer
	if err := m.Send(context.Background(), Message{To: []string{"x@y"}, Subject: "test"}); err != nil {
		t.Fatalf("LogMailer.Send: %v", err)
	}
}

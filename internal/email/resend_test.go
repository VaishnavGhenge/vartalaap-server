package email

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type resendTransport func(*http.Request) (*http.Response, error)

func (f resendTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResendPreservesIdempotencyAndAttachments(t *testing.T) {
	m := &ResendMailer{apiKey: "test-key", from: "host@example.com", client: &http.Client{Transport: resendTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Idempotency-Key") != "booking/test/created/guest_email" {
			t.Error("missing idempotency header")
		}
		var payload struct {
			Attachments []struct {
				Filename    string `json:"filename"`
				Content     string `json:"content"`
				ContentType string `json:"content_type"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Attachments) != 1 {
			t.Fatal("calendar attachment dropped")
		}
		a := payload.Attachments[0]
		body, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil || string(body) != "BEGIN:VCALENDAR" || a.Filename != "booking.ics" || a.ContentType != "text/calendar" {
			t.Fatalf("invalid attachment: %+v %v", a, err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"test"}`)), Header: make(http.Header)}, nil
	})}}
	err := m.Send(context.Background(), Message{IdempotencyKey: "booking/test/created/guest_email", To: []string{"guest@example.com"}, Subject: "Booked", Attachments: []Attachment{{Filename: "booking.ics", ContentType: "text/calendar", Body: []byte("BEGIN:VCALENDAR")}}})
	if err != nil {
		t.Fatal(err)
	}
}

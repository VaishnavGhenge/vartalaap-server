package email

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime/quotedprintable"
	"strings"
	"time"
)

// encodeMessage produces a RFC 5322 multipart/mixed message with a
// multipart/alternative inner part (text + html) and one MIME part per
// attachment. Chunking base64 at 76 chars per line keeps the output valid
// for every SMTP server we've tested against.
func encodeMessage(msg Message, from string) ([]byte, error) {
	mixedBoundary := "mixed_" + randomToken()
	altBoundary := "alt_" + randomToken()

	var buf bytes.Buffer
	headers := []struct{ k, v string }{
		{"From", from},
		{"To", strings.Join(msg.To, ", ")},
		{"Subject", encodeHeader(msg.Subject)},
		{"Date", time.Now().UTC().Format(time.RFC1123Z)},
		{"MIME-Version", "1.0"},
		{"Content-Type", `multipart/mixed; boundary="` + mixedBoundary + `"`},
	}
	for _, h := range headers {
		fmt.Fprintf(&buf, "%s: %s\r\n", h.k, h.v)
	}
	buf.WriteString("\r\n")

	// Alternative part: text + html.
	fmt.Fprintf(&buf, "--%s\r\n", mixedBoundary)
	fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", altBoundary)

	if msg.TextBody != "" {
		writeAltPart(&buf, altBoundary, "text/plain; charset=UTF-8", msg.TextBody)
	}
	if msg.HTMLBody != "" {
		writeAltPart(&buf, altBoundary, "text/html; charset=UTF-8", msg.HTMLBody)
	}
	fmt.Fprintf(&buf, "--%s--\r\n", altBoundary)

	for _, att := range msg.Attachments {
		fmt.Fprintf(&buf, "--%s\r\n", mixedBoundary)
		fmt.Fprintf(&buf, "Content-Type: %s; name=%q\r\n", att.ContentType, att.Filename)
		fmt.Fprintf(&buf, "Content-Disposition: attachment; filename=%q\r\n", att.Filename)
		buf.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		enc := base64.StdEncoding.EncodeToString(att.Body)
		// Wrap at 76 chars per RFC 2045.
		for i := 0; i < len(enc); i += 76 {
			end := i + 76
			if end > len(enc) {
				end = len(enc)
			}
			buf.WriteString(enc[i:end])
			buf.WriteString("\r\n")
		}
	}
	fmt.Fprintf(&buf, "--%s--\r\n", mixedBoundary)
	return buf.Bytes(), nil
}

func writeAltPart(buf *bytes.Buffer, boundary, contentType, body string) {
	fmt.Fprintf(buf, "--%s\r\n", boundary)
	fmt.Fprintf(buf, "Content-Type: %s\r\n", contentType)
	buf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	w := quotedprintable.NewWriter(buf)
	_, _ = w.Write([]byte(body))
	_ = w.Close()
	buf.WriteString("\r\n")
}

func encodeHeader(s string) string {
	// If all ASCII keep verbatim; otherwise wrap as encoded-word.
	for _, r := range s {
		if r > 0x7f {
			return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}

// randomToken returns a short, unique-enough boundary suffix. Time-based
// nanosecond avoids pulling in crypto/rand for something that just needs to
// not collide with the body content.
func randomToken() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

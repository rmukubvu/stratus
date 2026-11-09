// Package ses emulates Amazon Simple Email Service. Nothing is delivered:
// every email a client sends is captured in an inbox that tests and agents
// read back through the control API, so "did the app send the right email?"
// becomes an assertion instead of a guess.
package ses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
)

// Identity types, as SES names them.
const (
	IdentityEmailAddress = "EmailAddress"
	IdentityDomain       = "Domain"
)

// Identity is a verified sender: an email address or a whole domain.
// Verification is immediate; there is no confirmation email to click.
type Identity struct {
	Name              string    `json:"name"`
	Type              string    `json:"type"`
	VerifiedAt        time.Time `json:"verified_at"`
	VerificationToken string    `json:"verification_token,omitempty"`
}

// IdentityTypeFor says whether a name is an address or a domain.
func IdentityTypeFor(name string) string {
	if strings.Contains(name, "@") {
		return IdentityEmailAddress
	}
	return IdentityDomain
}

// Template is a reusable email with {{placeholders}}.
type Template struct {
	Name      string    `json:"name"`
	Subject   string    `json:"subject"`
	Text      string    `json:"text,omitempty"`
	HTML      string    `json:"html,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Destination lists who an email goes to.
type Destination struct {
	To  []string `json:"to,omitempty"`
	Cc  []string `json:"cc,omitempty"`
	Bcc []string `json:"bcc,omitempty"`
}

// Recipients returns every address, in To, Cc, Bcc order.
func (d Destination) Recipients() []string {
	out := make([]string, 0, len(d.To)+len(d.Cc)+len(d.Bcc))
	out = append(out, d.To...)
	out = append(out, d.Cc...)
	return append(out, d.Bcc...)
}

// Body is an email's content in the formats the sender provided.
type Body struct {
	Text string `json:"text,omitempty"`
	HTML string `json:"html,omitempty"`
}

// TemplateRef records which template rendered a message and with what data.
type TemplateRef struct {
	Name string `json:"name"`
	Data string `json:"data,omitempty"`
}

// Message is one captured email.
type Message struct {
	ID               string            `json:"id"`
	Timestamp        time.Time         `json:"timestamp"`
	Source           string            `json:"source"`
	Destination      Destination       `json:"destination"`
	ReplyTo          []string          `json:"reply_to,omitempty"`
	Subject          string            `json:"subject"`
	Body             Body              `json:"body"`
	Template         *TemplateRef      `json:"template,omitempty"`
	Raw              string            `json:"raw,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
	ConfigurationSet string            `json:"configuration_set,omitempty"`
	// API says which API family sent the message: "ses" (v1) or "sesv2".
	API string `json:"api"`
}

// SendInput is a request to send an email, in any of SES's three shapes:
// simple content, a template with data, or a raw MIME message.
type SendInput struct {
	Source           string
	Destination      Destination
	ReplyTo          []string
	Simple           *Body
	Subject          string
	Template         string
	TemplateData     string
	Raw              []byte
	Tags             map[string]string
	ConfigurationSet string
	API              string
}

// MessageFilter narrows an inbox query. Zero values match everything.
type MessageFilter struct {
	// To matches a recipient in To, Cc or Bcc, case-insensitively.
	To string
	// From matches the sender address, case-insensitively.
	From string
	// Subject matches a case-insensitive substring of the subject.
	Subject string
	// Since keeps messages sent at or after this time.
	Since time.Time
	// Limit caps the result, newest last. Zero means no cap.
	Limit int
}

// Matches reports whether m satisfies the filter.
func (f MessageFilter) Matches(m Message) bool {
	if f.To != "" && !containsAddress(m.Destination.Recipients(), f.To) {
		return false
	}
	if f.From != "" && !strings.EqualFold(addressOf(m.Source), addressOf(f.From)) {
		return false
	}
	if f.Subject != "" && !strings.Contains(strings.ToLower(m.Subject), strings.ToLower(f.Subject)) {
		return false
	}
	if !f.Since.IsZero() && m.Timestamp.Before(f.Since) {
		return false
	}
	return true
}

func containsAddress(list []string, want string) bool {
	want = addressOf(want)
	for _, item := range list {
		if strings.EqualFold(addressOf(item), want) {
			return true
		}
	}
	return false
}

// addressOf extracts the bare address from "Name <addr>" forms.
func addressOf(raw string) string {
	if parsed, err := mail.ParseAddress(strings.TrimSpace(raw)); err == nil {
		return parsed.Address
	}
	return strings.TrimSpace(raw)
}

// domainOf returns the part after the @ of an address, lower-cased.
func domainOf(address string) string {
	if idx := strings.LastIndex(address, "@"); idx >= 0 {
		return strings.ToLower(address[idx+1:])
	}
	return ""
}

var placeholderPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.]+)\s*\}\}`)

// Render fills a template's {{placeholders}} from JSON data. Nested values
// are reached with dots ({{user.name}}); unknown placeholders render empty,
// the way a missing variable renders in SES.
func (t Template) Render(data string) (subject string, body Body, err error) {
	values := map[string]any{}
	if strings.TrimSpace(data) != "" {
		if err := json.Unmarshal([]byte(data), &values); err != nil {
			return "", Body{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidTemplateData", Message: "TemplateData is not valid JSON"}
		}
	}
	fill := func(text string) string {
		return placeholderPattern.ReplaceAllStringFunc(text, func(match string) string {
			key := placeholderPattern.FindStringSubmatch(match)[1]
			return lookup(values, strings.Split(key, "."))
		})
	}
	return fill(t.Subject), Body{Text: fill(t.Text), HTML: fill(t.HTML)}, nil
}

func lookup(values map[string]any, path []string) string {
	var current any = values
	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = object[segment]
		if !ok {
			return ""
		}
	}
	switch v := current.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// parseRaw extracts what the inbox indexes from a raw MIME message. Bodies
// are kept as the sender wrote them; multipart messages keep only the raw
// form, which is still searchable.
func parseRaw(raw []byte) (from string, dest Destination, subject string, body Body) {
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		return "", Destination{}, "", Body{}
	}
	from = strings.TrimSpace(msg.Header.Get("From"))
	subject = strings.TrimSpace(msg.Header.Get("Subject"))
	dest = Destination{To: headerAddresses(msg.Header, "To"), Cc: headerAddresses(msg.Header, "Cc"), Bcc: headerAddresses(msg.Header, "Bcc")}

	content := strings.ToLower(msg.Header.Get("Content-Type"))
	if strings.HasPrefix(content, "multipart/") {
		return from, dest, subject, Body{}
	}
	var text strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := msg.Body.Read(buf)
		text.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if strings.HasPrefix(content, "text/html") {
		return from, dest, subject, Body{HTML: strings.TrimSpace(text.String())}
	}
	return from, dest, subject, Body{Text: strings.TrimSpace(text.String())}
}

func headerAddresses(header mail.Header, key string) []string {
	raw := header.Get(key)
	if raw == "" {
		return nil
	}
	list, err := header.AddressList(key)
	if err != nil {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	out := make([]string, 0, len(list))
	for _, addr := range list {
		out = append(out, addr.String())
	}
	return out
}

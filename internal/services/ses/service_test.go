package ses

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/store/bbolt"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newTestService(t *testing.T) (*Service, *clock) {
	t.Helper()
	metadata, err := bbolt.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { metadata.Close() })
	c := &clock{now: t0}
	return NewService(Options{Metadata: metadata, Now: c.Now}), c
}

func requireCode(t *testing.T, err error, code string) *apierror.Error {
	t.Helper()
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
	return apiErr
}

func simple(from string, to ...string) SendInput {
	return SendInput{
		Source:      from,
		Destination: Destination{To: to},
		Subject:     "Your order",
		Simple:      &Body{Text: "Thanks!", HTML: "<p>Thanks!</p>"},
		API:         "ses",
	}
}

func TestSendRequiresVerifiedSenderAddressOrDomain(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.Send(simple("noreply@shop.example", "a@b.example"))
	apiErr := requireCode(t, err, "MessageRejected")
	if !strings.Contains(apiErr.Message, "noreply@shop.example") || !strings.Contains(apiErr.Message, "verify-email-identity") {
		t.Fatalf("rejection should name the address and how to verify it: %q", apiErr.Message)
	}

	if _, err := svc.VerifyIdentity("shop.example"); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	if _, err := svc.Send(simple("Shop <NoReply@Shop.example>", "a@b.example")); err != nil {
		t.Fatalf("a verified domain should cover any address in it, case-insensitively: %v", err)
	}
	if _, err := svc.Send(simple("other@elsewhere.example", "a@b.example")); err == nil {
		t.Fatal("an unrelated domain must still be rejected")
	}
	if _, err := svc.VerifyIdentity("other@elsewhere.example"); err != nil {
		t.Fatalf("verify address: %v", err)
	}
	if _, err := svc.Send(simple("other@elsewhere.example", "a@b.example")); err != nil {
		t.Fatalf("a verified address should send: %v", err)
	}
}

// sendErr is Send for callers that only care about the error.
func sendErr(svc *Service, in SendInput) error {
	_, err := svc.Send(in)
	return err
}

func TestSendCapturesSimpleEmailWithAllItsParts(t *testing.T) {
	svc, _ := newTestService(t)
	_, _ = svc.VerifyIdentity("noreply@shop.example")

	in := simple("noreply@shop.example", "a@b.example")
	in.Destination.Cc = []string{"cc@b.example"}
	in.Destination.Bcc = []string{"bcc@b.example"}
	in.ReplyTo = []string{"support@shop.example"}
	in.Tags = map[string]string{"campaign": "welcome"}
	in.ConfigurationSet = "default"
	msg, err := svc.Send(in)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if msg.ID == "" || !msg.Timestamp.Equal(t0) || msg.Subject != "Your order" || msg.Body.Text != "Thanks!" || msg.Body.HTML != "<p>Thanks!</p>" {
		t.Fatalf("unexpected message: %+v", msg)
	}
	if len(msg.Destination.Recipients()) != 3 || msg.ReplyTo[0] != "support@shop.example" || msg.Tags["campaign"] != "welcome" || msg.ConfigurationSet != "default" || msg.API != "ses" {
		t.Fatalf("message lost fields: %+v", msg)
	}

	stored, err := svc.Message(msg.ID)
	if err != nil || stored.ID != msg.ID {
		t.Fatalf("message should be readable by id: %v %+v", err, stored)
	}
}

func TestSendValidatesSimpleContent(t *testing.T) {
	svc, _ := newTestService(t)
	_, _ = svc.VerifyIdentity("noreply@shop.example")

	noSubject := simple("noreply@shop.example", "a@b.example")
	noSubject.Subject = ""
	requireCode(t, sendErr(svc, noSubject), "InvalidParameterValue")

	noBody := simple("noreply@shop.example", "a@b.example")
	noBody.Simple = &Body{}
	requireCode(t, sendErr(svc, noBody), "InvalidParameterValue")

	noRecipients := simple("noreply@shop.example")
	requireCode(t, sendErr(svc, noRecipients), "InvalidParameterValue")

	noContent := SendInput{Source: "noreply@shop.example", Destination: Destination{To: []string{"a@b.example"}}}
	requireCode(t, sendErr(svc, noContent), "InvalidParameterValue")
}

func TestSendRendersTemplatesWithNestedData(t *testing.T) {
	svc, _ := newTestService(t)
	_, _ = svc.VerifyIdentity("noreply@shop.example")
	if _, err := svc.CreateTemplate(Template{Name: "welcome", Subject: "Hi {{ user.name }}", Text: "Order {{order}} for {{user.name}}", HTML: "<b>{{order}}</b> {{missing}}"}); err != nil {
		t.Fatalf("create template: %v", err)
	}

	msg, err := svc.Send(SendInput{
		Source:       "noreply@shop.example",
		Destination:  Destination{To: []string{"a@b.example"}},
		Template:     "welcome",
		TemplateData: `{"user":{"name":"Ada"},"order":1042}`,
		API:          "sesv2",
	})
	if err != nil {
		t.Fatalf("send templated: %v", err)
	}
	if msg.Subject != "Hi Ada" || msg.Body.Text != "Order 1042 for Ada" || msg.Body.HTML != "<b>1042</b> " {
		t.Fatalf("template not rendered as expected: %+v", msg)
	}
	if msg.Template == nil || msg.Template.Name != "welcome" || !strings.Contains(msg.Template.Data, "Ada") {
		t.Fatalf("message should record the template and data: %+v", msg.Template)
	}

	requireCode(t, sendErr(svc, SendInput{Source: "noreply@shop.example", Destination: Destination{To: []string{"a@b.example"}}, Template: "nope"}), "TemplateDoesNotExist")
	requireCode(t, sendErr(svc, SendInput{Source: "noreply@shop.example", Destination: Destination{To: []string{"a@b.example"}}, Template: "welcome", TemplateData: "{bad"}), "InvalidTemplateData")
}

func TestSendRawParsesHeadersAndKeepsTheRawMessage(t *testing.T) {
	svc, _ := newTestService(t)
	_, _ = svc.VerifyIdentity("shop.example")

	raw := "From: Shop <noreply@shop.example>\r\nTo: Ada <ada@b.example>, bob@b.example\r\nCc: cc@b.example\r\nSubject: Raw hello\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello from raw\r\n"
	msg, err := svc.Send(SendInput{Raw: []byte(raw), API: "ses"})
	if err != nil {
		t.Fatalf("send raw: %v", err)
	}
	if msg.Source != "Shop <noreply@shop.example>" || msg.Subject != "Raw hello" || msg.Body.Text != "Hello from raw" {
		t.Fatalf("raw headers not parsed: %+v", msg)
	}
	if len(msg.Destination.To) != 2 || len(msg.Destination.Cc) != 1 || msg.Raw != raw {
		t.Fatalf("raw recipients or body lost: %+v", msg)
	}

	// Explicit Source and Destinations win over the headers, as in SES.
	msg, err = svc.Send(SendInput{Raw: []byte(raw), Source: "orders@shop.example", Destination: Destination{To: []string{"x@b.example"}}, API: "ses"})
	if err != nil {
		t.Fatalf("send raw with overrides: %v", err)
	}
	if msg.Source != "orders@shop.example" || len(msg.Destination.Recipients()) != 1 {
		t.Fatalf("overrides ignored: %+v", msg)
	}
}

func TestIdentitiesAreIdempotentListableAndDeletable(t *testing.T) {
	svc, _ := newTestService(t)

	first, _ := svc.VerifyIdentity("shop.example")
	second, _ := svc.VerifyIdentity("shop.example")
	if first.VerificationToken == "" || first.VerificationToken != second.VerificationToken {
		t.Fatalf("verifying twice should return the same domain token: %q %q", first.VerificationToken, second.VerificationToken)
	}
	addr, _ := svc.VerifyIdentity("a@shop.example")
	if addr.Type != IdentityEmailAddress || addr.VerificationToken != "" {
		t.Fatalf("addresses have no token: %+v", addr)
	}

	all, _ := svc.Identities("")
	domains, _ := svc.Identities(IdentityDomain)
	if len(all) != 2 || len(domains) != 1 || domains[0].Name != "shop.example" {
		t.Fatalf("unexpected listing: all=%+v domains=%+v", all, domains)
	}
	if err := svc.DeleteIdentity("shop.example"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.DeleteIdentity("never-existed.example"); err != nil {
		t.Fatalf("deleting an unknown identity is not an error: %v", err)
	}
	if remaining, _ := svc.Identities(""); len(remaining) != 1 {
		t.Fatalf("domain should be gone: %+v", remaining)
	}
	requireCode(t, identityErr(svc.VerifyIdentity("")), "InvalidParameterValue")
}

func identityErr(_ Identity, err error) error { return err }

func TestTemplatesLifecycle(t *testing.T) {
	svc, _ := newTestService(t)

	requireCode(t, templateErr(svc.CreateTemplate(Template{Name: "", Subject: "s", Text: "t"})), "InvalidParameterValue")
	requireCode(t, templateErr(svc.CreateTemplate(Template{Name: "x", Subject: "", Text: "t"})), "InvalidParameterValue")
	requireCode(t, templateErr(svc.CreateTemplate(Template{Name: "x", Subject: "s"})), "InvalidParameterValue")

	created, err := svc.CreateTemplate(Template{Name: "welcome", Subject: "Hi", Text: "v1"})
	if err != nil || !created.CreatedAt.Equal(t0) {
		t.Fatalf("create: %v %+v", err, created)
	}
	requireCode(t, templateErr(svc.CreateTemplate(Template{Name: "welcome", Subject: "Hi", Text: "v2"})), "AlreadyExists")

	updated, err := svc.UpdateTemplate(Template{Name: "welcome", Subject: "Hi", Text: "v2"})
	if err != nil || updated.Text != "v2" || !updated.CreatedAt.Equal(t0) {
		t.Fatalf("update should keep the creation time: %v %+v", err, updated)
	}
	requireCode(t, templateErr(svc.UpdateTemplate(Template{Name: "nope", Subject: "Hi", Text: "v2"})), "TemplateDoesNotExist")

	if got, err := svc.Template("welcome"); err != nil || got.Text != "v2" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if list, _ := svc.Templates(); len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	if err := svc.DeleteTemplate("welcome"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	requireCode(t, templateErr(svc.Template("welcome")), "TemplateDoesNotExist")
}

func templateErr(_ Template, err error) error { return err }

func TestInboxFiltersAndOrdersMessages(t *testing.T) {
	svc, c := newTestService(t)
	_, _ = svc.VerifyIdentity("shop.example")

	send := func(from, to, subject string) Message {
		in := simple(from, to)
		in.Subject = subject
		msg, err := svc.Send(in)
		if err != nil {
			t.Fatalf("send %s: %v", subject, err)
		}
		c.now = c.now.Add(time.Minute)
		return msg
	}
	send("noreply@shop.example", "ada@b.example", "Welcome Ada")
	send("orders@shop.example", "Bob <bob@b.example>", "Order 1")
	send("orders@shop.example", "ada@b.example", "Order 2")

	all, _ := svc.Messages(MessageFilter{})
	if len(all) != 3 || all[0].Subject != "Welcome Ada" || all[2].Subject != "Order 2" {
		t.Fatalf("inbox should list oldest first: %+v", subjects(all))
	}
	byTo, _ := svc.Messages(MessageFilter{To: "ADA@b.example"})
	if len(byTo) != 2 {
		t.Fatalf("to filter: %v", subjects(byTo))
	}
	byDisplayName, _ := svc.Messages(MessageFilter{To: "bob@b.example"})
	if len(byDisplayName) != 1 {
		t.Fatalf("to filter should see through display names: %v", subjects(byDisplayName))
	}
	byFrom, _ := svc.Messages(MessageFilter{From: "orders@shop.example"})
	if len(byFrom) != 2 {
		t.Fatalf("from filter: %v", subjects(byFrom))
	}
	bySubject, _ := svc.Messages(MessageFilter{Subject: "order"})
	if len(bySubject) != 2 {
		t.Fatalf("subject filter should be a case-insensitive substring: %v", subjects(bySubject))
	}
	since, _ := svc.Messages(MessageFilter{Since: t0.Add(time.Minute)})
	if len(since) != 2 || since[0].Subject != "Order 1" {
		t.Fatalf("since filter: %v", subjects(since))
	}
	limited, _ := svc.Messages(MessageFilter{Limit: 1})
	if len(limited) != 1 || limited[0].Subject != "Order 2" {
		t.Fatalf("limit should keep the newest: %v", subjects(limited))
	}

	if count, _ := svc.SentCount(); count != 3 {
		t.Fatalf("sent count: %d", count)
	}
	cleared, err := svc.ClearMessages()
	if err != nil || cleared != 3 {
		t.Fatalf("clear: %v %d", err, cleared)
	}
	if rest, _ := svc.Messages(MessageFilter{}); len(rest) != 0 {
		t.Fatalf("inbox should be empty: %v", subjects(rest))
	}
	if ids, _ := svc.Identities(""); len(ids) != 1 {
		t.Fatal("clearing the inbox must keep identities")
	}
	requireCode(t, messageErr(svc.Message("missing")), "MessageNotFound")
}

func messageErr(_ Message, err error) error { return err }

func subjects(msgs []Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Subject)
	}
	return out
}

package ses

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func query(t *testing.T, svc *Service, operation string, params url.Values) (string, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(params.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	err := svc.HandleQuery(rec, req, operation, "req-1")
	return rec.Body.String(), err
}

func mustQuery(t *testing.T, svc *Service, operation string, params url.Values) string {
	t.Helper()
	body, err := query(t, svc, operation, params)
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	return body
}

func TestQuerySendEmailRoundTrip(t *testing.T) {
	svc, _ := newTestService(t)

	body := mustQuery(t, svc, "VerifyEmailIdentity", url.Values{"EmailAddress": {"noreply@shop.example"}})
	if !strings.Contains(body, "<VerifyEmailIdentityResponse") || !strings.Contains(body, "<RequestId>req-1</RequestId>") || strings.Contains(body, "Result") {
		t.Fatalf("empty results still get the envelope:\n%s", body)
	}

	body = mustQuery(t, svc, "SendEmail", url.Values{
		"Source":                            {"noreply@shop.example"},
		"Destination.ToAddresses.member.1":  {"ada@b.example"},
		"Destination.CcAddresses.member.1":  {"cc@b.example"},
		"Destination.BccAddresses.member.1": {"bcc@b.example"},
		"ReplyToAddresses.member.1":         {"support@shop.example"},
		"Message.Subject.Data":              {"Hello"},
		"Message.Body.Text.Data":            {"plain"},
		"Message.Body.Html.Data":            {"<b>html</b>"},
		"Tags.member.1.Name":                {"campaign"},
		"Tags.member.1.Value":               {"welcome"},
		"ConfigurationSetName":              {"cs"},
	})
	if !strings.Contains(body, "<SendEmailResponse") || !strings.Contains(body, "<SendEmailResult><MessageId>") {
		t.Fatalf("unexpected SendEmail response:\n%s", body)
	}
	msgs, _ := svc.Messages(MessageFilter{})
	if len(msgs) != 1 || msgs[0].Body.HTML != "<b>html</b>" || msgs[0].Tags["campaign"] != "welcome" || len(msgs[0].Destination.Recipients()) != 3 || msgs[0].ReplyTo[0] != "support@shop.example" || msgs[0].ConfigurationSet != "cs" {
		t.Fatalf("form fields not captured: %+v", msgs)
	}

	_, err := query(t, svc, "SendEmail", url.Values{"Source": {"stranger@x.example"}, "Destination.ToAddresses.member.1": {"a@b.example"}, "Message.Subject.Data": {"s"}, "Message.Body.Text.Data": {"t"}})
	requireCode(t, err, "MessageRejected")
}

func TestQueryRawAndTemplatedSends(t *testing.T) {
	svc, _ := newTestService(t)
	mustQuery(t, svc, "VerifyDomainIdentity", url.Values{"Domain": {"shop.example"}})

	raw := "From: noreply@shop.example\r\nTo: ada@b.example\r\nSubject: Raw\r\n\r\nbody\r\n"
	body := mustQuery(t, svc, "SendRawEmail", url.Values{"RawMessage.Data": {base64.StdEncoding.EncodeToString([]byte(raw))}})
	if !strings.Contains(body, "<SendRawEmailResult><MessageId>") {
		t.Fatalf("unexpected SendRawEmail response:\n%s", body)
	}
	_, err := query(t, svc, "SendRawEmail", url.Values{"RawMessage.Data": {"not base64!"}})
	requireCode(t, err, "InvalidParameterValue")

	mustQuery(t, svc, "CreateTemplate", url.Values{"Template.TemplateName": {"welcome"}, "Template.SubjectPart": {"Hi {{name}}"}, "Template.TextPart": {"Welcome {{name}}"}})
	body = mustQuery(t, svc, "SendTemplatedEmail", url.Values{
		"Source":                           {"noreply@shop.example"},
		"Destination.ToAddresses.member.1": {"ada@b.example"},
		"Template":                         {"welcome"},
		"TemplateData":                     {`{"name":"Ada"}`},
	})
	if !strings.Contains(body, "<SendTemplatedEmailResult><MessageId>") {
		t.Fatalf("unexpected SendTemplatedEmail response:\n%s", body)
	}
	msgs, _ := svc.Messages(MessageFilter{Subject: "Hi Ada"})
	if len(msgs) != 1 || msgs[0].Body.Text != "Welcome Ada" {
		t.Fatalf("templated email not rendered: %+v", msgs)
	}
	_, err = query(t, svc, "SendTemplatedEmail", url.Values{"Source": {"noreply@shop.example"}, "Destination.ToAddresses.member.1": {"a@b.example"}})
	requireCode(t, err, "InvalidParameterValue")
}

func TestQueryIdentityAndTemplateReads(t *testing.T) {
	svc, _ := newTestService(t)
	mustQuery(t, svc, "VerifyEmailIdentity", url.Values{"EmailAddress": {"a@shop.example"}})
	domain := mustQuery(t, svc, "VerifyDomainIdentity", url.Values{"Domain": {"shop.example"}})
	if !strings.Contains(domain, "<VerificationToken>") {
		t.Fatalf("domain verification should return a token:\n%s", domain)
	}

	list := mustQuery(t, svc, "ListIdentities", url.Values{})
	if !strings.Contains(list, "<Identities><member>a@shop.example</member><member>shop.example</member></Identities>") {
		t.Fatalf("unexpected ListIdentities:\n%s", list)
	}
	onlyDomains := mustQuery(t, svc, "ListIdentities", url.Values{"IdentityType": {"Domain"}})
	if strings.Contains(onlyDomains, "a@shop.example") {
		t.Fatalf("IdentityType filter ignored:\n%s", onlyDomains)
	}
	verified := mustQuery(t, svc, "ListVerifiedEmailAddresses", url.Values{})
	if !strings.Contains(verified, "<VerifiedEmailAddresses><member>a@shop.example</member></VerifiedEmailAddresses>") {
		t.Fatalf("unexpected ListVerifiedEmailAddresses:\n%s", verified)
	}
	attrs := mustQuery(t, svc, "GetIdentityVerificationAttributes", url.Values{"Identities.member.1": {"shop.example"}, "Identities.member.2": {"unknown.example"}})
	if !strings.Contains(attrs, "<entry><key>shop.example</key><value><VerificationStatus>Success</VerificationStatus><VerificationToken>") || strings.Contains(attrs, "unknown.example") {
		t.Fatalf("unexpected verification attributes:\n%s", attrs)
	}

	mustQuery(t, svc, "CreateTemplate", url.Values{"Template.TemplateName": {"welcome"}, "Template.SubjectPart": {"Hi"}, "Template.HtmlPart": {"<p>hi</p>"}})
	got := mustQuery(t, svc, "GetTemplate", url.Values{"TemplateName": {"welcome"}})
	if !strings.Contains(got, "<Template><TemplateName>welcome</TemplateName><SubjectPart>Hi</SubjectPart><HtmlPart>&lt;p&gt;hi&lt;/p&gt;</HtmlPart></Template>") {
		t.Fatalf("unexpected GetTemplate:\n%s", got)
	}
	listed := mustQuery(t, svc, "ListTemplates", url.Values{})
	if !strings.Contains(listed, "<TemplatesMetadata><member><Name>welcome</Name><CreatedTimestamp>2026-11-09T12:00:00Z</CreatedTimestamp></member></TemplatesMetadata>") {
		t.Fatalf("unexpected ListTemplates:\n%s", listed)
	}
	mustQuery(t, svc, "DeleteTemplate", url.Values{"TemplateName": {"welcome"}})
	_, err := query(t, svc, "GetTemplate", url.Values{"TemplateName": {"welcome"}})
	requireCode(t, err, "TemplateDoesNotExist")

	quota := mustQuery(t, svc, "GetSendQuota", url.Values{})
	if !strings.Contains(quota, "<Max24HourSend>200</Max24HourSend>") || !strings.Contains(quota, "<SentLast24Hours>0</SentLast24Hours>") {
		t.Fatalf("unexpected GetSendQuota:\n%s", quota)
	}
	_, err = query(t, svc, "SetIdentityNotificationTopic", url.Values{})
	requireCode(t, err, "NotImplemented")
}

func v2(t *testing.T, svc *Service, method, path string, payload any) (int, map[string]any, error) {
	t.Helper()
	var body string
	if payload != nil {
		raw, _ := json.Marshal(payload)
		body = string(raw)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	err := svc.HandleV2(rec, req)
	out := map[string]any{}
	if err == nil {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out, err
}

func TestOperationV2RoutesByMethodAndPath(t *testing.T) {
	cases := map[string]string{
		"POST /v2/email/outbound-emails":           "SendEmail",
		"POST /v2/email/identities":                "CreateEmailIdentity",
		"GET /v2/email/identities":                 "ListEmailIdentities",
		"GET /v2/email/identities/a%40b.example":   "GetEmailIdentity",
		"DELETE /v2/email/identities/shop.example": "DeleteEmailIdentity",
		"POST /v2/email/templates":                 "CreateEmailTemplate",
		"GET /v2/email/templates":                  "ListEmailTemplates",
		"GET /v2/email/templates/welcome":          "GetEmailTemplate",
		"PUT /v2/email/templates/welcome":          "UpdateEmailTemplate",
		"DELETE /v2/email/templates/welcome":       "DeleteEmailTemplate",
		"GET /v2/email/account":                    "GetAccount",
		"PATCH /v2/email/identities":               "",
		"GET /v2/email/identities/a/b":             "",
		"GET /schedules":                           "",
	}
	for request, want := range cases {
		method, path, _ := strings.Cut(request, " ")
		if got := OperationV2(method, path); got != want {
			t.Errorf("%s: got %q, want %q", request, got, want)
		}
	}
}

func TestV2SendEmailAndIdentities(t *testing.T) {
	svc, _ := newTestService(t)

	status, out, err := v2(t, svc, http.MethodPost, "/v2/email/identities", map[string]any{"EmailIdentity": "shop.example"})
	if err != nil || status != http.StatusOK || out["IdentityType"] != "DOMAIN" || out["VerifiedForSendingStatus"] != true {
		t.Fatalf("create identity: %d %v %v", status, out, err)
	}
	_, out, err = v2(t, svc, http.MethodGet, "/v2/email/identities/shop.example", nil)
	if err != nil || out["VerificationStatus"] != "SUCCESS" {
		t.Fatalf("get identity: %v %v", out, err)
	}
	_, _, err = v2(t, svc, http.MethodGet, "/v2/email/identities/unknown.example", nil)
	requireCode(t, err, "NotFoundException")

	_, out, err = v2(t, svc, http.MethodPost, "/v2/email/outbound-emails", map[string]any{
		"FromEmailAddress": "noreply@shop.example",
		"Destination":      map[string]any{"ToAddresses": []string{"ada@b.example"}},
		"Content": map[string]any{"Simple": map[string]any{
			"Subject": map[string]any{"Data": "Hello v2"},
			"Body":    map[string]any{"Text": map[string]any{"Data": "plain"}},
		}},
		"EmailTags": []map[string]any{{"Name": "k", "Value": "v"}},
	})
	if err != nil || out["MessageId"] == "" {
		t.Fatalf("send: %v %v", out, err)
	}
	msgs, _ := svc.Messages(MessageFilter{})
	if len(msgs) != 1 || msgs[0].API != "sesv2" || msgs[0].Subject != "Hello v2" || msgs[0].Tags["k"] != "v" {
		t.Fatalf("v2 send not captured: %+v", msgs)
	}

	_, _, err = v2(t, svc, http.MethodPost, "/v2/email/outbound-emails", map[string]any{
		"FromEmailAddress": "noreply@shop.example",
		"Destination":      map[string]any{"ToAddresses": []string{"ada@b.example"}},
		"Content":          map[string]any{"Simple": map[string]any{"Subject": map[string]any{"Data": ""}}},
	})
	requireCode(t, err, "BadRequestException")
	_, _, err = v2(t, svc, http.MethodPost, "/v2/email/outbound-emails", map[string]any{
		"FromEmailAddress": "someone@else.example",
		"Destination":      map[string]any{"ToAddresses": []string{"ada@b.example"}},
		"Content":          map[string]any{"Simple": map[string]any{"Subject": map[string]any{"Data": "s"}, "Body": map[string]any{"Text": map[string]any{"Data": "t"}}}},
	})
	requireCode(t, err, "MessageRejected")

	_, out, _ = v2(t, svc, http.MethodGet, "/v2/email/identities", nil)
	if items, _ := out["EmailIdentities"].([]any); len(items) != 1 {
		t.Fatalf("list identities: %v", out)
	}
	if _, _, err := v2(t, svc, http.MethodDelete, "/v2/email/identities/shop.example", nil); err != nil {
		t.Fatalf("delete identity: %v", err)
	}
	_, out, _ = v2(t, svc, http.MethodGet, "/v2/email/account", nil)
	if quota, _ := out["SendQuota"].(map[string]any); quota["SentLast24Hours"] != 1.0 {
		t.Fatalf("account should report sent count: %v", out)
	}
}

func TestV2TemplatesAndRawContent(t *testing.T) {
	svc, _ := newTestService(t)
	_, _, _ = v2(t, svc, http.MethodPost, "/v2/email/identities", map[string]any{"EmailIdentity": "noreply@shop.example"})

	_, _, err := v2(t, svc, http.MethodPost, "/v2/email/templates", map[string]any{"TemplateName": "welcome", "TemplateContent": map[string]any{"Subject": "Hi {{name}}", "Html": "<p>{{name}}</p>"}})
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	_, _, err = v2(t, svc, http.MethodPost, "/v2/email/templates", map[string]any{"TemplateName": "welcome", "TemplateContent": map[string]any{"Subject": "Hi", "Text": "x"}})
	requireCode(t, err, "AlreadyExistsException")
	_, _, err = v2(t, svc, http.MethodPut, "/v2/email/templates/welcome", map[string]any{"TemplateContent": map[string]any{"Subject": "Hey {{name}}", "Html": "<p>{{name}}</p>"}})
	if err != nil {
		t.Fatalf("update template: %v", err)
	}
	_, out, _ := v2(t, svc, http.MethodGet, "/v2/email/templates/welcome", nil)
	if content, _ := out["TemplateContent"].(map[string]any); content["Subject"] != "Hey {{name}}" {
		t.Fatalf("get template: %v", out)
	}
	_, _, err = v2(t, svc, http.MethodGet, "/v2/email/templates/nope", nil)
	requireCode(t, err, "NotFoundException")

	_, _, err = v2(t, svc, http.MethodPost, "/v2/email/outbound-emails", map[string]any{
		"FromEmailAddress": "noreply@shop.example",
		"Destination":      map[string]any{"ToAddresses": []string{"ada@b.example"}},
		"Content":          map[string]any{"Template": map[string]any{"TemplateName": "welcome", "TemplateData": `{"name":"Ada"}`}},
	})
	if err != nil {
		t.Fatalf("templated send: %v", err)
	}
	raw := "From: noreply@shop.example\r\nTo: ada@b.example\r\nSubject: Raw v2\r\nContent-Type: text/html\r\n\r\n<p>raw</p>\r\n"
	_, _, err = v2(t, svc, http.MethodPost, "/v2/email/outbound-emails", map[string]any{
		"Content": map[string]any{"Raw": map[string]any{"Data": base64.StdEncoding.EncodeToString([]byte(raw))}},
	})
	if err != nil {
		t.Fatalf("raw send: %v", err)
	}
	msgs, _ := svc.Messages(MessageFilter{})
	if len(msgs) != 2 || msgs[0].Subject != "Hey Ada" || msgs[1].Body.HTML != "<p>raw</p>" {
		t.Fatalf("v2 sends not captured as expected: %+v", msgs)
	}
	_, out, _ = v2(t, svc, http.MethodGet, "/v2/email/templates", nil)
	if items, _ := out["TemplatesMetadata"].([]any); len(items) != 1 {
		t.Fatalf("list templates: %v", out)
	}
	if _, _, err := v2(t, svc, http.MethodDelete, "/v2/email/templates/welcome", nil); err != nil {
		t.Fatalf("delete template: %v", err)
	}
	_, _, err = v2(t, svc, http.MethodPatch, "/v2/email/templates/welcome", nil)
	requireCode(t, err, "NotFoundException")
}

func TestInboxRoutes(t *testing.T) {
	svc, c := newTestService(t)
	_, _ = svc.VerifyIdentity("shop.example")
	mux := http.NewServeMux()
	RegisterRoutes(muxRouter{mux}, svc)

	first, _ := svc.Send(simple("a@shop.example", "ada@b.example"))
	c.now = c.now.Add(time.Hour)
	in := simple("b@shop.example", "bob@b.example")
	in.Subject = "Second"
	second, _ := svc.Send(in)

	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	status, out := get("/_stratus/control/v1/ses/messages")
	if status != http.StatusOK || out["count"] != 2.0 {
		t.Fatalf("list: %d %v", status, out)
	}
	_, out = get("/_stratus/control/v1/ses/messages?to=bob@b.example&subject=sec")
	if out["count"] != 1.0 {
		t.Fatalf("filters: %v", out)
	}
	_, out = get("/_stratus/control/v1/ses/messages?since=" + url.QueryEscape(t0.Add(30*time.Minute).Format(time.RFC3339)))
	if items, _ := out["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != second.ID {
		t.Fatalf("since filter: %v", out)
	}
	status, out = get("/_stratus/control/v1/ses/messages?since=yesterday")
	if status != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("bad since should be a control API error: %d %v", status, out)
	}
	status, _ = get("/_stratus/control/v1/ses/messages?limit=-1")
	if status != http.StatusBadRequest {
		t.Fatalf("bad limit should be rejected: %d", status)
	}
	status, out = get("/_stratus/control/v1/ses/messages/" + first.ID)
	if status != http.StatusOK || out["id"] != first.ID {
		t.Fatalf("get one: %d %v", status, out)
	}
	status, out = get("/_stratus/control/v1/ses/messages/nope")
	if status != http.StatusNotFound || out["error"].(map[string]any)["code"] != "MessageNotFound" {
		t.Fatalf("missing message: %d %v", status, out)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/_stratus/control/v1/ses/messages", nil))
	cleared := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &cleared)
	if rec.Code != http.StatusOK || cleared["cleared"] != 2.0 {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	_, out = get("/_stratus/control/v1/ses/messages")
	if out["count"] != 0.0 {
		t.Fatalf("inbox should be empty: %v", out)
	}
}

// muxRouter adapts the standard mux to the control API's Router shape.
type muxRouter struct{ *http.ServeMux }

func (m muxRouter) HandleFunc(pattern string, fn http.HandlerFunc) {
	m.ServeMux.HandleFunc(pattern, fn)
}

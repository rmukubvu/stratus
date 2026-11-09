package ses

import (
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
)

const queryNamespace = "http://ses.amazonaws.com/doc/2010-12-01/"

// QueryOperations are the SES v1 (query protocol) actions this service
// answers. The HTTP layer uses them to route unsigned requests.
var QueryOperations = []string{
	"SendEmail", "SendRawEmail", "SendTemplatedEmail",
	"VerifyEmailIdentity", "VerifyEmailAddress", "VerifyDomainIdentity", "DeleteIdentity",
	"ListIdentities", "GetIdentityVerificationAttributes", "ListVerifiedEmailAddresses",
	"CreateTemplate", "ListTemplates", "DeleteTemplate", "UpdateTemplate",
	"GetSendQuota",
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

// HandleQuery serves an SES v1 request.
func (s *Service) HandleQuery(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	form, err := awscompat.ParseQueryForm(r)
	if err != nil {
		return invalidParameter("request body is not valid form data")
	}
	switch operation {
	case "SendEmail":
		return s.querySendEmail(w, form, requestID)
	case "SendRawEmail":
		return s.querySendRawEmail(w, form, requestID)
	case "SendTemplatedEmail":
		return s.querySendTemplatedEmail(w, form, requestID)
	case "VerifyEmailIdentity", "VerifyEmailAddress":
		_, err := s.VerifyIdentity(form.Get("EmailAddress"))
		return respondEmpty(w, operation, requestID, err)
	case "VerifyDomainIdentity":
		identity, err := s.VerifyIdentity(form.Get("Domain"))
		if err != nil {
			return err
		}
		return respond(w, operation, requestID, struct {
			VerificationToken string `xml:"VerificationToken"`
		}{identity.VerificationToken})
	case "DeleteIdentity":
		return respondEmpty(w, operation, requestID, s.DeleteIdentity(form.Get("Identity")))
	case "ListIdentities":
		identities, err := s.Identities(form.Get("IdentityType"))
		if err != nil {
			return err
		}
		names := make([]string, 0, len(identities))
		for _, identity := range identities {
			names = append(names, identity.Name)
		}
		return respond(w, operation, requestID, struct {
			Identities []string `xml:"Identities>member"`
		}{names})
	case "ListVerifiedEmailAddresses":
		identities, err := s.Identities(IdentityEmailAddress)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(identities))
		for _, identity := range identities {
			names = append(names, identity.Name)
		}
		return respond(w, operation, requestID, struct {
			Addresses []string `xml:"VerifiedEmailAddresses>member"`
		}{names})
	case "GetIdentityVerificationAttributes":
		return s.queryVerificationAttributes(w, form, requestID)
	case "CreateTemplate":
		_, err := s.CreateTemplate(templateFromForm(form))
		return respondEmpty(w, operation, requestID, err)
	case "UpdateTemplate":
		_, err := s.UpdateTemplate(templateFromForm(form))
		return respondEmpty(w, operation, requestID, err)
	case "GetTemplate":
		t, err := s.Template(form.Get("TemplateName"))
		if err != nil {
			return err
		}
		return respond(w, operation, requestID, struct {
			Template templateXML `xml:"Template"`
		}{templateXML{TemplateName: t.Name, SubjectPart: t.Subject, TextPart: t.Text, HtmlPart: t.HTML}})
	case "ListTemplates":
		templates, err := s.Templates()
		if err != nil {
			return err
		}
		items := make([]templateMetadataXML, 0, len(templates))
		for _, t := range templates {
			items = append(items, templateMetadataXML{Name: t.Name, CreatedTimestamp: t.CreatedAt})
		}
		return respond(w, operation, requestID, struct {
			Items []templateMetadataXML `xml:"TemplatesMetadata>member"`
		}{items})
	case "DeleteTemplate":
		return respondEmpty(w, operation, requestID, s.DeleteTemplate(form.Get("TemplateName")))
	case "GetSendQuota":
		sent, err := s.SentCount()
		if err != nil {
			return err
		}
		return respond(w, operation, requestID, struct {
			Max24HourSend   float64 `xml:"Max24HourSend"`
			MaxSendRate     float64 `xml:"MaxSendRate"`
			SentLast24Hours float64 `xml:"SentLast24Hours"`
		}{max24HourSend, maxSendRate, float64(sent)})
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "ses operation is not implemented: " + operation}
	}
}

func (s *Service) querySendEmail(w http.ResponseWriter, form url.Values, requestID string) error {
	in := sendInputFromForm(form)
	in.Subject = form.Get("Message.Subject.Data")
	in.Simple = &Body{Text: form.Get("Message.Body.Text.Data"), HTML: form.Get("Message.Body.Html.Data")}
	msg, err := s.Send(in)
	if err != nil {
		return err
	}
	return respondMessageID(w, "SendEmail", requestID, msg.ID)
}

func (s *Service) querySendTemplatedEmail(w http.ResponseWriter, form url.Values, requestID string) error {
	in := sendInputFromForm(form)
	in.Template = form.Get("Template")
	in.TemplateData = form.Get("TemplateData")
	if in.Template == "" {
		return invalidParameter("Template is required")
	}
	msg, err := s.Send(in)
	if err != nil {
		return err
	}
	return respondMessageID(w, "SendTemplatedEmail", requestID, msg.ID)
}

func (s *Service) querySendRawEmail(w http.ResponseWriter, form url.Values, requestID string) error {
	raw, err := base64.StdEncoding.DecodeString(form.Get("RawMessage.Data"))
	if err != nil || len(raw) == 0 {
		return invalidParameter("RawMessage.Data must be a base64-encoded MIME message")
	}
	in := sendInputFromForm(form)
	in.Destination = Destination{To: members(form, "Destinations.member.")}
	in.Raw = raw
	msg, err := s.Send(in)
	if err != nil {
		return err
	}
	return respondMessageID(w, "SendRawEmail", requestID, msg.ID)
}

func (s *Service) queryVerificationAttributes(w http.ResponseWriter, form url.Values, requestID string) error {
	type attributes struct {
		VerificationStatus string `xml:"VerificationStatus"`
		VerificationToken  string `xml:"VerificationToken,omitempty"`
	}
	type entry struct {
		Key   string     `xml:"key"`
		Value attributes `xml:"value"`
	}
	entries := make([]entry, 0)
	for _, name := range members(form, "Identities.member.") {
		identity, ok, err := s.Identity(name)
		if err != nil {
			return err
		}
		if ok {
			entries = append(entries, entry{Key: identity.Name, Value: attributes{VerificationStatus: "Success", VerificationToken: identity.VerificationToken}})
		}
	}
	return respond(w, "GetIdentityVerificationAttributes", requestID, struct {
		Entries []entry `xml:"VerificationAttributes>entry"`
	}{entries})
}

func sendInputFromForm(form url.Values) SendInput {
	return SendInput{
		Source: form.Get("Source"),
		Destination: Destination{
			To:  members(form, "Destination.ToAddresses.member."),
			Cc:  members(form, "Destination.CcAddresses.member."),
			Bcc: members(form, "Destination.BccAddresses.member."),
		},
		ReplyTo:          members(form, "ReplyToAddresses.member."),
		Tags:             tagsFromForm(form, "Tags.member."),
		ConfigurationSet: form.Get("ConfigurationSetName"),
		API:              "ses",
	}
}

func templateFromForm(form url.Values) Template {
	return Template{
		Name:    form.Get("Template.TemplateName"),
		Subject: form.Get("Template.SubjectPart"),
		Text:    form.Get("Template.TextPart"),
		HTML:    form.Get("Template.HtmlPart"),
	}
}

func members(form url.Values, prefix string) []string {
	var items []string
	for idx := 1; ; idx++ {
		value := form.Get(prefix + strconv.Itoa(idx))
		if value == "" {
			break
		}
		items = append(items, value)
	}
	return items
}

func tagsFromForm(form url.Values, prefix string) map[string]string {
	tags := map[string]string{}
	for idx := 1; ; idx++ {
		name := form.Get(prefix + strconv.Itoa(idx) + ".Name")
		if name == "" {
			break
		}
		tags[name] = form.Get(prefix + strconv.Itoa(idx) + ".Value")
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

type templateXML struct {
	TemplateName string `xml:"TemplateName"`
	SubjectPart  string `xml:"SubjectPart"`
	TextPart     string `xml:"TextPart,omitempty"`
	HtmlPart     string `xml:"HtmlPart,omitempty"`
}

type templateMetadataXML struct {
	Name             string    `xml:"Name"`
	CreatedTimestamp time.Time `xml:"CreatedTimestamp"`
}

func respondMessageID(w http.ResponseWriter, operation, requestID, id string) error {
	return respond(w, operation, requestID, struct {
		MessageId string `xml:"MessageId"`
	}{id})
}

func respondEmpty(w http.ResponseWriter, operation, requestID string, err error) error {
	if err != nil {
		return err
	}
	return respond(w, operation, requestID, nil)
}

// respond writes the query-protocol envelope: <OpResponse><OpResult>…
// </OpResult><ResponseMetadata/></OpResponse>.
func respond(w http.ResponseWriter, operation, requestID string, result any) error {
	type envelope struct {
		XMLName          xml.Name
		XMLNS            string           `xml:"xmlns,attr"`
		Result           any              `xml:",omitempty"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}
	payload := envelope{
		XMLName:          xml.Name{Local: operation + "Response"},
		XMLNS:            queryNamespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	}
	if result != nil {
		payload.Result = &resultElement{Name: operation + "Result", Value: result}
	}
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	return xml.NewEncoder(w).Encode(payload)
}

// resultElement wraps an operation's result in its <OpResult> element.
type resultElement struct {
	Name  string
	Value any
}

func (r *resultElement) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	return e.EncodeElement(r.Value, xml.StartElement{Name: xml.Name{Local: r.Name}})
}

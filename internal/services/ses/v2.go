package ses

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
)

// V2PathPrefix is where the SES v2 REST API lives.
const V2PathPrefix = "/v2/email/"

// OperationV2 names the SES v2 operation for a request, or "" when the path
// is not one this service serves.
func OperationV2(method, path string) string {
	resource, id, ok := splitV2Path(path)
	if !ok {
		return ""
	}
	switch {
	case resource == "outbound-emails" && id == "" && method == http.MethodPost:
		return "SendEmail"
	case resource == "identities" && id == "" && method == http.MethodPost:
		return "CreateEmailIdentity"
	case resource == "identities" && id == "" && method == http.MethodGet:
		return "ListEmailIdentities"
	case resource == "identities" && id != "" && method == http.MethodGet:
		return "GetEmailIdentity"
	case resource == "identities" && id != "" && method == http.MethodDelete:
		return "DeleteEmailIdentity"
	case resource == "templates" && id == "" && method == http.MethodPost:
		return "CreateEmailTemplate"
	case resource == "templates" && id == "" && method == http.MethodGet:
		return "ListEmailTemplates"
	case resource == "templates" && id != "" && method == http.MethodGet:
		return "GetEmailTemplate"
	case resource == "templates" && id != "" && method == http.MethodPut:
		return "UpdateEmailTemplate"
	case resource == "templates" && id != "" && method == http.MethodDelete:
		return "DeleteEmailTemplate"
	case resource == "account" && id == "" && method == http.MethodGet:
		return "GetAccount"
	}
	return ""
}

func splitV2Path(path string) (resource, id string, ok bool) {
	if !strings.HasPrefix(path, V2PathPrefix) {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, V2PathPrefix), "/"), "/")
	switch len(parts) {
	case 1:
		return parts[0], "", parts[0] != ""
	case 2:
		id, err := url.PathUnescape(parts[1])
		return parts[0], id, err == nil && id != ""
	default:
		return "", "", false
	}
}

type v2SendInput struct {
	FromEmailAddress     string   `json:"FromEmailAddress"`
	ReplyToAddresses     []string `json:"ReplyToAddresses"`
	ConfigurationSetName string   `json:"ConfigurationSetName"`
	Destination          struct {
		ToAddresses  []string `json:"ToAddresses"`
		CcAddresses  []string `json:"CcAddresses"`
		BccAddresses []string `json:"BccAddresses"`
	} `json:"Destination"`
	Content struct {
		Simple *struct {
			Subject struct {
				Data string `json:"Data"`
			} `json:"Subject"`
			Body struct {
				Text *struct {
					Data string `json:"Data"`
				} `json:"Text"`
				Html *struct {
					Data string `json:"Data"`
				} `json:"Html"`
			} `json:"Body"`
		} `json:"Simple"`
		Raw *struct {
			Data string `json:"Data"`
		} `json:"Raw"`
		Template *struct {
			TemplateName string `json:"TemplateName"`
			TemplateArn  string `json:"TemplateArn"`
			TemplateData string `json:"TemplateData"`
		} `json:"Template"`
	} `json:"Content"`
	EmailTags []struct {
		Name  string `json:"Name"`
		Value string `json:"Value"`
	} `json:"EmailTags"`
}

type v2TemplateContent struct {
	Subject string `json:"Subject"`
	Text    string `json:"Text,omitempty"`
	Html    string `json:"Html,omitempty"`
}

type v2TemplateInput struct {
	TemplateName    string            `json:"TemplateName"`
	TemplateContent v2TemplateContent `json:"TemplateContent"`
}

// HandleV2 serves an SES v2 REST request. Errors keep the v2 vocabulary
// (BadRequestException, NotFoundException, AlreadyExistsException).
func (s *Service) HandleV2(w http.ResponseWriter, r *http.Request) error {
	err := s.handleV2(w, r)
	if err == nil {
		return nil
	}
	var apiErr *apierror.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "InvalidParameterValue", "InvalidTemplateData":
			return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "BadRequestException", Message: apiErr.Message}
		case "TemplateDoesNotExist":
			return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: apiErr.Message}
		case "AlreadyExists":
			return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "AlreadyExistsException", Message: apiErr.Message}
		}
	}
	return err
}

func (s *Service) handleV2(w http.ResponseWriter, r *http.Request) error {
	_, id, _ := splitV2Path(r.URL.Path)
	switch OperationV2(r.Method, r.URL.Path) {
	case "SendEmail":
		var in v2SendInput
		if err := decodeJSON(r, &in); err != nil {
			return err
		}
		msg, err := s.Send(sendInputFromV2(in))
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{"MessageId": msg.ID})
	case "CreateEmailIdentity":
		var in struct {
			EmailIdentity string `json:"EmailIdentity"`
		}
		if err := decodeJSON(r, &in); err != nil {
			return err
		}
		identity, err := s.VerifyIdentity(in.EmailIdentity)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{
			"IdentityType":             v2IdentityType(identity.Type),
			"VerifiedForSendingStatus": true,
			"DkimAttributes":           map[string]any{"SigningEnabled": true, "Status": "SUCCESS"},
		})
	case "ListEmailIdentities":
		identities, err := s.Identities("")
		if err != nil {
			return err
		}
		items := make([]map[string]any, 0, len(identities))
		for _, identity := range identities {
			items = append(items, map[string]any{
				"IdentityType":       v2IdentityType(identity.Type),
				"IdentityName":       identity.Name,
				"SendingEnabled":     true,
				"VerificationStatus": "SUCCESS",
			})
		}
		return writeJSON(w, http.StatusOK, map[string]any{"EmailIdentities": items})
	case "GetEmailIdentity":
		identity, ok, err := s.Identity(id)
		if err != nil {
			return err
		}
		if !ok {
			return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "identity not found: " + id}
		}
		return writeJSON(w, http.StatusOK, map[string]any{
			"IdentityType":             v2IdentityType(identity.Type),
			"FeedbackForwardingStatus": true,
			"VerifiedForSendingStatus": true,
			"VerificationStatus":       "SUCCESS",
			"DkimAttributes":           map[string]any{"SigningEnabled": true, "Status": "SUCCESS"},
		})
	case "DeleteEmailIdentity":
		if err := s.DeleteIdentity(id); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{})
	case "CreateEmailTemplate":
		var in v2TemplateInput
		if err := decodeJSON(r, &in); err != nil {
			return err
		}
		if _, err := s.CreateTemplate(templateFromV2(in)); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{})
	case "UpdateEmailTemplate":
		var in v2TemplateInput
		if err := decodeJSON(r, &in); err != nil {
			return err
		}
		in.TemplateName = id
		if _, err := s.UpdateTemplate(templateFromV2(in)); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{})
	case "GetEmailTemplate":
		t, err := s.Template(id)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{
			"TemplateName":    t.Name,
			"TemplateContent": v2TemplateContent{Subject: t.Subject, Text: t.Text, Html: t.HTML},
		})
	case "ListEmailTemplates":
		templates, err := s.Templates()
		if err != nil {
			return err
		}
		items := make([]map[string]any, 0, len(templates))
		for _, t := range templates {
			items = append(items, map[string]any{"TemplateName": t.Name, "CreatedTimestamp": t.CreatedAt.Format(time.RFC3339)})
		}
		return writeJSON(w, http.StatusOK, map[string]any{"TemplatesMetadata": items})
	case "DeleteEmailTemplate":
		if err := s.DeleteTemplate(id); err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{})
	case "GetAccount":
		sent, err := s.SentCount()
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, map[string]any{
			"ProductionAccessEnabled": false,
			"SendingEnabled":          true,
			"EnforcementStatus":       "HEALTHY",
			"SendQuota": map[string]any{
				"Max24HourSend":   max24HourSend,
				"MaxSendRate":     maxSendRate,
				"SentLast24Hours": float64(sent),
			},
		})
	default:
		return &apierror.Error{StatusCode: http.StatusNotFound, Code: "NotFoundException", Message: "sesv2 operation is not implemented: " + r.Method + " " + r.URL.Path}
	}
}

func sendInputFromV2(in v2SendInput) SendInput {
	out := SendInput{
		Source:           in.FromEmailAddress,
		Destination:      Destination{To: in.Destination.ToAddresses, Cc: in.Destination.CcAddresses, Bcc: in.Destination.BccAddresses},
		ReplyTo:          in.ReplyToAddresses,
		ConfigurationSet: in.ConfigurationSetName,
		API:              "sesv2",
	}
	if len(in.EmailTags) > 0 {
		out.Tags = map[string]string{}
		for _, tag := range in.EmailTags {
			out.Tags[tag.Name] = tag.Value
		}
	}
	switch {
	case in.Content.Raw != nil:
		// The v2 API carries raw data base64-encoded; tolerate clients that
		// send the MIME text as is.
		if raw, err := base64.StdEncoding.DecodeString(in.Content.Raw.Data); err == nil {
			out.Raw = raw
		} else {
			out.Raw = []byte(in.Content.Raw.Data)
		}
	case in.Content.Template != nil:
		out.Template = in.Content.Template.TemplateName
		if out.Template == "" && in.Content.Template.TemplateArn != "" {
			out.Template = in.Content.Template.TemplateArn[strings.LastIndex(in.Content.Template.TemplateArn, "/")+1:]
		}
		out.TemplateData = in.Content.Template.TemplateData
	case in.Content.Simple != nil:
		out.Subject = in.Content.Simple.Subject.Data
		body := &Body{}
		if in.Content.Simple.Body.Text != nil {
			body.Text = in.Content.Simple.Body.Text.Data
		}
		if in.Content.Simple.Body.Html != nil {
			body.HTML = in.Content.Simple.Body.Html.Data
		}
		out.Simple = body
	}
	return out
}

func templateFromV2(in v2TemplateInput) Template {
	return Template{Name: in.TemplateName, Subject: in.TemplateContent.Subject, Text: in.TemplateContent.Text, HTML: in.TemplateContent.Html}
}

func v2IdentityType(identityType string) string {
	if identityType == IdentityDomain {
		return "DOMAIN"
	}
	return "EMAIL_ADDRESS"
}

func decodeJSON(r *http.Request, target any) error {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "BadRequestException", Message: "request body is not valid JSON"}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(payload)
}

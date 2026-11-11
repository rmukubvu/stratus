package iam

import (
	"encoding/xml"
	"net/http"
	"net/url"
)

// envelope is the <OperationResponse> wrapper every IAM query response has.
type envelope struct {
	XMLName          xml.Name
	XMLNS            string `xml:"xmlns,attr"`
	Result           *result
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

// result renders body as <OperationResult> with body's fields as children.
type result struct {
	name string
	body any
}

func (r *result) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	return e.EncodeElement(r.body, xml.StartElement{Name: xml.Name{Local: r.name}})
}

// respond writes the response for operation; a nil body writes only the
// response metadata, as IAM does for mutations.
func respond(w http.ResponseWriter, operation, requestID string, body any) error {
	env := envelope{
		XMLName:          xml.Name{Local: operation + "Response"},
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	}
	if body != nil {
		env.Result = &result{name: operation + "Result", body: body}
	}
	writeXML(w, http.StatusOK, env)
	return nil
}

// required returns a form field or a ValidationError naming it.
func required(form url.Values, name string) (string, error) {
	v := form.Get(name)
	if v == "" {
		return "", badRequest("ValidationError", name+" is required.")
	}
	return v, nil
}

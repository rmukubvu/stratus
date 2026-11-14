package awscompat

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"net/url"
)

// ParseQueryForm parses AWS query-protocol parameters from either the URL query
// string or the request body. Unlike net/http's ParseForm, it decodes a
// URL-encoded POST body even when the client omits the classic form content
// type, and it leaves the body readable afterwards, so the exact bytes can
// still be hashed to verify the request's signature.
//
// The result is cached in r.Form and r.PostForm.
func ParseQueryForm(r *http.Request) (url.Values, error) {
	if len(r.Form) > 0 {
		return r.Form, nil
	}

	query := r.URL.Query()
	bodyValues := url.Values{}
	if r.Body != nil && r.Body != http.NoBody && !isMultipart(r.Header.Get("Content-Type")) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if len(raw) > 0 {
			parsed, err := url.ParseQuery(string(raw))
			switch {
			case err == nil:
				bodyValues = parsed
			case len(query) == 0:
				return nil, err
			}
			// A body that is not a form (JSON alongside an Action in
			// the query string) adds no parameters.
		}
	}

	form := url.Values{}
	for key, values := range bodyValues {
		form[key] = append(form[key], values...)
	}
	for key, values := range query {
		form[key] = append(form[key], values...)
	}
	r.Form = form
	r.PostForm = bodyValues
	return form, nil
}

func isMultipart(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "multipart/form-data"
}

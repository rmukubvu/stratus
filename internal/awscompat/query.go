package awscompat

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
)

// ParseQueryForm parses AWS query-protocol parameters from either the URL query
// string or the request body. Unlike net/http's ParseForm, it falls back to
// decoding a URL-encoded POST body even when the client omits the classic
// form content type.
func ParseQueryForm(r *http.Request) (url.Values, error) {
	if err := r.ParseForm(); err == nil && len(r.Form) > 0 {
		return r.Form, nil
	}

	form := url.Values{}
	for key, values := range r.URL.Query() {
		form[key] = append(form[key], values...)
	}

	if r.Body == nil {
		return form, nil
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if len(raw) == 0 {
		return form, nil
	}

	bodyValues, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, err
	}
	for key, values := range bodyValues {
		form[key] = append(form[key], values...)
	}
	r.Form = form
	r.PostForm = bodyValues
	return form, nil
}

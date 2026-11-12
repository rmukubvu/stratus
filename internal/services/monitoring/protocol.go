package monitoring

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/stratus/internal/apierror"
)

// CloudWatch speaks both the query protocol (form in, XML out) and AWS JSON
// (newer SDKs). The alarm operations are written once against JSON-shaped
// request and response types; this file adapts them to the query protocol.

// decodeInput reads the request into dst, from a JSON body or a query form.
func decodeInput(r *http.Request, dst any) error {
	if expectsJSON(r) {
		payload, ok, err := parseJSONBody[json.RawMessage](r)
		if err != nil || !ok {
			return err
		}
		if err := json.Unmarshal(payload, dst); err != nil {
			return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "SerializationException", Message: err.Error()}
		}
		return nil
	}
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(formTree(form))
	if err := json.Unmarshal(raw, dst); err != nil {
		return validation("request parameters are malformed: " + err.Error())
	}
	return nil
}

// formTree turns query parameters into the nested shape JSON would carry:
// "Dimensions.member.1.Name=x" becomes {"Dimensions":[{"Name":"x"}]}. Leaves
// stay strings; the flex types below accept them where numbers or booleans
// are expected.
func formTree(form url.Values) map[string]any {
	root := map[string]any{}
	for key, values := range form {
		if key == "Action" || key == "Version" || len(values) == 0 {
			continue
		}
		insert(root, strings.Split(key, "."), values[0])
	}
	return normalize(root).(map[string]any)
}

// list is a query-protocol list being assembled, by 1-based index.
type list map[int]any

func insert(node map[string]any, path []string, value string) {
	head := path[0]
	if len(path) == 1 {
		node[head] = value
		return
	}
	if (path[1] == "member" || path[1] == "entry") && len(path) >= 3 {
		if n, err := strconv.Atoi(path[2]); err == nil {
			items, _ := node[head].(list)
			if items == nil {
				items = list{}
				node[head] = items
			}
			if len(path) == 3 {
				items[n] = value
				return
			}
			child, _ := items[n].(map[string]any)
			if child == nil {
				child = map[string]any{}
				items[n] = child
			}
			insert(child, path[3:], value)
			return
		}
	}
	child, _ := node[head].(map[string]any)
	if child == nil {
		child = map[string]any{}
		node[head] = child
	}
	insert(child, path[1:], value)
}

func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = normalize(child)
		}
		return t
	case list:
		indexes := make([]int, 0, len(t))
		for n := range t {
			indexes = append(indexes, n)
		}
		sort.Ints(indexes)
		out := make([]any, 0, len(indexes))
		for _, n := range indexes {
			out = append(out, normalize(t[n]))
		}
		return out
	}
	return v
}

// flexInt, flexFloat and flexBool accept a JSON number/boolean or the string
// a query form carries.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	v, err := strconv.Atoi(strings.Trim(string(b), `"`))
	if err != nil {
		return err
	}
	*f = flexInt(v)
	return nil
}

type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseFloat(strings.Trim(string(b), `"`), 64)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseBool(strings.Trim(string(b), `"`))
	if err != nil {
		return err
	}
	*f = flexBool(v)
	return nil
}

// respond writes result as AWS JSON, or as the query protocol's
// <ActionResponse><ActionResult>…</ActionResult> XML. A nil result writes an
// empty response.
func respond(w http.ResponseWriter, r *http.Request, action, requestID string, result any) error {
	if expectsJSON(r) {
		if result == nil {
			result = struct{}{}
		}
		writeJSON(w, http.StatusOK, result)
		return nil
	}
	var out bytes.Buffer
	out.WriteString(`<` + action + `Response xmlns="` + namespace + `">`)
	if result != nil {
		raw, err := json.Marshal(result)
		if err != nil {
			return internal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := jsonToXML(dec, &out, action+"Result"); err != nil {
			return internal(err)
		}
	}
	out.WriteString("<ResponseMetadata><RequestId>")
	_ = xml.EscapeText(&out, []byte(requestID))
	out.WriteString("</RequestId></ResponseMetadata></" + action + "Response>")
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Bytes())
	return nil
}

// jsonToXML re-encodes the next JSON value as an element called name, in
// field order: objects become nested elements, arrays <member> lists, and
// null is left out.
func jsonToXML(dec *json.Decoder, out *bytes.Buffer, name string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		out.WriteString("<" + name + ">")
		child := "member"
		for dec.More() {
			if t == '{' {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				child = key.(string)
			}
			if err := jsonToXML(dec, out, child); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // closing delimiter
			return err
		}
		out.WriteString("</" + name + ">")
	case nil:
	default:
		out.WriteString("<" + name + ">")
		_ = xml.EscapeText(out, []byte(scalarText(t)))
		out.WriteString("</" + name + ">")
	}
	return nil
}

func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

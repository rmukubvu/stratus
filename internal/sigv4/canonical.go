package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// PathStyle selects how the canonical URI is derived from the request path.
type PathStyle int

const (
	// PathDoubleEncoded is what every service but S3 verifies: each path
	// segment, as sent, is URI-encoded once more.
	PathDoubleEncoded PathStyle = iota
	// PathReencoded is S3's rule: the path is decoded and each segment
	// URI-encoded once, so a key signs the same however the client escaped
	// it on the wire. The AWS documentation's PutObject example sends
	// /test$file.text and signs /test%24file.text, and Go's net/http sends
	// ":", "+", "=", "(", ")", "@" and "," raw where an SDK signs them
	// escaped.
	PathReencoded
)

// CanonicalRequest builds the SigV4 canonical request for r as auth signed
// it, with payloadHash as the last line.
func CanonicalRequest(r *http.Request, auth *Auth, style PathStyle, payloadHash string) string {
	var headers strings.Builder
	for _, name := range auth.SignedHeaders {
		headers.WriteString(name)
		headers.WriteByte(':')
		headers.WriteString(headerValue(r, name))
		headers.WriteByte('\n')
	}
	return strings.Join([]string{
		r.Method,
		canonicalURI(wirePath(r), style),
		canonicalQuery(wireQuery(r), auth.Presigned),
		headers.String(),
		strings.Join(auth.SignedHeaders, ";"),
		payloadHash,
	}, "\n")
}

// StringToSign is the string the request signature is computed over.
func StringToSign(auth *Auth, canonicalRequest string) string {
	return Algorithm + "\n" + auth.AmzDate + "\n" + auth.Scope() + "\n" + hashHex([]byte(canonicalRequest))
}

// SigningKey derives the key for one day, region and service.
func SigningKey(secret, date, region, service string) []byte {
	key := hmacSHA256([]byte("AWS4"+secret), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	return hmacSHA256(key, scopeTerminator)
}

// Signature is the hex HMAC of stringToSign under key.
func Signature(key []byte, stringToSign string) string {
	return hex.EncodeToString(hmacSHA256(key, stringToSign))
}

// URIEncode percent-encodes everything except the RFC 3986 unreserved
// characters, as SigV4 requires.
func URIEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

// wirePath is the request path exactly as the client sent it.
func wirePath(r *http.Request) string {
	uri := r.RequestURI
	if uri == "" {
		return r.URL.EscapedPath()
	}
	if i := strings.Index(uri, "://"); i >= 0 && !strings.HasPrefix(uri, "/") {
		// Absolute form, as sent to a proxy: drop scheme and authority.
		uri = uri[i+3:]
		if j := strings.IndexByte(uri, '/'); j >= 0 {
			uri = uri[j:]
		} else {
			uri = "/"
		}
	}
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	return uri
}

// wireQuery is the raw query string as sent.
func wireQuery(r *http.Request) string {
	if r.RequestURI != "" {
		if i := strings.IndexByte(r.RequestURI, '?'); i >= 0 {
			return r.RequestURI[i+1:]
		}
		return ""
	}
	return r.URL.RawQuery
}

func canonicalURI(path string, style PathStyle) string {
	if path == "" {
		return "/"
	}
	switch style {
	case PathReencoded:
		segments := strings.Split(path, "/")
		for i, s := range segments {
			if decoded, err := url.PathUnescape(s); err == nil {
				s = decoded
			}
			segments[i] = URIEncode(s)
		}
		return strings.Join(segments, "/")
	default:
		segments := strings.Split(path, "/")
		for i, s := range segments {
			segments[i] = URIEncode(s)
		}
		return strings.Join(segments, "/")
	}
}

type queryPair struct{ name, value string }

// canonicalQuery sorts the query parameters by encoded name, then value,
// dropping X-Amz-Signature from presigned requests.
func canonicalQuery(raw string, presigned bool) string {
	var pairs []queryPair
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		name, value = unescapeQuery(name), unescapeQuery(value)
		if presigned && name == "X-Amz-Signature" {
			continue
		}
		pairs = append(pairs, queryPair{URIEncode(name), URIEncode(value)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].name != pairs[j].name {
			return pairs[i].name < pairs[j].name
		}
		return pairs[i].value < pairs[j].value
	})
	encoded := make([]string, len(pairs))
	for i, p := range pairs {
		encoded[i] = p.name + "=" + p.value
	}
	return strings.Join(encoded, "&")
}

func unescapeQuery(s string) string {
	if decoded, err := url.QueryUnescape(s); err == nil {
		return decoded
	}
	return s
}

// headerValue is a header's canonical value: every value trimmed, inner
// runs of spaces collapsed, and the values joined with commas.
func headerValue(r *http.Request, name string) string {
	switch name {
	case "host":
		return r.Host
	case "content-length":
		if v := r.Header.Get("Content-Length"); v != "" {
			return v
		}
		if r.ContentLength >= 0 {
			return strconv.FormatInt(r.ContentLength, 10)
		}
	case "transfer-encoding":
		if len(r.TransferEncoding) > 0 {
			return strings.Join(r.TransferEncoding, ",")
		}
	}
	values := r.Header.Values(name)
	trimmed := make([]string, len(values))
	for i, v := range values {
		trimmed[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(trimmed, ",")
}

func isUnreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// EmptyPayloadHash is the SHA-256 of an empty body.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

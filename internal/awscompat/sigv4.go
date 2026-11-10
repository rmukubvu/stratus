package awscompat

import "regexp"

type SigV4Identity struct {
	AccessKeyID string
	Date        string
	Region      string
	Service     string
}

var sigV4Re = regexp.MustCompile(`Credential=([^/]+)/(\d{8})/([^/]+)/([^/,]+)/aws4_request`)

func ParseSigV4Authorization(header string) *SigV4Identity {
	match := sigV4Re.FindStringSubmatch(header)
	if match == nil {
		return nil
	}

	return &SigV4Identity{
		AccessKeyID: match[1],
		Date:        match[2],
		Region:      match[3],
		Service:     match[4],
	}
}

// ParseSigV4Credential parses a presigned URL's X-Amz-Credential value,
// <key>/<date>/<region>/<service>/aws4_request.
func ParseSigV4Credential(credential string) *SigV4Identity {
	if credential == "" {
		return nil
	}
	return ParseSigV4Authorization("Credential=" + credential)
}

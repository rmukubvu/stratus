package awscompat

import "encoding/xml"

type QueryErrorResponse struct {
	XMLName   xml.Name   `xml:"ErrorResponse"`
	Error     QueryError `xml:"Error"`
	RequestID string     `xml:"RequestId"`
}

type QueryError struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// EC2ErrorResponse is the error shape of the EC2 query protocol, which
// differs from the other query services' <ErrorResponse>.
type EC2ErrorResponse struct {
	XMLName   xml.Name     `xml:"Response"`
	Errors    []QueryError `xml:"Errors>Error"`
	RequestID string       `xml:"RequestID"`
}

type S3ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

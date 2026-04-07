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

type S3ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

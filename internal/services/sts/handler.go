package sts

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
)

const namespace = "https://sts.amazonaws.com/doc/2011-06-15/"

// Handle serves one STS query-protocol request.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string, caller Caller) error {
	switch operation {
	case "GetCallerIdentity":
		out, err := s.GetCallerIdentity(caller)
		if err != nil {
			return err
		}
		writeXML(w, NewGetCallerIdentityResponse(out, requestID))
		return nil
	case "AssumeRole":
		form, err := awscompat.ParseQueryForm(r)
		if err != nil {
			return validation("request body is not a valid query string")
		}
		duration := 0
		if raw := form.Get("DurationSeconds"); raw != "" {
			if duration, err = strconv.Atoi(raw); err != nil {
				return validation("DurationSeconds must be a number")
			}
		}
		session, err := s.AssumeRole(AssumeRoleInput{
			RoleARN:         form.Get("RoleArn"),
			RoleSessionName: form.Get("RoleSessionName"),
			DurationSeconds: duration,
		})
		if err != nil {
			return err
		}
		writeXML(w, newAssumeRoleResponse(session, requestID))
		return nil
	case "GetAccessKeyInfo":
		form, err := awscompat.ParseQueryForm(r)
		if err != nil {
			return validation("request body is not a valid query string")
		}
		account, err := s.GetAccessKeyInfo(form.Get("AccessKeyId"))
		if err != nil {
			return err
		}
		writeXML(w, accessKeyInfoResponse{XMLNS: namespace, Account: account, RequestID: requestID})
		return nil
	case "":
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidAction", Message: "missing Action for STS query request"}
	default:
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidAction", Message: "The action " + operation + " is not valid for this endpoint."}
	}
}

type CallerIdentityResponseEnvelope struct {
	XMLName          xml.Name             `xml:"GetCallerIdentityResponse"`
	XMLNS            string               `xml:"xmlns,attr"`
	Result           CallerIdentityResult `xml:"GetCallerIdentityResult"`
	ResponseMetadata responseMetadata     `xml:"ResponseMetadata"`
}

type CallerIdentityResult struct {
	Account string `xml:"Account"`
	ARN     string `xml:"Arn"`
	UserID  string `xml:"UserId"`
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

func NewGetCallerIdentityResponse(output GetCallerIdentityOutput, requestID string) CallerIdentityResponseEnvelope {
	return CallerIdentityResponseEnvelope{
		XMLNS:            namespace,
		Result:           CallerIdentityResult{Account: output.Account, ARN: output.ARN, UserID: output.UserID},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	}
}

type assumeRoleResponse struct {
	XMLName xml.Name `xml:"AssumeRoleResponse"`
	XMLNS   string   `xml:"xmlns,attr"`
	Result  struct {
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"Credentials"`
		AssumedRoleUser struct {
			ARN           string `xml:"Arn"`
			AssumedRoleID string `xml:"AssumedRoleId"`
		} `xml:"AssumedRoleUser"`
		PackedPolicySize int `xml:"PackedPolicySize"`
	} `xml:"AssumeRoleResult"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

func newAssumeRoleResponse(s Session, requestID string) assumeRoleResponse {
	var out assumeRoleResponse
	out.XMLNS = namespace
	out.Result.Credentials.AccessKeyID = s.AccessKeyID
	out.Result.Credentials.SecretAccessKey = s.SecretAccessKey
	out.Result.Credentials.SessionToken = s.SessionToken
	out.Result.Credentials.Expiration = s.Expiration.UTC().Format(time.RFC3339)
	out.Result.AssumedRoleUser.ARN = s.AssumedRoleARN()
	out.Result.AssumedRoleUser.AssumedRoleID = s.AssumedRoleID()
	out.ResponseMetadata.RequestID = requestID
	return out
}

type accessKeyInfoResponse struct {
	XMLName   xml.Name `xml:"GetAccessKeyInfoResponse"`
	XMLNS     string   `xml:"xmlns,attr"`
	Account   string   `xml:"GetAccessKeyInfoResult>Account"`
	RequestID string   `xml:"ResponseMetadata>RequestId"`
}

func writeXML(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(payload)
}

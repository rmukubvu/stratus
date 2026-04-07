package sts

import (
	"encoding/xml"
	"fmt"
)

const namespace = "https://sts.amazonaws.com/doc/2011-06-15/"

type Service struct {
	accountID string
}

type GetCallerIdentityInput struct {
	AccessKeyID string
}

type GetCallerIdentityOutput struct {
	Account string
	ARN     string
	UserID  string
}

type CallerIdentityResponseEnvelope struct {
	XMLName          xml.Name             `xml:"GetCallerIdentityResponse"`
	XMLNS            string               `xml:"xmlns,attr"`
	Result           CallerIdentityResult `xml:"GetCallerIdentityResult"`
	ResponseMetadata CallerIdentityMeta   `xml:"ResponseMetadata"`
}

type CallerIdentityResult struct {
	Account string `xml:"Account"`
	ARN     string `xml:"Arn"`
	UserID  string `xml:"UserId"`
}

type CallerIdentityMeta struct {
	RequestID string `xml:"RequestId"`
}

func NewService() *Service {
	return &Service{accountID: "000000000000"}
}

func (s *Service) GetCallerIdentity(input GetCallerIdentityInput) GetCallerIdentityOutput {
	accessKey := input.AccessKeyID
	if accessKey == "" {
		accessKey = "STRATUSLOCAL"
	}

	return GetCallerIdentityOutput{
		Account: s.accountID,
		ARN:     fmt.Sprintf("arn:aws:iam::%s:user/stratus", s.accountID),
		UserID:  accessKey + ":stratus",
	}
}

func NewGetCallerIdentityResponse(output GetCallerIdentityOutput, requestID string) CallerIdentityResponseEnvelope {
	return CallerIdentityResponseEnvelope{
		XMLNS: namespace,
		Result: CallerIdentityResult{
			Account: output.Account,
			ARN:     output.ARN,
			UserID:  output.UserID,
		},
		ResponseMetadata: CallerIdentityMeta{
			RequestID: requestID,
		},
	}
}

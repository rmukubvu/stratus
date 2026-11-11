package httpapi

import (
	"context"

	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/scope"
)

type contextKey string

const requestMetadataKey contextKey = "request-metadata"

type RequestMetadata struct {
	RequestID      string
	Classification Classification
	SigV4          *awscompat.SigV4Identity
	// Scope is the account and region the request acts in.
	Scope scope.Scope
}

func WithRequestMetadata(ctx context.Context, metadata RequestMetadata) context.Context {
	return context.WithValue(ctx, requestMetadataKey, metadata)
}

func MetadataFromContext(ctx context.Context) RequestMetadata {
	metadata, _ := ctx.Value(requestMetadataKey).(RequestMetadata)
	return metadata
}

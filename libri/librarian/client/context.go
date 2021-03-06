package client

import (
	"time"
	"errors"

	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc/metadata"
)

const (
	signatureKey = "signature"
)

var (
	errContextMissingMetadata = errors.New("context unexpectedly missing metadata")
	errContextMissingSignature = errors.New("metadata signature key unexpectedly does not exist")
)


// NewSignatureContext creates a new context with the signed JSON web token (JWT) string.
func NewSignatureContext(ctx context.Context, signedJWT string) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(signatureKey, signedJWT))
}

// NewIncomingSignatureContext creates a new context with the signed JSON web token (JWT) string
// in the incoming metadata field. This function should only be used for testing.
func NewIncomingSignatureContext(ctx context.Context, signedJWT string) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs(signatureKey, signedJWT))
}

// FromSignatureContext extracts the signed JSON web token from the context.
func FromSignatureContext(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", errContextMissingMetadata
	}
	signedJWTs, exists := md[signatureKey]
	if !exists {
		return "", errContextMissingSignature
	}
	return signedJWTs[0], nil
}

// NewSignedContext creates a new context with a request signature.
func NewSignedContext(signer Signer, request proto.Message) (context.Context, error) {

	ctx := context.Background()

	// sign the message
	signedJWT, err := signer.Sign(request)
	if err != nil {
		return nil, err
	}
	ctx = NewSignatureContext(ctx, signedJWT)
	return ctx, nil
}

// NewSignedTimeoutContext creates a new context with a timeout and request signature.
func NewSignedTimeoutContext(signer Signer, request proto.Message, timeout time.Duration) (
	context.Context, context.CancelFunc, error) {

	ctx, err := NewSignedContext(signer, request)
	if err != nil {
		return nil, func() {}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, cancel, nil
}

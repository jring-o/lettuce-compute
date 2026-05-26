package client

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// gRPC metadata keys for Ed25519 request authentication. These MUST match the
// server interceptor (services/infrastructure/internal/server/grpc_auth.go). The
// "-bin" suffix marks raw-binary metadata, which gRPC base64-encodes on the wire.
const (
	authPubKeyMeta    = "x-lettuce-pubkey-bin"
	authTimestampMeta = "x-lettuce-timestamp"
	authSignatureMeta = "x-lettuce-signature-bin"
)

// authPublicMethods are the discovery RPCs that require no authentication on the
// server. The client may be built before an identity exists (e.g. `attach`), so we
// never attach auth for these and they work without a key.
var authPublicMethods = map[string]bool{
	"/lettuce.volunteer.v1.VolunteerService/GetServerStatus": true,
	"/lettuce.volunteer.v1.VolunteerService/GetHeadInfo":     true,
}

// Identity holds the volunteer's Ed25519 keypair used to sign outgoing requests.
type Identity struct {
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// signingClientInterceptor returns a UnaryClientInterceptor that signs every
// outgoing (non-public) request with the volunteer's Ed25519 private key, matching
// the server's canonical message format:
//
//	<unix-ts>:<method>:<hex(sha256(deterministic-marshal(req)))>
//
// The `method` passed by gRPC equals the server's FullMethod
// (e.g. "/lettuce.volunteer.v1.VolunteerService/SubmitResult").
//
// If id is nil (discovery-only client built before registration), no auth metadata
// is attached; the public discovery methods still succeed and any authenticated
// call will be rejected by the server, which is the correct behavior.
func signingClientInterceptor(id *Identity) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if id == nil || authPublicMethods[method] {
			return invoker(ctx, method, req, reply, cc, opts...)
		}

		msg, ok := req.(proto.Message)
		if !ok {
			return fmt.Errorf("request for %s is not a protobuf message", method)
		}
		// Deterministic marshal must match the server's hashing. Both modules share
		// the workspace protobuf-go version, so the bytes (and hash) are identical.
		requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshaling request for signing: %w", err)
		}

		unixTs := time.Now().Unix()
		sum := sha256.Sum256(requestBytes)
		signed := fmt.Sprintf("%d:%s:%s", unixTs, method, hex.EncodeToString(sum[:]))
		sig := ed25519.Sign(id.PrivateKey, []byte(signed))

		ctx = metadata.AppendToOutgoingContext(ctx,
			authPubKeyMeta, string(id.PublicKey),
			authTimestampMeta, strconv.FormatInt(unixTs, 10),
			authSignatureMeta, string(sig),
		)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

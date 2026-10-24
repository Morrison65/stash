// Package remotefs is the Go client for stash-fsd, the Windows service that
// owns all access to the media library. It implements models.FS (and the
// extended file.LibraryFS) on top of the gRPC contract in
// proto/stashfs/v1/stashfs.proto so that the existing scan, clean, delete and
// serving code works unchanged against a remote file system.
package remotefs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	pb "github.com/stashapp/stash/pkg/remotefs/stashfsv1"
)

const (
	// maxMessageSize must exceed the largest ReadAt chunk (8 MiB) plus framing.
	maxMessageSize = 16 << 20

	defaultCallTimeout = 2 * time.Minute
)

// Config configures the connection to stash-fsd.
type Config struct {
	// Address is host:port of the gRPC listener, e.g. host.docker.internal:7700.
	Address string
	// Secret is the shared bearer token configured in stash-fsd.toml.
	Secret string
	// TLS enables transport security. Plain TCP is used otherwise; only do
	// that on a trusted network segment (e.g. the Docker Desktop host link).
	TLS bool
	// TLSInsecureSkipVerify disables certificate verification (self-signed).
	TLSInsecureSkipVerify bool
	// CallTimeout bounds unary metadata calls. Streaming calls are bounded
	// by their context only.
	CallTimeout time.Duration
}

// Client is a connection to stash-fsd.
type Client struct {
	conn        *grpc.ClientConn
	fs          pb.FileSystemClient
	hasher      pb.HasherClient
	media       pb.MediaClient
	callTimeout time.Duration
}

type bearerToken string

func (t bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

// RequireTransportSecurity is false so the token can be sent over plain TCP
// inside a trusted network.
func (t bearerToken) RequireTransportSecurity() bool { return false }

// Dial connects to stash-fsd. The connection is established lazily; use
// Ping to verify connectivity.
func Dial(cfg Config) (*Client, error) {
	if cfg.Address == "" {
		return nil, errors.New("remotefs: address is required")
	}
	if cfg.Secret == "" {
		return nil, errors.New("remotefs: shared secret is required")
	}

	var transport grpc.DialOption
	if cfg.TLS {
		transport = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.TLSInsecureSkipVerify, //nolint:gosec // opt-in for self-signed certs
		}))
	} else {
		transport = grpc.WithTransportCredentials(insecure.NewCredentials())
	}

	conn, err := grpc.NewClient(cfg.Address,
		transport,
		grpc.WithPerRPCCredentials(bearerToken(cfg.Secret)),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMessageSize),
			grpc.MaxCallSendMsgSize(maxMessageSize),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("remotefs: dialing %s: %w", cfg.Address, err)
	}

	return NewClient(conn, cfg.CallTimeout), nil
}

// NewClient wraps an existing connection (used by tests).
func NewClient(conn *grpc.ClientConn, callTimeout time.Duration) *Client {
	if callTimeout <= 0 {
		callTimeout = defaultCallTimeout
	}
	return &Client{
		conn:        conn,
		fs:          pb.NewFileSystemClient(conn),
		hasher:      pb.NewHasherClient(conn),
		media:       pb.NewMediaClient(conn),
		callTimeout: callTimeout,
	}
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Ping verifies the service is reachable and the secret is accepted.
func (c *Client) Ping(ctx context.Context) (*pb.CapabilitiesResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ret, err := c.media.Capabilities(ctx, &pb.Empty{})
	if err != nil {
		return nil, mapError("ping", "", err)
	}
	return ret, nil
}

// unaryCtx derives a context for unary calls. The models.FS interface has no
// context parameter, so a background context with the call timeout is used.
func (c *Client) unaryCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.callTimeout)
}

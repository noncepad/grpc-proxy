// Copyright 2017 Michal Witkowski. All Rights Reserved.
// See LICENSE for licensing terms.

package proxy_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jhump/grpctunnel"
	"github.com/jhump/grpctunnel/tunnelpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/noncepad/grpc-proxy/proxy"
	pb "github.com/noncepad/grpc-proxy/testservice"
)

const (
	pingDefaultValue   = "I like kittens."
	clientMdKey        = "test-client-header"
	serverHeaderMdKey  = "test-client-header"
	serverTrailerMdKey = "test-client-trailer"

	rejectingMdKey = "test-reject-rpc-if-in-context"

	countListResponses = 20
)

// asserting service is implemented on the server side and serves as a handler for stuff.
type assertingService struct {
	pb.UnimplementedTestServiceServer

	t *testing.T
}

func (s *assertingService) PingEmpty(ctx context.Context, _ *pb.Empty) (*pb.PingResponse, error) {
	// Check that this call has client's metadata.
	md, ok := metadata.FromIncomingContext(ctx)
	assert.True(s.t, ok, "PingEmpty call must have metadata in context")
	_, ok = md[clientMdKey]
	assert.True(s.t, ok, "PingEmpty call must have clients's custom headers in metadata")

	return &pb.PingResponse{Value: pingDefaultValue, Counter: 42}, nil
}

func (s *assertingService) Ping(ctx context.Context, ping *pb.PingRequest) (*pb.PingResponse, error) {
	// Send user trailers and headers.
	grpc.SendHeader(ctx, metadata.Pairs(serverHeaderMdKey, "I like turtles."))         //nolint: errcheck
	grpc.SetTrailer(ctx, metadata.Pairs(serverTrailerMdKey, "I like ending turtles.")) //nolint: errcheck

	return &pb.PingResponse{Value: ping.Value, Counter: 42}, nil
}

func (s *assertingService) PingError(context.Context, *pb.PingRequest) (*pb.Empty, error) {
	return nil, status.Errorf(codes.FailedPrecondition, "Userspace error.")
}

func (s *assertingService) PingList(ping *pb.PingRequest, stream pb.TestService_PingListServer) error {
	// Send user trailers and headers.
	stream.SendHeader(metadata.Pairs(serverHeaderMdKey, "I like turtles.")) //nolint: errcheck

	for i := range countListResponses {
		stream.Send(&pb.PingResponse{Value: ping.Value, Counter: int32(i)}) //nolint: errcheck
	}

	stream.SetTrailer(metadata.Pairs(serverTrailerMdKey, "I like ending turtles."))

	return nil
}

func (s *assertingService) PingStream(stream pb.TestService_PingStreamServer) error {
	stream.SendHeader(metadata.Pairs(serverHeaderMdKey, "I like turtles.")) //nolint: errcheck

	counter := int32(0)

	for {
		ping, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			require.NoError(s.t, err, "can't fail reading stream")

			return err
		}

		pong := &pb.PingResponse{Value: ping.Value, Counter: counter}

		if err := stream.Send(pong); err != nil {
			require.NoError(s.t, err, "can't fail sending back a pong")
		}

		counter++
	}

	stream.SetTrailer(metadata.Pairs(serverTrailerMdKey, "I like ending turtles."))

	return nil
}

// ProxyOne2OneSuite tests the "happy" path of handling: that everything works in absence of connection issues.
type ProxyOne2OneSuite struct {
	suite.Suite

	serverListener   net.Listener
	server           *grpc.Server
	proxyListener    net.Listener
	proxy            *grpc.Server
	serverClientConn *grpc.ClientConn

	tunnelHandler  *grpctunnel.TunnelServiceHandler
	tunnelOpenedCh chan struct{}

	client       *grpc.ClientConn
	testClient   pb.TestServiceClient
	tunnelClient tunnelpb.TunnelServiceClient

	ctx       context.Context //nolint:containedctx
	ctxCancel context.CancelFunc
}

func (s *ProxyOne2OneSuite) SetupTest() {
	s.ctx, s.ctxCancel = context.WithTimeout(context.TODO(), 120*time.Second)
}

func (s *ProxyOne2OneSuite) TearDownTest() {
	s.ctxCancel()
}

func (s *ProxyOne2OneSuite) TestPingEmptyCarriesClientMetadata() {
	ctx := metadata.NewOutgoingContext(s.ctx, metadata.Pairs(clientMdKey, "true"))
	out, err := s.testClient.PingEmpty(ctx, &pb.Empty{})
	require.NoError(s.T(), err, "PingEmpty should succeed without errors")
	require.True(s.T(), proto.Equal(&pb.PingResponse{Value: pingDefaultValue, Counter: 42}, out))
}

func (s *ProxyOne2OneSuite) TestPingEmpty_StressTest() {
	for range 50 {
		s.TestPingEmptyCarriesClientMetadata()
	}
}

func (s *ProxyOne2OneSuite) TestPingCarriesServerHeadersAndTrailers() {
	s.testPingCarriesServerHeadersAndTrailers(s.testClient)
}

func (s *ProxyOne2OneSuite) testPingCarriesServerHeadersAndTrailers(client pb.TestServiceClient) {
	headerMd := make(metadata.MD)
	trailerMd := make(metadata.MD)
	// This is an awkward calling convention... but meh.
	out, err := client.Ping(s.ctx, &pb.PingRequest{Value: "foo"}, grpc.Header(&headerMd), grpc.Trailer(&trailerMd))
	require.NoError(s.T(), err, "Ping should succeed without errors")
	require.True(s.T(), proto.Equal(&pb.PingResponse{Value: "foo", Counter: 42}, out))
	assert.Contains(s.T(), headerMd, serverHeaderMdKey, "server response headers must contain server data")
	assert.Len(s.T(), trailerMd, 1, "server response trailers must contain server data")
}

func (s *ProxyOne2OneSuite) TestPingErrorPropagatesAppError() {
	s.testPingErrorPropagatesAppError(s.testClient)
}

func (s *ProxyOne2OneSuite) testPingErrorPropagatesAppError(client pb.TestServiceClient) {
	_, err := client.PingError(s.ctx, &pb.PingRequest{Value: "foo"})
	require.Error(s.T(), err, "PingError should never succeed")
	assert.Equal(s.T(), codes.FailedPrecondition, status.Code(err))
	assert.Equal(s.T(), "Userspace error.", status.Convert(err).Message())
}

func (s *ProxyOne2OneSuite) TestDirectorErrorIsPropagated() {
	// See SetupSuite where the StreamDirector has a special case.
	ctx := metadata.NewOutgoingContext(s.ctx, metadata.Pairs(rejectingMdKey, "true"))
	_, err := s.testClient.Ping(ctx, &pb.PingRequest{Value: "foo"})
	require.Error(s.T(), err, "Director should reject this RPC")
	assert.Equal(s.T(), codes.PermissionDenied, status.Code(err))
	assert.Equal(s.T(), "testing rejection", status.Convert(err).Message())
}

func (s *ProxyOne2OneSuite) TestPingStream_FullDuplexWorks() {
	s.testStream(s.testClient)
}

func (s *ProxyOne2OneSuite) TestReverseTunnel() {
	channelServer := grpctunnel.NewReverseTunnelServer(s.tunnelClient)

	pb.RegisterTestServiceServer(channelServer, &assertingService{t: s.T()})

	go func() {
		channelServer.Serve(s.ctx) //nolint:errcheck // we are not interested in the error here
	}()

	// wait for the tunnel to open
	select {
	case <-s.ctx.Done():
		s.FailNow("timeout waiting for tunnel to open")
	case <-s.tunnelOpenedCh:
	}

	fakeConn := s.tunnelHandler.KeyAsChannel("asdf")
	client := pb.NewTestServiceClient(fakeConn)

	s.testPingCarriesServerHeadersAndTrailers(client)
	s.testPingErrorPropagatesAppError(client)
	s.testStream(client)
}

func (s *ProxyOne2OneSuite) testStream(client pb.TestServiceClient) {
	stream, err := client.PingStream(s.ctx)
	require.NoError(s.T(), err, "PingStream request should be successful.")

	for i := range countListResponses {
		ping := &pb.PingRequest{Value: fmt.Sprintf("foo:%d", i)}
		require.NoError(s.T(), stream.Send(ping), "sending to PingStream must not fail")

		var resp *pb.PingResponse

		resp, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}

		if i == 0 {
			// Check that the header arrives before all entries.
			var headerMd metadata.MD
			headerMd, err = stream.Header()
			require.NoError(s.T(), err, "PingStream headers should not error.")
			assert.Contains(s.T(), headerMd, serverHeaderMdKey, "PingStream response headers user contain metadata")
		}

		assert.EqualValues(s.T(), i, resp.Counter, "ping roundtrip must succeed with the correct id")
	}

	require.NoError(s.T(), stream.CloseSend(), "no error on close send")
	_, err = stream.Recv()
	require.Equal(s.T(), io.EOF, err, "stream should close with io.EOF, meaining OK")
	// Check that the trailer headers are here.
	trailerMd := stream.Trailer()
	assert.Len(s.T(), trailerMd, 1, "PingList trailer headers user contain metadata")
}

func (s *ProxyOne2OneSuite) TestPingStream_StressTest() {
	for range 50 {
		s.TestPingStream_FullDuplexWorks()
	}
}

func (s *ProxyOne2OneSuite) SetupSuite() {
	var err error

	s.proxyListener, err = net.Listen("tcp", "127.0.0.1:0")
	require.NoError(s.T(), err, "must be able to allocate a port for proxyListener")
	s.serverListener, err = net.Listen("tcp", "127.0.0.1:0")
	require.NoError(s.T(), err, "must be able to allocate a port for serverListener")

	s.server = grpc.NewServer()
	pb.RegisterTestServiceServer(s.server, &assertingService{t: s.T()})

	s.tunnelOpenedCh = make(chan struct{})

	s.tunnelHandler = grpctunnel.NewTunnelServiceHandler(
		grpctunnel.TunnelServiceHandlerOptions{
			OnReverseTunnelOpen: func(grpctunnel.TunnelChannel) {
				s.T().Logf("[tunnel] open reverse tunnel")
			},
			OnReverseTunnelClose: func(grpctunnel.TunnelChannel) {
				s.T().Logf("[tunnel] close reverse tunnel")
			},
			AffinityKey: func(grpctunnel.TunnelChannel) any {
				s.T().Logf("[tunnel] get affinity key")

				select {
				case <-s.ctx.Done():
					return "fail"
				case s.tunnelOpenedCh <- struct{}{}:
					return "asdf"
				}
			},
		},
	)

	tunnelpb.RegisterTunnelServiceServer(s.server, s.tunnelHandler.Service())

	// Setup of the proxy's Director.
	s.serverClientConn, err = grpc.NewClient(
		s.serverListener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(proxy.Codec())),
	)
	require.NoError(s.T(), err, "must not error on deferred client Dial")

	director := func(ctx context.Context, _ string) (proxy.Mode, []proxy.Backend, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			if _, exists := md[rejectingMdKey]; exists {
				return proxy.One2One, nil, status.Errorf(codes.PermissionDenied, "testing rejection")
			}
		}

		return proxy.One2One, []proxy.Backend{
			&proxy.SingleBackend{
				GetConn: func(ctx context.Context) (context.Context, *grpc.ClientConn, error) {
					md, _ := metadata.FromIncomingContext(ctx)
					// Explicitly copy the metadata, otherwise the tests will fail.
					outCtx := metadata.NewOutgoingContext(ctx, md.Copy())

					return outCtx, s.serverClientConn, nil
				},
			},
		}, nil
	}

	s.proxy = grpc.NewServer(
		grpc.ForceServerCodecV2(proxy.Codec()),
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
	)

	// Ping handler is handled as an explicit registration and not as a TransparentHandler.
	proxy.RegisterService(s.proxy, director,
		"talos.testproto.TestService",
		proxy.WithMethodNames("Ping"),
	)

	// Start the serving loops.
	s.T().Logf("starting grpc.Server at: %v", s.serverListener.Addr().String())

	go func() {
		s.server.Serve(s.serverListener) //nolint: errcheck
	}()

	s.T().Logf("starting grpc.Proxy at: %v", s.proxyListener.Addr().String())

	go func() {
		s.proxy.Serve(s.proxyListener) //nolint: errcheck
	}()

	clientConn, err := grpc.NewClient(
		strings.Replace(s.proxyListener.Addr().String(), "127.0.0.1", "localhost", 1),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(s.T(), err, "must not error on deferred client Dial")
	s.testClient = pb.NewTestServiceClient(clientConn)
	s.tunnelClient = tunnelpb.NewTunnelServiceClient(clientConn)
}

func (s *ProxyOne2OneSuite) TearDownSuite() {
	if s.client != nil {
		s.client.Close() //nolint: errcheck
	}

	if s.serverClientConn != nil {
		s.serverClientConn.Close() //nolint: errcheck
	}

	// Close all transports so the logs don't get spammy.
	time.Sleep(10 * time.Millisecond)

	if s.proxy != nil {
		s.proxy.Stop()
		s.proxyListener.Close() //nolint: errcheck
	}

	if s.serverListener != nil {
		s.server.Stop()
		s.serverListener.Close() //nolint: errcheck
	}
}

func TestProxyOne2OneSuite(t *testing.T) {
	suite.Run(t, &ProxyOne2OneSuite{})
}

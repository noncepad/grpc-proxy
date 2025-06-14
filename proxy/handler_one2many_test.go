// Copyright 2017 Michal Witkowski. All Rights Reserved.
// Copyright 2019 Andrey Smirnov. All Rights Reserved.
// See LICENSE for licensing terms.

package proxy_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/noncepad/grpc-proxy/proxy"
	pb "github.com/noncepad/grpc-proxy/testservice"
)

const (
	numUpstreams = 5
)

// asserting service is implemented on the server side and serves as a handler for stuff.
type assertingMultiService struct {
	pb.UnimplementedMultiServiceServer

	t      *testing.T
	server string
}

func (s *assertingMultiService) PingEmpty(ctx context.Context, _ *pb.Empty) (*pb.MultiPingReply, error) {
	// Check that this call has client's metadata.
	md, ok := metadata.FromIncomingContext(ctx)
	assert.True(s.t, ok, "PingEmpty call must have metadata in context")
	_, ok = md[clientMdKey]
	assert.True(s.t, ok, "PingEmpty call must have clients's custom headers in metadata")

	return &pb.MultiPingReply{
		Response: []*pb.MultiPingResponse{
			{
				Value:   pingDefaultValue,
				Counter: 42,
				Server:  s.server,
			},
		},
	}, nil
}

func (s *assertingMultiService) Ping(ctx context.Context, ping *pb.PingRequest) (*pb.MultiPingReply, error) {
	// Send user trailers and headers.
	grpc.SendHeader(ctx, metadata.Pairs(serverHeaderMdKey, "I like turtles."))         //nolint: errcheck
	grpc.SetTrailer(ctx, metadata.Pairs(serverTrailerMdKey, "I like ending turtles.")) //nolint: errcheck

	return &pb.MultiPingReply{
		Response: []*pb.MultiPingResponse{
			{
				Value:   ping.Value,
				Counter: 42,
				Server:  s.server,
			},
		},
	}, nil
}

func (s *assertingMultiService) PingError(context.Context, *pb.PingRequest) (*pb.EmptyReply, error) {
	return nil, status.Errorf(codes.FailedPrecondition, "Userspace error.")
}

func (s *assertingMultiService) PingList(ping *pb.PingRequest, stream pb.MultiService_PingListServer) error {
	// Send user trailers and headers.
	stream.SendHeader(metadata.Pairs(serverHeaderMdKey, "I like turtles.")) //nolint: errcheck

	for i := range countListResponses {
		stream.Send(&pb.MultiPingResponse{ //nolint: errcheck
			Value:   ping.Value,
			Counter: int32(i),
			Server:  s.server,
		})
	}

	stream.SetTrailer(metadata.Pairs(serverTrailerMdKey, "I like ending turtles."))

	return nil
}

func (s *assertingMultiService) PingStream(stream pb.MultiService_PingStreamServer) error {
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

		pong := &pb.MultiPingResponse{
			Value:   ping.Value,
			Counter: counter,
			Server:  s.server,
		}

		if err := stream.Send(pong); err != nil {
			require.NoError(s.t, err, "can't fail sending back a pong")
		}

		counter++
	}

	stream.SetTrailer(metadata.Pairs(serverTrailerMdKey, "I like ending turtles."))

	return nil
}

func (s *assertingMultiService) PingStreamError(pb.MultiService_PingStreamErrorServer) error {
	return status.Errorf(codes.FailedPrecondition, "Userspace error.")
}

type assertingBackend struct {
	conn *grpc.ClientConn
	i    int
}

func (b *assertingBackend) String() string {
	return fmt.Sprintf("backend%d", b.i)
}

func (b *assertingBackend) GetConnection(ctx context.Context, _ string) (context.Context, *grpc.ClientConn, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	// Explicitly copy the metadata, otherwise the tests will fail.
	outCtx := metadata.NewOutgoingContext(ctx, md.Copy())

	if b.conn == nil {
		return ctx, nil, status.Error(codes.Unavailable, "backend connection failed")
	}

	return outCtx, b.conn, nil
}

func (b *assertingBackend) AppendInfo(streaming bool, resp []byte) ([]byte, error) {
	payload, err := proto.Marshal(&pb.ResponseMetadataPrepender{
		Metadata: &pb.ResponseMetadata{
			Hostname: fmt.Sprintf("server%d", b.i),
		},
	})

	if streaming {
		return append(resp, payload...), err
	}

	// decode protobuf embedded header
	typ, n1 := protowire.ConsumeVarint(resp)
	_, n2 := protowire.ConsumeVarint(resp[n1:]) // length

	if typ != (1<<3)|2 { // type: 2, field_number: 1
		return nil, fmt.Errorf("unexpected message format: %d", typ)
	}

	// cut off embedded message header
	resp = resp[n1+n2:]
	// build new embedded message header
	prefix := protowire.AppendVarint(protowire.AppendVarint(nil, (1<<3)|2), uint64(len(resp)+len(payload)))
	resp = append(prefix, resp...)

	return append(resp, payload...), err
}

func (b *assertingBackend) BuildError(streaming bool, err error) ([]byte, error) {
	resp := &pb.EmptyReply{
		Response: []*pb.EmptyResponse{
			{
				Metadata: &pb.ResponseMetadata{
					Hostname:      fmt.Sprintf("server%d", b.i),
					UpstreamError: err.Error(),
				},
			},
		},
	}

	if streaming {
		return proto.Marshal(resp.Response[0])
	}

	return proto.Marshal(resp)
}

type ProxyOne2ManySuite struct { //nolint: govet
	suite.Suite

	serverListeners  []net.Listener
	servers          []*grpc.Server
	proxyListener    net.Listener
	proxy            *grpc.Server
	serverClientConn *grpc.ClientConn

	client     *grpc.ClientConn
	testClient pb.MultiServiceClient

	ctx       context.Context //nolint:containedctx
	ctxCancel context.CancelFunc
}

func (s *ProxyOne2ManySuite) TestPingEmptyCarriesClientMetadata() {
	ctx := metadata.NewOutgoingContext(s.ctx, metadata.Pairs(clientMdKey, "true"))
	out, err := s.testClient.PingEmpty(ctx, &pb.Empty{})
	require.NoError(s.T(), err, "PingEmpty should succeed without errors")

	expectedUpstreams := map[string]struct{}{}
	for i := range numUpstreams {
		expectedUpstreams[fmt.Sprintf("server%d", i)] = struct{}{}
	}

	s.Require().Len(out.Response, numUpstreams)

	for _, resp := range out.Response {
		s.Require().Equal(pingDefaultValue, resp.Value)
		s.Require().EqualValues(42, resp.Counter)

		// equal metadata set by proxy and server
		s.Require().Equal(resp.Metadata.Hostname, resp.Server)

		delete(expectedUpstreams, resp.Metadata.Hostname)
	}

	s.Require().Empty(expectedUpstreams)
}

func (s *ProxyOne2ManySuite) TestPingEmpty_StressTest() {
	for range 50 {
		s.TestPingEmptyCarriesClientMetadata()
	}
}

func (s *ProxyOne2ManySuite) TestPingEmptyTargets() {
	for _, targets := range [][]string{
		{"1", "2"},
		{"3", "2", "1"},
		{"0", "4"},
		{"3"},
	} {
		md := metadata.Pairs(clientMdKey, "true")
		md.Set("targets", targets...)

		ctx := metadata.NewOutgoingContext(s.ctx, md)
		out, err := s.testClient.PingEmpty(ctx, &pb.Empty{})
		require.NoError(s.T(), err, "PingEmpty should succeed without errors")

		expectedUpstreams := map[string]struct{}{}
		for _, target := range targets {
			expectedUpstreams[fmt.Sprintf("server%s", target)] = struct{}{}
		}

		s.Require().Len(out.Response, len(expectedUpstreams))

		for _, resp := range out.Response {
			s.Require().Equal(pingDefaultValue, resp.Value)
			s.Require().EqualValues(42, resp.Counter)

			// equal metadata set by proxy and server
			s.Require().Equal(resp.Metadata.Hostname, resp.Server)

			delete(expectedUpstreams, resp.Metadata.Hostname)
		}

		s.Require().Empty(expectedUpstreams)
	}
}

func (s *ProxyOne2ManySuite) TestPingEmptyConnError() {
	targets := []string{"0", "-1", "2"}
	md := metadata.Pairs(clientMdKey, "true")
	md.Set("targets", targets...)

	ctx := metadata.NewOutgoingContext(s.ctx, md)
	out, err := s.testClient.PingEmpty(ctx, &pb.Empty{})
	require.NoError(s.T(), err, "PingEmpty should succeed without errors")

	expectedUpstreams := map[string]struct{}{}
	for _, target := range targets {
		expectedUpstreams[fmt.Sprintf("server%s", target)] = struct{}{}
	}

	s.Require().Len(out.Response, len(expectedUpstreams))

	for _, resp := range out.Response {
		delete(expectedUpstreams, resp.Metadata.Hostname)

		if resp.Metadata.Hostname != "server-1" {
			s.Assert().Equal(pingDefaultValue, resp.Value)
			s.Assert().EqualValues(42, resp.Counter)

			// equal metadata set by proxy and server
			s.Assert().Equal(resp.Metadata.Hostname, resp.Server)
		} else {
			s.Assert().Equal("rpc error: code = Unavailable desc = backend connection failed", resp.Metadata.UpstreamError)
		}
	}

	s.Require().Empty(expectedUpstreams)
}

func (s *ProxyOne2ManySuite) TestPingCarriesServerHeadersAndTrailers() {
	headerMd := make(metadata.MD)
	trailerMd := make(metadata.MD)
	// This is an awkward calling convention... but meh.
	out, err := s.testClient.Ping(s.ctx, &pb.PingRequest{Value: "foo"}, grpc.Header(&headerMd), grpc.Trailer(&trailerMd))
	require.NoError(s.T(), err, "Ping should succeed without errors")

	s.Require().Len(out.Response, numUpstreams)

	for _, resp := range out.Response {
		s.Require().Equal("foo", resp.Value)
		s.Require().EqualValues(42, resp.Counter)

		// equal metadata set by proxy and server
		s.Require().Equal(resp.Metadata.Hostname, resp.Server)
	}

	assert.Contains(s.T(), headerMd, serverHeaderMdKey, "server response headers must contain server data")
	assert.Len(s.T(), trailerMd, 1, "server response trailers must contain server data")
}

func (s *ProxyOne2ManySuite) TestPingErrorPropagatesAppError() {
	out, err := s.testClient.PingError(s.ctx, &pb.PingRequest{Value: "foo"})
	s.Require().NoError(err, "error should be encapsulated in the response")

	s.Require().Len(out.Response, numUpstreams)

	for _, resp := range out.Response {
		s.Require().NotEmpty(resp.Metadata.UpstreamError)
		s.Require().NotEmpty(resp.Metadata.Hostname)
		s.Assert().Equal("rpc error: code = FailedPrecondition desc = Userspace error.", resp.Metadata.UpstreamError)
	}
}

func (s *ProxyOne2ManySuite) TestPingStreamErrorPropagatesAppError() {
	stream, err := s.testClient.PingStreamError(s.ctx)
	s.Require().NoError(err, "error should be encapsulated in the response")

	for range numUpstreams {
		var resp *pb.MultiPingResponse

		resp, err = stream.Recv()
		s.Require().NoError(err)

		s.Assert().Equal("rpc error: code = FailedPrecondition desc = Userspace error.", resp.Metadata.UpstreamError)
	}

	require.NoError(s.T(), stream.CloseSend(), "no error on close send")
	_, err = stream.Recv()
	require.Equal(s.T(), io.EOF, err, "stream should close with io.EOF, meaning OK")
}

func (s *ProxyOne2ManySuite) TestPingStreamConnError() {
	targets := []string{"0", "-1", "2"}
	md := metadata.Pairs(clientMdKey, "true")
	md.Set("targets", targets...)

	ctx := metadata.NewOutgoingContext(s.ctx, md)
	stream, err := s.testClient.PingStream(ctx)
	s.Require().NoError(err, "error should be encapsulated in the response")

	require.NoError(s.T(), stream.CloseSend(), "no error on close send")

	resp, err := stream.Recv()
	s.Require().NoError(err)

	s.Assert().Equal("rpc error: code = Unavailable desc = backend connection failed", resp.Metadata.UpstreamError)

	_, err = stream.Recv()
	require.Equal(s.T(), io.EOF, err, "stream should close with io.EOF, meaning OK")
}

func (s *ProxyOne2ManySuite) TestDirectorErrorIsPropagated() {
	// See SetupSuite where the StreamDirector has a special case.
	ctx := metadata.NewOutgoingContext(s.ctx, metadata.Pairs(rejectingMdKey, "true"))
	_, err := s.testClient.Ping(ctx, &pb.PingRequest{Value: "foo"})
	require.Error(s.T(), err, "Director should reject this RPC")
	assert.Equal(s.T(), codes.PermissionDenied, status.Code(err))
	assert.Equal(s.T(), "testing rejection", status.Convert(err).Message())
}

func (s *ProxyOne2ManySuite) TestPingStream_FullDuplexWorks() {
	stream, err := s.testClient.PingStream(s.ctx)
	require.NoError(s.T(), err, "PingStream request should be successful.")

	for i := range countListResponses {
		ping := &pb.PingRequest{Value: fmt.Sprintf("foo:%d", i)}
		require.NoError(s.T(), stream.Send(ping), "sending to PingStream must not fail")

		expectedUpstreams := map[string]struct{}{}
		for j := range numUpstreams {
			expectedUpstreams[fmt.Sprintf("server%d", j)] = struct{}{}
		}

		// each upstream should send back response
		for range numUpstreams {
			var resp *pb.MultiPingResponse
			resp, err = stream.Recv()
			s.Require().NoError(err)

			s.Assert().EqualValues(i, resp.Counter, "ping roundtrip must succeed with the correct id")
			s.Assert().EqualValues(resp.Metadata.Hostname, resp.Server)

			delete(expectedUpstreams, resp.Metadata.Hostname)
		}

		s.Require().Empty(expectedUpstreams)

		if i == 0 {
			// Check that the header arrives before all entries.
			var headerMd metadata.MD
			headerMd, err = stream.Header()
			require.NoError(s.T(), err, "PingStream headers should not error.")
			assert.Contains(s.T(), headerMd, serverHeaderMdKey, "PingStream response headers user contain metadata")
		}
	}

	require.NoError(s.T(), stream.CloseSend(), "no error on close send")
	_, err = stream.Recv()
	require.Equal(s.T(), io.EOF, err, "stream should close with io.EOF, meaning OK")
	// Check that the trailer headers are here.
	trailerMd := stream.Trailer()
	assert.Len(s.T(), trailerMd, 1, "PingList trailer headers user contain metadata")
}

//nolint:gocognit
func (s *ProxyOne2ManySuite) TestPingStream_FullDuplexConcurrent() {
	stream, err := s.testClient.PingStream(s.ctx)
	require.NoError(s.T(), err, "PingStream request should be successful.")

	// send countListResponses requests and concurrently read numUpstreams * countListResponses replies
	errCh := make(chan error, 2)

	expectedUpstreams := map[string]int32{}

	for j := range numUpstreams {
		expectedUpstreams[fmt.Sprintf("server%d", j)] = 0
	}

	go func() {
		errCh <- func() error {
			for i := range countListResponses {
				ping := &pb.PingRequest{Value: fmt.Sprintf("foo:%d", i)}
				if err = stream.Send(ping); err != nil {
					return err
				}
			}

			return stream.CloseSend()
		}()
	}()

	go func() {
		errCh <- func() error {
			for range countListResponses * numUpstreams {
				var resp *pb.MultiPingResponse

				resp, err = stream.Recv()
				if err != nil {
					return err
				}

				if resp.Metadata == nil {
					return fmt.Errorf("response metadata expected: %v", resp)
				}

				if resp.Metadata.Hostname != resp.Server {
					return fmt.Errorf("mismatch on host metadata: %v != %v", resp.Metadata.Hostname, resp.Server)
				}

				expectedCounter, ok := expectedUpstreams[resp.Server]
				if !ok {
					return fmt.Errorf("unexpected host: %v", resp.Server)
				}

				if expectedCounter != resp.Counter {
					return fmt.Errorf("unexpected counter value: %d != %d", expectedCounter, resp.Counter)
				}

				expectedUpstreams[resp.Server]++
			}

			return nil
		}()
	}()

	s.Require().NoError(<-errCh)
	s.Require().NoError(<-errCh)

	_, err = stream.Recv()
	require.Equal(s.T(), io.EOF, err, "stream should close with io.EOF, meaning OK")
	// Check that the trailer headers are here.
	trailerMd := stream.Trailer()
	assert.Len(s.T(), trailerMd, 1, "PingList trailer headers user contain metadata")
}

func (s *ProxyOne2ManySuite) TestPingStream_StressTest() {
	for range 50 {
		s.TestPingStream_FullDuplexWorks()
	}
}

func (s *ProxyOne2ManySuite) SetupTest() {
	s.ctx, s.ctxCancel = context.WithTimeout(context.TODO(), 120*time.Second)
}

func (s *ProxyOne2ManySuite) TearDownTest() {
	s.ctxCancel()
}

//nolint:gocognit
func (s *ProxyOne2ManySuite) SetupSuite() {
	var err error

	s.proxyListener, err = net.Listen("tcp", "127.0.0.1:0")
	require.NoError(s.T(), err, "must be able to allocate a port for proxyListener")

	s.serverListeners = make([]net.Listener, numUpstreams)

	for i := range s.serverListeners {
		s.serverListeners[i], err = net.Listen("tcp", "127.0.0.1:0")
		require.NoError(s.T(), err, "must be able to allocate a port for serverListener")
	}

	s.servers = make([]*grpc.Server, numUpstreams)

	for i := range s.servers {
		s.servers[i] = grpc.NewServer()
		pb.RegisterMultiServiceServer(s.servers[i],
			&assertingMultiService{
				t:      s.T(),
				server: fmt.Sprintf("server%d", i),
			})
	}

	backends := make([]*assertingBackend, numUpstreams)

	for i := range backends {
		var conn *grpc.ClientConn
		conn, err = grpc.NewClient(
			s.serverListeners[i].Addr().String(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.ForceCodecV2(proxy.Codec())),
		)
		require.NoError(s.T(), err)

		backends[i] = &assertingBackend{
			conn: conn,
			i:    i,
		}
	}

	failingBackend := &assertingBackend{
		conn: nil,
		i:    -1,
	}

	// Setup of the proxy's Director.
	director := func(ctx context.Context, _ string) (proxy.Mode, []proxy.Backend, error) {
		var targets []int

		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			if _, exists := md[rejectingMdKey]; exists {
				return proxy.One2Many, nil, status.Errorf(codes.PermissionDenied, "testing rejection")
			}

			if mdTargets, exists := md["targets"]; exists {
				for _, strTarget := range mdTargets {
					var t int

					t, err = strconv.Atoi(strTarget)
					if err != nil {
						return proxy.One2Many, nil, err
					}

					targets = append(targets, t)
				}
			}
		}

		var result []proxy.Backend

		if targets == nil {
			for i := range backends {
				targets = append(targets, i)
			}
		}

		for _, t := range targets {
			if t == -1 {
				result = append(result, failingBackend)
			} else {
				result = append(result, backends[t])
			}
		}

		return proxy.One2Many, result, nil
	}

	s.proxy = grpc.NewServer(
		grpc.ForceServerCodecV2(proxy.Codec()),
		grpc.UnknownServiceHandler(proxy.TransparentHandler(director)),
	)
	// Ping handler is handled as an explicit registration and not as a TransparentHandler.
	proxy.RegisterService(s.proxy, director,
		"talos.testproto.MultiService",
		proxy.WithMethodNames("Ping", "PingStream", "PingStreamError"),
		proxy.WithStreamedMethodNames("PingStream", "PingStreamError"),
	)

	// Start the serving loops.
	for i := range s.servers {
		s.T().Logf("starting grpc.Server at: %v", s.serverListeners[i].Addr().String())

		go func(i int) {
			s.servers[i].Serve(s.serverListeners[i]) //nolint: errcheck
		}(i)
	}

	s.T().Logf("starting grpc.Proxy at: %v", s.proxyListener.Addr().String())

	go func() {
		s.proxy.Serve(s.proxyListener) //nolint: errcheck
	}()

	clientConn, err := grpc.NewClient(strings.Replace(s.proxyListener.Addr().String(), "127.0.0.1", "localhost", 1), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(s.T(), err, "must not error on deferred client Dial")
	s.testClient = pb.NewMultiServiceClient(clientConn)
}

func (s *ProxyOne2ManySuite) TearDownSuite() {
	if s.client != nil {
		s.Assert().NoError(s.client.Close())
	}

	if s.serverClientConn != nil {
		s.Assert().NoError(s.serverClientConn.Close())
	}

	// Close all transports so the logs don't get spammy.
	time.Sleep(10 * time.Millisecond)

	if s.proxy != nil {
		s.proxy.Stop()
		s.proxyListener.Close() //nolint: errcheck
	}

	for _, server := range s.servers {
		if server != nil {
			server.Stop()
		}
	}

	for _, serverListener := range s.serverListeners {
		if serverListener != nil {
			serverListener.Close() //nolint: errcheck
		}
	}
}

func TestProxyOne2ManySuite(t *testing.T) {
	suite.Run(t, &ProxyOne2ManySuite{})
}

func init() {
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(os.Stderr, os.Stderr, os.Stderr))
}

// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jaegerreceiver

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"testing"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	collectorSampling "github.com/jaegertracing/jaeger/cmd/collector/app/sampling"
	"github.com/jaegertracing/jaeger/model"
	staticStrategyStore "github.com/jaegertracing/jaeger/plugin/sampling/strategystore/static"
	"github.com/jaegertracing/jaeger/proto-gen/api_v2"
	jaegerthrift "github.com/jaegertracing/jaeger/thrift-gen/jaeger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/model/pdata"
	conventions "go.opentelemetry.io/collector/model/semconv/v1.5.0"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal/testutil"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/translator/jaeger"
)

var jaegerReceiver = config.NewComponentIDWithName("jaeger", "receiver_test")

func TestTraceSource(t *testing.T) {
	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerReceiver, &configuration{}, nil, set)
	require.NotNil(t, jr)
}

func jaegerBatchToHTTPBody(b *jaegerthrift.Batch) (*http.Request, error) {
	body, err := thrift.NewTSerializer().Write(context.Background(), b)
	if err != nil {
		return nil, err
	}
	r := httptest.NewRequest("POST", "/api/traces", bytes.NewReader(body))
	r.Header.Add("content-type", "application/x-thrift")
	return r, nil
}

func TestThriftHTTPBodyDecode(t *testing.T) {
	jr := jReceiver{}
	batch := &jaegerthrift.Batch{
		Process: jaegerthrift.NewProcess(),
		Spans:   []*jaegerthrift.Span{jaegerthrift.NewSpan()},
	}
	r, err := jaegerBatchToHTTPBody(batch)
	require.NoError(t, err, "failed to prepare http body")

	gotBatch, hErr := jr.decodeThriftHTTPBody(r)
	require.Nil(t, hErr, "failed to decode http body")
	assert.Equal(t, batch, gotBatch)
}

func TestReception(t *testing.T) {
	port := testutil.GetAvailablePort(t)
	// 1. Create the Jaeger receiver aka "server"
	config := &configuration{
		CollectorHTTPPort: int(port),
		CollectorHTTPSettings: confighttp.HTTPServerSettings{
			Endpoint: fmt.Sprintf(":%d", port),
		},
	}
	sink := new(consumertest.TracesSink)

	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerReceiver, config, sink, set)

	require.NoError(t, jr.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })

	// 2. Then send spans to the Jaeger receiver.
	collectorAddr := fmt.Sprintf("http://localhost:%d/api/traces", port)
	td := generateTraceData()
	batches, err := jaeger.InternalTracesToJaegerProto(td)
	require.NoError(t, err)
	for _, batch := range batches {
		require.NoError(t, sendToCollector(collectorAddr, modelToThrift(batch)))
	}

	assert.NoError(t, err, "should not have failed to create the Jaeger OpenCensus exporter")

	gotTraces := sink.AllTraces()
	assert.Equal(t, 1, len(gotTraces))

	assert.EqualValues(t, td, gotTraces[0])
}

func TestPortsNotOpen(t *testing.T) {
	// an empty config should result in no open ports
	config := &configuration{}

	sink := new(consumertest.TracesSink)

	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerReceiver, config, sink, set)

	require.NoError(t, jr.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })

	// there is a race condition here that we're ignoring.
	//  this test may occasionally pass incorrectly, but it will not fail incorrectly
	//  TODO: consider adding a way for a receiver to asynchronously signal that is ready to receive spans to eliminate races/arbitrary waits
	l, err := net.Listen("tcp", "localhost:14250")
	assert.NoError(t, err, "should have been able to listen on 14250.  jaeger receiver incorrectly started grpc")
	if l != nil {
		l.Close()
	}

	l, err = net.Listen("tcp", "localhost:14268")
	assert.NoError(t, err, "should have been able to listen on 14268.  jaeger receiver incorrectly started thrift_http")
	if l != nil {
		l.Close()
	}
}

func TestGRPCReception(t *testing.T) {
	// prepare
	config := &configuration{
		CollectorGRPCPort: 14250, // that's the only one used by this test
	}
	sink := new(consumertest.TracesSink)

	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerReceiver, config, sink, set)

	require.NoError(t, jr.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })

	conn, err := grpc.Dial(fmt.Sprintf("0.0.0.0:%d", config.CollectorGRPCPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	cl := api_v2.NewCollectorServiceClient(conn)

	now := time.Unix(1542158650, 536343000).UTC()
	d10min := 10 * time.Minute
	d2sec := 2 * time.Second
	nowPlus10min := now.Add(d10min)
	nowPlus10min2sec := now.Add(d10min).Add(d2sec)

	// test
	req := grpcFixture(now, d10min, d2sec)
	resp, err := cl.PostSpans(context.Background(), req, grpc.WaitForReady(true))

	// verify
	assert.NoError(t, err, "should not have failed to post spans")
	assert.NotNil(t, resp, "response should not have been nil")

	gotTraces := sink.AllTraces()
	assert.Equal(t, 1, len(gotTraces))
	want := expectedTraceData(now, nowPlus10min, nowPlus10min2sec)

	assert.Len(t, req.Batch.Spans, want.SpanCount(), "got a conflicting amount of spans")

	assert.EqualValues(t, want, gotTraces[0])
}

func TestGRPCReceptionWithTLS(t *testing.T) {
	// prepare
	tlsCreds := &configtls.TLSServerSetting{
		TLSSetting: configtls.TLSSetting{
			CertFile: path.Join(".", "testdata", "server.crt"),
			KeyFile:  path.Join(".", "testdata", "server.key"),
		},
	}

	grpcServerSettings := configgrpc.GRPCServerSettings{
		TLSSetting: tlsCreds,
	}

	port := testutil.GetAvailablePort(t)
	config := &configuration{
		CollectorGRPCPort:           int(port),
		CollectorGRPCServerSettings: grpcServerSettings,
	}
	sink := new(consumertest.TracesSink)

	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerReceiver, config, sink, set)

	require.NoError(t, jr.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })

	creds, err := credentials.NewClientTLSFromFile(path.Join(".", "testdata", "server.crt"), "localhost")
	require.NoError(t, err)
	conn, err := grpc.Dial(jr.collectorGRPCAddr(), grpc.WithTransportCredentials(creds))
	require.NoError(t, err)
	defer conn.Close()

	cl := api_v2.NewCollectorServiceClient(conn)

	now := time.Now()
	d10min := 10 * time.Minute
	d2sec := 2 * time.Second
	nowPlus10min := now.Add(d10min)
	nowPlus10min2sec := now.Add(d10min).Add(d2sec)

	// test
	req := grpcFixture(now, d10min, d2sec)
	resp, err := cl.PostSpans(context.Background(), req, grpc.WaitForReady(true))

	// verify
	assert.NoError(t, err, "should not have failed to post spans")
	assert.NotNil(t, resp, "response should not have been nil")

	gotTraces := sink.AllTraces()
	assert.Equal(t, 1, len(gotTraces))
	want := expectedTraceData(now, nowPlus10min, nowPlus10min2sec)

	assert.Len(t, req.Batch.Spans, want.SpanCount(), "got a conflicting amount of spans")
	assert.EqualValues(t, want, gotTraces[0])
}

func expectedTraceData(t1, t2, t3 time.Time) pdata.Traces {
	traceID := pdata.NewTraceID(
		[16]byte{0xF1, 0xF2, 0xF3, 0xF4, 0xF5, 0xF6, 0xF7, 0xF8, 0xF9, 0xFA, 0xFB, 0xFC, 0xFD, 0xFE, 0xFF, 0x80})
	parentSpanID := pdata.NewSpanID([8]byte{0x1F, 0x1E, 0x1D, 0x1C, 0x1B, 0x1A, 0x19, 0x18})
	childSpanID := pdata.NewSpanID([8]byte{0xAF, 0xAE, 0xAD, 0xAC, 0xAB, 0xAA, 0xA9, 0xA8})

	traces := pdata.NewTraces()
	rs := traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().InsertString(conventions.AttributeServiceName, "issaTest")
	rs.Resource().Attributes().InsertBool("bool", true)
	rs.Resource().Attributes().InsertString("string", "yes")
	rs.Resource().Attributes().InsertInt("int64", 10000000)
	spans := rs.InstrumentationLibrarySpans().AppendEmpty().Spans()

	span0 := spans.AppendEmpty()
	span0.SetSpanID(childSpanID)
	span0.SetParentSpanID(parentSpanID)
	span0.SetTraceID(traceID)
	span0.SetName("DBSearch")
	span0.SetStartTimestamp(pdata.NewTimestampFromTime(t1))
	span0.SetEndTimestamp(pdata.NewTimestampFromTime(t2))
	span0.Status().SetCode(pdata.StatusCodeError)
	span0.Status().SetMessage("Stale indices")

	span1 := spans.AppendEmpty()
	span1.SetSpanID(parentSpanID)
	span1.SetTraceID(traceID)
	span1.SetName("ProxyFetch")
	span1.SetStartTimestamp(pdata.NewTimestampFromTime(t2))
	span1.SetEndTimestamp(pdata.NewTimestampFromTime(t3))
	span1.Status().SetCode(pdata.StatusCodeError)
	span1.Status().SetMessage("Frontend crash")

	return traces
}

func grpcFixture(t1 time.Time, d1, d2 time.Duration) *api_v2.PostSpansRequest {
	traceID := model.TraceID{}
	traceID.Unmarshal([]byte{0xF1, 0xF2, 0xF3, 0xF4, 0xF5, 0xF6, 0xF7, 0xF8, 0xF9, 0xFA, 0xFB, 0xFC, 0xFD, 0xFE, 0xFF, 0x80}) // nolint:errcheck
	parentSpanID := model.NewSpanID(binary.BigEndian.Uint64([]byte{0x1F, 0x1E, 0x1D, 0x1C, 0x1B, 0x1A, 0x19, 0x18}))
	childSpanID := model.NewSpanID(binary.BigEndian.Uint64([]byte{0xAF, 0xAE, 0xAD, 0xAC, 0xAB, 0xAA, 0xA9, 0xA8}))

	return &api_v2.PostSpansRequest{
		Batch: model.Batch{
			Process: &model.Process{
				ServiceName: "issaTest",
				Tags: []model.KeyValue{
					model.Bool("bool", true),
					model.String("string", "yes"),
					model.Int64("int64", 1e7),
				},
			},
			Spans: []*model.Span{
				{
					TraceID:       traceID,
					SpanID:        childSpanID,
					OperationName: "DBSearch",
					StartTime:     t1,
					Duration:      d1,
					Tags: []model.KeyValue{
						model.String(conventions.OtelStatusDescription, "Stale indices"),
						model.Int64(conventions.OtelStatusCode, int64(pdata.StatusCodeError)),
						model.Bool("error", true),
					},
					References: []model.SpanRef{
						{
							TraceID: traceID,
							SpanID:  parentSpanID,
							RefType: model.SpanRefType_CHILD_OF,
						},
					},
				},
				{
					TraceID:       traceID,
					SpanID:        parentSpanID,
					OperationName: "ProxyFetch",
					StartTime:     t1.Add(d1),
					Duration:      d2,
					Tags: []model.KeyValue{
						model.String(conventions.OtelStatusDescription, "Frontend crash"),
						model.Int64(conventions.OtelStatusCode, int64(pdata.StatusCodeError)),
						model.Bool("error", true),
					},
				},
			},
		},
	}
}

func TestSampling(t *testing.T) {
	port := testutil.GetAvailablePort(t)
	config := &configuration{
		CollectorGRPCPort:          int(port),
		RemoteSamplingStrategyFile: "testdata/strategies.json",
	}
	sink := new(consumertest.TracesSink)

	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerReceiver, config, sink, set)

	require.NoError(t, jr.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })

	conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", config.CollectorGRPCPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NoError(t, err)
	defer conn.Close()

	cl := api_v2.NewSamplingManagerClient(conn)

	expected := &api_v2.SamplingStrategyResponse{
		StrategyType: api_v2.SamplingStrategyType_PROBABILISTIC,
		ProbabilisticSampling: &api_v2.ProbabilisticSamplingStrategy{
			SamplingRate: 0.8,
		},
		OperationSampling: &api_v2.PerOperationSamplingStrategies{
			DefaultSamplingProbability: 0.8,
			PerOperationStrategies: []*api_v2.OperationSamplingStrategy{
				{
					Operation: "op1",
					ProbabilisticSampling: &api_v2.ProbabilisticSamplingStrategy{
						SamplingRate: 0.2,
					},
				},
				{
					Operation: "op2",
					ProbabilisticSampling: &api_v2.ProbabilisticSamplingStrategy{
						SamplingRate: 0.4,
					},
				},
			},
		},
	}

	resp, err := cl.GetSamplingStrategy(context.Background(), &api_v2.SamplingStrategyParameters{
		ServiceName: "foo",
	})
	assert.NoError(t, err)
	assert.Equal(t, expected, resp)
}

func TestSamplingFailsOnNotConfigured(t *testing.T) {
	port := testutil.GetAvailablePort(t)
	// prepare
	config := &configuration{
		CollectorGRPCPort: int(port),
	}
	sink := new(consumertest.TracesSink)

	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerReceiver, config, sink, set)

	require.NoError(t, jr.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })

	conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", config.CollectorGRPCPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NoError(t, err)
	defer conn.Close()

	cl := api_v2.NewSamplingManagerClient(conn)

	response, err := cl.GetSamplingStrategy(context.Background(), &api_v2.SamplingStrategyParameters{
		ServiceName: "nothing",
	})
	require.NoError(t, err)
	assert.Equal(t, 0.001, response.GetProbabilisticSampling().GetSamplingRate())
}

func TestSamplingFailsOnBadFile(t *testing.T) {
	port := testutil.GetAvailablePort(t)
	// prepare
	config := &configuration{
		CollectorGRPCPort:          int(port),
		RemoteSamplingStrategyFile: "does-not-exist",
	}
	sink := new(consumertest.TracesSink)

	set := componenttest.NewNopReceiverCreateSettings()
	jr := newJaegerReceiver(jaegerReceiver, config, sink, set)
	assert.Error(t, jr.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, jr.Shutdown(context.Background())) })
}

func TestSamplingStrategiesMutualTLS(t *testing.T) {
	caPath := path.Join(".", "testdata", "ca.crt")
	serverCertPath := path.Join(".", "testdata", "server.crt")
	serverKeyPath := path.Join(".", "testdata", "server.key")
	clientCertPath := path.Join(".", "testdata", "client.crt")
	clientKeyPath := path.Join(".", "testdata", "client.key")

	// start gRPC server that serves sampling strategies
	tlsCfgOpts := configtls.TLSServerSetting{
		TLSSetting: configtls.TLSSetting{
			CAFile:   caPath,
			CertFile: serverCertPath,
			KeyFile:  serverKeyPath,
		},
	}
	tlsCfg, err := tlsCfgOpts.LoadTLSConfig()
	require.NoError(t, err)
	server, serverAddr := initializeGRPCTestServer(t, func(s *grpc.Server) {
		ss, serr := staticStrategyStore.NewStrategyStore(staticStrategyStore.Options{
			StrategiesFile: path.Join(".", "testdata", "strategies.json"),
		}, zap.NewNop())
		require.NoError(t, serr)
		api_v2.RegisterSamplingManagerServer(s, collectorSampling.NewGRPCHandler(ss))
	}, grpc.Creds(credentials.NewTLS(tlsCfg)))
	defer server.GracefulStop()

	// Create sampling strategies receiver
	port := testutil.GetAvailablePort(t)
	require.NoError(t, err)
	hostEndpoint := fmt.Sprintf("localhost:%d", port)
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.RemoteSampling = &RemoteSamplingConfig{
		GRPCClientSettings: configgrpc.GRPCClientSettings{
			TLSSetting: configtls.TLSClientSetting{
				TLSSetting: configtls.TLSSetting{
					CAFile:   caPath,
					CertFile: clientCertPath,
					KeyFile:  clientKeyPath,
				},
				Insecure:   false,
				ServerName: "localhost",
			},
			Endpoint: serverAddr.String(),
		},
		HostEndpoint: hostEndpoint,
	}
	// at least one protocol has to be enabled
	thriftHTTPPort := testutil.GetAvailablePort(t)
	require.NoError(t, err)
	cfg.Protocols.ThriftHTTP = &confighttp.HTTPServerSettings{
		Endpoint: fmt.Sprintf("localhost:%d", thriftHTTPPort),
	}
	exp, err := factory.CreateTracesReceiver(context.Background(), componenttest.NewNopReceiverCreateSettings(), cfg, consumertest.NewNop())
	require.NoError(t, err)
	require.NoError(t, exp.Start(context.Background(), newAssertNoErrorHost(t)))
	t.Cleanup(func() { require.NoError(t, exp.Shutdown(context.Background())) })
	<-time.After(200 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://%s?service=bar", hostEndpoint))
	require.NoError(t, err)
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, "{\"strategyType\":1,\"rateLimitingSampling\":{\"maxTracesPerSecond\":5}}", string(bodyBytes))
}

func TestConsumeThriftTrace(t *testing.T) {
	tests := []struct {
		batch    *jaegerthrift.Batch
		numSpans int
	}{
		{
			batch: nil,
		},
		{
			batch:    &jaegerthrift.Batch{Spans: []*jaegerthrift.Span{{}}},
			numSpans: 1,
		},
	}
	for _, test := range tests {
		numSpans, err := consumeTraces(context.Background(), test.batch, consumertest.NewNop())
		require.NoError(t, err)
		assert.Equal(t, test.numSpans, numSpans)
	}
}

func sendToCollector(endpoint string, batch *jaegerthrift.Batch) error {
	buf, err := thrift.NewTSerializer().Write(context.Background(), batch)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-thrift")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	_, err = io.Copy(ioutil.Discard, resp.Body)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("failed to upload traces; HTTP status code: %d", resp.StatusCode)
	}
	return nil
}

// assertNoErrorHost implements a component.Host that asserts that there were no errors.
type assertNoErrorHost struct {
	component.Host
	*testing.T
}

// newAssertNoErrorHost returns a new instance of assertNoErrorHost.
func newAssertNoErrorHost(t *testing.T) component.Host {
	return &assertNoErrorHost{
		Host: componenttest.NewNopHost(),
		T:    t,
	}
}

func (aneh *assertNoErrorHost) ReportFatalError(err error) {
	assert.NoError(aneh, err)
}

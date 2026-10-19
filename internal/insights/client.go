package insights

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	insightsv1 "github.com/earlgrey/piproxy/internal/insightsrpc/v1"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

const maxGRPCMessageBytes = 16 << 20

// ClientPool keeps HTTP/2 connections open across nightly runs. Configuration
// changes naturally create a new keyed connection; Close releases all of them.
type ClientPool struct {
	mu          sync.Mutex
	connections map[string]*grpc.ClientConn
}

func NewClientPool() *ClientPool { return &ClientPool{connections: make(map[string]*grpc.ClientConn)} }

func (p *ClientPool) Generate(ctx context.Context, processor *pgstore.InsightProcessorRecord, req *insightsv1.GenerateInsightsRequest) (*insightsv1.GenerateInsightsResponse, error) {
	conn, err := p.connection(processor)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(processor.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := insightsv1.NewInsightsServiceClient(conn).GenerateInsights(callCtx, req, grpc.WaitForReady(true))
	if err != nil {
		return nil, fmt.Errorf("generate insights via %s: %w", processor.GRPCTarget, err)
	}
	return response, nil
}

func (p *ClientPool) Health(ctx context.Context, processor *pgstore.InsightProcessorRecord) (*insightsv1.HealthResponse, error) {
	conn, err := p.connection(processor)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(processor.TimeoutMS) * time.Millisecond
	if timeout <= 0 || timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := insightsv1.NewInsightsServiceClient(conn).Health(callCtx, &emptypb.Empty{}, grpc.WaitForReady(true))
	if err != nil {
		return nil, fmt.Errorf("check insight processor %s: %w", processor.GRPCTarget, err)
	}
	return response, nil
}

func (p *ClientPool) connection(processor *pgstore.InsightProcessorRecord) (*grpc.ClientConn, error) {
	if processor == nil || strings.TrimSpace(processor.GRPCTarget) == "" {
		return nil, fmt.Errorf("insight processor target is required")
	}
	key := fmt.Sprintf("%s|%t|%s", processor.GRPCTarget, processor.Plaintext, processor.TLSServerName)
	p.mu.Lock()
	defer p.mu.Unlock()
	if conn := p.connections[key]; conn != nil {
		return conn, nil
	}
	var transport credentials.TransportCredentials
	if processor.Plaintext {
		transport = insecure.NewCredentials()
	} else {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.TrimSpace(processor.TLSServerName)})
	}
	conn, err := grpc.NewClient(strings.TrimSpace(processor.GRPCTarget),
		grpc.WithTransportCredentials(transport),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxGRPCMessageBytes), grpc.MaxCallSendMsgSize(maxGRPCMessageBytes)),
	)
	if err != nil {
		return nil, fmt.Errorf("create insight processor client: %w", err)
	}
	p.connections[key] = conn
	return conn, nil
}

func (p *ClientPool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var first error
	for key, conn := range p.connections {
		if err := conn.Close(); err != nil && first == nil {
			first = err
		}
		delete(p.connections, key)
	}
	return first
}

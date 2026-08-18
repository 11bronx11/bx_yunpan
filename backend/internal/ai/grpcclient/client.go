// Package grpcclient 是 API 侧调用 aisvc 的客户端，实现 ai.Backend。
package grpcclient

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/pb"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/breaker"
)

// Config 描述客户端的连接、超时与各接口重试策略。
type Config struct {
	// Target 是 gRPC dial 目标，可以是 host:port，也可以是 etcd resolver 的
	// scheme URL（T3 起用 yunpan-etcd:///services/aisvc）。
	Target string
	// CallTimeout 是单次调用的总预算，经 gRPC deadline 传播到 aisvc 与上游 LLM。
	// 三层 deadline 必须收敛：下游不允许长于上游。
	CallTimeout    time.Duration
	SearchRetry    RetryPolicy
	AskRetry       RetryPolicy
	ReprocessRetry RetryPolicy
	Breaker        breaker.Config
	DialOptions    []grpc.DialOption
}

// Client 实现 ai.Backend，把 API 侧的调用转成 gRPC 请求。
type Client struct {
	conn    *grpc.ClientConn
	client  pb.AIServiceClient
	config  Config
	circuit *breaker.Breaker
}

var _ ai.Backend = (*Client)(nil)

func New(config Config) (*Client, error) {
	options := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// 客户端侧负载均衡：配合 T3 的 etcd resolver 在多副本间轮询。
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
	}, config.DialOptions...)

	conn, err := grpc.NewClient(config.Target, options...)
	if err != nil {
		return nil, fmt.Errorf("dial aisvc: %w", err)
	}
	return &Client{
		conn:    conn,
		client:  pb.NewAIServiceClient(conn),
		config:  config,
		circuit: breaker.New(config.Breaker),
	}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Search 可重试：只读且幂等。但 server streaming 一旦开始把消息交给调用方
// 就不能重来，因此只在"尚未收到任何消息"时才允许重试。
func (c *Client) Search(ctx context.Context, ownerID uuid.UUID, input ai.SearchInput) ([]ai.SearchHit, error) {
	request := &pb.SearchRequest{
		OwnerId:           ownerID.String(),
		Query:             input.Query,
		Mode:              input.Mode,
		IncludeSubfolders: input.IncludeSubfolders,
		MimeTypes:         input.MimeTypes,
		Limit:             int32(input.Limit),
	}
	if input.FolderID != nil {
		folderID := input.FolderID.String()
		request.FolderId = &folderID
	}

	var hits []ai.SearchHit
	err := c.call(ctx, c.config.SearchRetry, func(callCtx context.Context) (bool, error) {
		hits = nil
		received := false
		stream, err := c.client.Search(callCtx, request)
		if err != nil {
			return true, err
		}
		for {
			hit, err := stream.Recv()
			if err != nil {
				if isStreamEnd(err) {
					return false, nil
				}
				// 已经收过消息就不能重试，否则调用方会看到重复或拼接的结果。
				return !received, err
			}
			received = true
			hits = append(hits, hitFromProto(hit))
		}
	})
	if err != nil {
		return nil, ai.FromGRPCError(err)
	}
	if hits == nil {
		hits = []ai.SearchHit{}
	}
	return hits, nil
}

// Ask 不重试：LLM 调用非幂等且有成本，超时重试可能重复计费并返回两份不同答案。
func (c *Client) Ask(ctx context.Context, ownerID uuid.UUID, question string, folderID *uuid.UUID, fileIDs []uuid.UUID) (string, []ai.Citation, error) {
	request := &pb.AskRequest{OwnerId: ownerID.String(), Question: question}
	if folderID != nil {
		value := folderID.String()
		request.FolderId = &value
	}
	for _, fileID := range fileIDs {
		request.FileIds = append(request.FileIds, fileID.String())
	}

	var response *pb.AskResponse
	err := c.call(ctx, c.config.AskRetry, func(callCtx context.Context) (bool, error) {
		var err error
		response, err = c.client.Ask(callCtx, request)
		return false, err
	})
	if err != nil {
		return "", nil, ai.FromGRPCError(err)
	}
	return response.GetAnswer(), citationsFromProto(response.GetCitations()), nil
}

// RequestReprocess 可重试：既有 Pipeline/Model Version 与 dedupe_key 保证幂等。
func (c *Client) RequestReprocess(ctx context.Context, ownerID, fileID uuid.UUID, idempotencyKey string) (ai.Task, error) {
	request := &pb.ReprocessRequest{
		OwnerId:        ownerID.String(),
		FileId:         fileID.String(),
		IdempotencyKey: idempotencyKey,
	}
	var response *pb.ReprocessResponse
	err := c.call(ctx, c.config.ReprocessRetry, func(callCtx context.Context) (bool, error) {
		var err error
		response, err = c.client.Reprocess(callCtx, request)
		return true, err
	})
	if err != nil {
		return ai.Task{}, ai.FromGRPCError(err)
	}
	return taskFromProto(response.GetTask()), nil
}

// call 统一套上总预算 deadline、熔断与重试。deadline 设在整个重试序列之外，
// 保证"下游超时不长于上游"：预算耗尽后不会再发起新的尝试。
func (c *Client) call(ctx context.Context, policy RetryPolicy, attempt func(context.Context) (bool, error)) error {
	if c.config.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.config.CallTimeout)
		defer cancel()
	}
	return call(ctx, c.circuit, policy, attempt)
}

package ai

import (
	"context"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/11bronx11/bx_yunpan/backend/internal/platform/tracing"
)

// startSpan 开一个业务 span。未初始化 TracerProvider 时拿到的是 no-op span，
// 调用侧不必判空,也几乎没有开销。
func startSpan(ctx context.Context, name string, attributes ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracing.Tracer().Start(ctx, name, trace.WithAttributes(attributes...))
}

// endSpan 统一收尾：失败的 span 必须 RecordError + SetStatus(Error)，
// 否则 Jaeger 上看不出这一段是错的，排查时会漏掉真正的失败点。
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// extractSections 是 extract 的埋点包装：mime 与体积决定了这段耗时，
// 排查"某类文件特别慢"时先看这两个属性。
func (s *Service) extractSections(ctx context.Context, mimeType string, sizeBytes int64, data []byte) ([]extractedSection, error) {
	ctx, span := startSpan(ctx, "ai.extract",
		attribute.String("ai.mime_type", mimeType),
		attribute.Int64("ai.object_size_bytes", sizeBytes),
	)
	sections, err := extract(ctx, s.provider, mimeType, data)
	if err == nil {
		span.SetAttributes(attribute.Int("ai.section_count", len(sections)))
	}
	endSpan(span, err)
	return sections, err
}

// makeChunksTraced 记录切块结果：块数直接决定后面 embed 的批次数与成本。
func (s *Service) makeChunksTraced(ctx context.Context, sections []extractedSection) []chunkInput {
	_, span := startSpan(ctx, "ai.chunk", attribute.Int("ai.section_count", len(sections)))
	chunks := makeChunks(sections)
	span.SetAttributes(attribute.Int("ai.chunk_count", len(chunks)))
	span.End()
	return chunks
}

// embedTexts 分批向量化。父 span 记总量，每批一个子 span 记批大小与 token 数，
// 这样既能看整体耗时，也能定位是哪一批卡住。
func (s *Service) embedTexts(ctx context.Context, documentID uuid.UUID, texts []string) ([][]float32, error) {
	ctx, span := startSpan(ctx, "ai.embed",
		attribute.Int("ai.chunk_count", len(texts)),
		attribute.Int("ai.batch_size", embeddingBatchSize),
	)
	defer span.End()

	embeddings := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embeddingBatchSize {
		end := min(start+embeddingBatchSize, len(texts))
		batch, err := s.embedBatch(ctx, texts[start:end])
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
		embeddings = append(embeddings, batch...)
		// 每批结束续租，避免长文档被租约超时判定为僵死任务。
		if err := s.touchDocument(ctx, documentID); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, err
		}
	}
	return embeddings, nil
}

func (s *Service) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	tokens := 0
	for _, text := range texts {
		tokens += len([]rune(text)) / 2
	}
	ctx, span := startSpan(ctx, "ai.embed.batch",
		attribute.Int("ai.batch_size", len(texts)),
		attribute.Int("ai.estimated_tokens", tokens),
	)
	batch, err := s.provider.Embeddings(ctx, texts)
	endSpan(span, err)
	return batch, err
}

// enrich 是 LLM 摘要/标签调用，单独一个 ai.llm span：它通常是整条索引链路里
// 最贵也最容易超时的一段。
func (s *Service) enrich(ctx context.Context, text string) (Insight, error) {
	ctx, span := startSpan(ctx, "ai.llm",
		attribute.String("ai.operation", "enrich"),
		attribute.Int("ai.estimated_tokens", len([]rune(text))/2),
	)
	insight, err := s.provider.Enrich(ctx, text)
	endSpan(span, err)
	return insight, err
}

// answer 是问答的 LLM 调用，与 enrich 共用 ai.llm span 名，用 ai.operation 区分。
func (s *Service) answer(ctx context.Context, question string, evidence []Evidence) (AnswerResult, error) {
	ctx, span := startSpan(ctx, "ai.llm",
		attribute.String("ai.operation", "answer"),
		attribute.Int("ai.evidence_count", len(evidence)),
	)
	result, err := s.provider.Answer(ctx, question, evidence)
	endSpan(span, err)
	return result, err
}

// embedQuery 是检索侧的查询向量化，与索引侧的 ai.embed 区分开：
// 排查检索延迟时要能一眼看出这一跳花了多少。
func (s *Service) embedQuery(ctx context.Context, query string) ([][]float32, error) {
	ctx, span := startSpan(ctx, "ai.embed.query",
		attribute.Int("ai.estimated_tokens", len([]rune(query))/2),
	)
	embeddings, err := s.provider.Embeddings(ctx, []string{query})
	endSpan(span, err)
	return embeddings, err
}

// recallSpanNames 把召回模式映射到 span 名，让 T5 的延迟拆解可以直接按 span 名聚合。
var recallSpanNames = map[string]string{
	"name":     "ai.search.name",
	"fulltext": "ai.search.fts",
	"semantic": "ai.search.vector",
}

// recall 是 queryCandidates 的埋点包装。权限过滤下推到 SQL 后，这一个 span
// 的耗时就等于"带权限的召回"耗时，不需要再减掉应用层过滤的时间。
func (s *Service) recall(ctx context.Context, ownerID uuid.UUID, input SearchInput, mode, vector string) ([]searchCandidate, error) {
	name, ok := recallSpanNames[mode]
	if !ok {
		name = "ai.search." + mode
	}
	ctx, span := startSpan(ctx, name,
		attribute.String("ai.search_mode", mode),
		attribute.Int("ai.limit", input.Limit),
		attribute.Bool("ai.include_subfolders", input.IncludeSubfolders),
		attribute.Bool("ai.folder_scoped", input.FolderID != nil),
	)
	candidates, err := s.queryCandidates(ctx, ownerID, input, mode, vector)
	if err == nil {
		span.SetAttributes(attribute.Int("ai.candidate_count", len(candidates)))
	}
	endSpan(span, err)
	return candidates, err
}

// fuse 记录 RRF 融合：两路召回各自的候选数与融合后的命中数，
// 是判断"某一路是否形同虚设"的直接证据。
func fuse(ctx context.Context, fulltext, semantic []searchCandidate, limit int) []SearchHit {
	_, span := startSpan(ctx, "ai.rrf",
		attribute.Int("ai.fulltext_candidates", len(fulltext)),
		attribute.Int("ai.semantic_candidates", len(semantic)),
		attribute.Int("ai.limit", limit),
	)
	hits := reciprocalRankFusion(fulltext, semantic, limit)
	span.SetAttributes(attribute.Int("ai.hit_count", len(hits)))
	span.End()
	return hits
}

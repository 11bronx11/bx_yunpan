// Package aisystem implements AI-local preparation and publication. Control PG
// remains the only execution authority; this package has no business SQL pool.
package aisystem

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/11bronx11/bx_yunpan/backend/internal/platform/serviceauth"
)

type Planner struct {
	ProjectionBackends     []string
	TextEpoch, VectorEpoch int64
	TargetNamespace        string
	Profile                aiprotocol.Profile
	TargetEpoch            int64
	ProcessingKey          ed25519.PrivateKey
}

func Register(r asynccontrol.Registry) {
	for kind, queue := range map[string]string{"ai.intent.apply": "ai-content", "ai.source.prepare": "ai-content", "ai.text.build": "ai-content", "ai.embedding.generate": "ai-embedding", "ai.summary.generate": "ai-summary", "ai.project.text": "ai-project-text", "ai.project.vector": "ai-project-vector", "ai.object.tombstone": "ai-content", "ai.capability.finish": "ai-content", "ai.artifact.gc": "ai-maintenance"} {
		r[kind] = asynccontrol.Definition{Kind: kind, Queue: queue, Timeout: 30 * time.Minute, Lifetime: 24 * time.Hour, EffectTarget: asynccontrol.EffectAI, Priority: 2}
	}
	r["ai.job.finish"] = asynccontrol.Definition{Kind: "ai.job.finish", Queue: "core-command", Timeout: time.Minute, Lifetime: 24 * time.Hour, EffectTarget: asynccontrol.EffectCore, Priority: 1}
}

func (p *Planner) Intent(in aiprotocol.Intent) (asynccontrol.Spec, error) {
	if len(in.Targets) == 0 && in.ProfileID == "" {
		in.ProfileID = p.Profile.ID
		in.ProfileRevision = p.Profile.Revision
	}
	if in.Validate() != nil || in.ProfileID != p.Profile.ID || in.ProfileRevision != p.Profile.Revision {
		return asynccontrol.Spec{}, aiprotocol.ErrInvalid
	}
	input := aiprotocol.Input{Intent: in, Profile: p.Profile, TargetEpoch: max(1, p.TargetEpoch), TextTargetEpoch: max(1, p.TextEpoch), VectorTargetEpoch: max(1, p.VectorEpoch), TargetNamespace: p.TargetNamespace, ProjectionBackends: p.ProjectionBackends}
	deadline := time.Unix(in.RequestedAt, 0).Add(24 * time.Hour)
	if in.Legacy != nil && in.Legacy.Deadline.Before(deadline) {
		deadline = in.Legacy.Deadline
	}
	grant := aiprotocol.ProcessingGrant{IntentID: in.RequestID, InputIdentity: aiprotocol.HashJSON(in), ObjectID: in.ObjectID, ContentGeneration: in.ContentGeneration, ContentHash: in.ContentHash, Action: "object.read", Deadline: deadline.Unix()}
	token, err := serviceauth.Sign(p.ProcessingKey, "core-processing-v1", grant.Deadline, grant)
	if err != nil {
		return asynccontrol.Spec{}, err
	}
	input.ProcessingGrant = token
	s := spec("ai.intent.apply", input)
	if s.Deadline.IsZero() || deadline.Before(s.Deadline) {
		s.Deadline = deadline
	}
	return s, nil
}

func spec(kind string, in aiprotocol.Input) asynccontrol.Spec {
	b, _ := json.Marshal(in)
	resource := fmt.Sprintf("object:%s:%d", in.Intent.ObjectID, in.Intent.ContentGeneration)
	owner := resource
	if in.Intent.OwnerID.String() != "00000000-0000-0000-0000-000000000000" {
		owner = "owner:" + in.Intent.OwnerID.String()
	}
	// Grant signatures/expiry are authorization, never content identity.
	identity := in
	identity.ProcessingGrant = ""
	identity.Intent.JobID = [16]byte{}
	identity.Intent.OwnerID = [16]byte{}
	sp := asynccontrol.Spec{Kind: kind, SemanticKey: aiprotocol.Semantic(kind, identity), OwnerKey: owner, ResourceKey: resource, ShardKey: resource, Input: b}
	if old := in.Intent.Legacy; old != nil {
		sp.Attempts = old.Attempts
		sp.Dispatches = old.Dispatches
		sp.Recoveries = old.Recoveries
		sp.MaxAttempts = old.MaxAttempts
		sp.MaxDispatches = old.MaxDispatches
		sp.MaxRecoveries = old.MaxRecoveries
		sp.Deadline = old.Deadline
	}
	return sp
}

func (p *Planner) Continuations(t asynccontrol.Task, r asynccontrol.Result) ([]asynccontrol.Spec, error) {
	if !strings.HasPrefix(t.Kind, "ai.") || t.Kind == "ai.job.finish" {
		return nil, nil
	}
	var in aiprotocol.Input
	var out aiprotocol.Output
	if json.Unmarshal(t.Input, &in) != nil || json.Unmarshal(r.Body, &out) != nil || out.Validate() != nil || out.TaskID != t.ID || out.Generation != t.Generation || out.InputDigest != t.InputDigest || out.ObjectID != in.Intent.ObjectID || out.ContentGeneration != in.Intent.ContentGeneration {
		return nil, asynccontrol.ErrInvalid
	}
	return p.successors(t, in, aiprotocol.Published{OutputID: out.OutputID, ObjectID: out.ObjectID, ContentGeneration: out.ContentGeneration, SourceID: out.SourceID, ContentVersionID: out.ContentVersionID, EmbeddingSetID: out.EmbeddingSetID})
}

func (p *Planner) Completed(t asynccontrol.Task, c asynccontrol.Completion) ([]asynccontrol.Spec, error) {
	if !strings.HasPrefix(t.Kind, "ai.") || t.Kind == "ai.job.finish" {
		return nil, nil
	}
	if t.ContractVersion != 1 || c.Validate(t) != nil {
		return nil, asynccontrol.ErrInvalid
	}
	in, err := decode[aiprotocol.Input](t.Input)
	if err != nil {
		return nil, err
	}
	out, err := decode[aiprotocol.Published](c.Refs)
	if err != nil || out.Validate() != nil || out.ObjectID != in.Intent.ObjectID || out.ContentGeneration != in.Intent.ContentGeneration {
		return nil, asynccontrol.ErrConflict
	}
	return p.successors(t, in, out)
}

func (p *Planner) successors(t asynccontrol.Task, in aiprotocol.Input, out aiprotocol.Published) ([]asynccontrol.Spec, error) {
	if !slices.Contains([]string{"ai.intent.apply", "ai.source.prepare", "ai.text.build", "ai.embedding.generate", "ai.object.tombstone", "ai.artifact.gc"}, t.Kind) {
		return nil, nil
	}
	var kinds []string
	if t.Kind == "ai.object.tombstone" {
		in.Operation = "delete"
		kinds = []string{"ai.project.text", "ai.project.vector"}
	}
	switch t.Kind {
	case "ai.intent.apply":
		in.SourceID = out.SourceID
		in.ContentVersionID = out.ContentVersionID
		in.EmbeddingSetID = out.EmbeddingSetID
		// Apply resolves current compatible input pointers in its artifact. Source
		// is prepared for initial/all/source; targeted work uses frozen source refs.
		if !supportedText(in.Intent.MediaType) {
			break
		}
		if slices.Contains(in.Intent.Targets, "source") || in.SourceID.String() == "00000000-0000-0000-0000-000000000000" {
			kinds = []string{"ai.source.prepare"}
		} else {
			if slices.Contains(in.Intent.Targets, "text") {
				kinds = append(kinds, "ai.text.build")
			}
			if slices.Contains(in.Intent.Targets, "summary") {
				kinds = append(kinds, "ai.summary.generate")
			}
			if slices.Contains(in.Intent.Targets, "vectors") && !slices.Contains(in.Intent.Targets, "text") {
				kinds = append(kinds, "ai.embedding.generate")
			}
			if slices.Contains(in.Intent.Targets, "projection_text") {
				kinds = append(kinds, "ai.project.text")
			}
			if slices.Contains(in.Intent.Targets, "projection_vector") {
				kinds = append(kinds, "ai.project.vector")
			}
		}
	case "ai.source.prepare":
		in.SourceID = out.OutputID
		kinds = []string{"ai.text.build"}
		if in.Profile.EnableSummary {
			kinds = append(kinds, "ai.summary.generate")
		}
	case "ai.text.build":
		in.ContentVersionID = out.OutputID
		kinds = []string{"ai.project.text"}
		if in.Profile.EnableVectors {
			kinds = append(kinds, "ai.embedding.generate")
		}
	case "ai.artifact.gc":
		in.Operation = "retire"
		in.MaintenanceAt = t.NotBefore.Unix()
		kinds = []string{"ai.project.text", "ai.project.vector"}
	case "ai.embedding.generate":
		in.EmbeddingSetID = out.OutputID
		kinds = []string{"ai.project.vector"}
	}
	specs := []asynccontrol.Spec{}
	for _, kind := range kinds {
		if strings.HasPrefix(kind, "ai.project.") && in.Operation != "delete" && in.Operation != "retire" && len(in.ProjectionBackends) > 0 && !slices.Contains(in.ProjectionBackends, "es_qdrant") {
			continue
		}
		projectionInput := in
		if kind == "ai.project.text" {
			projectionInput.TargetEpoch = max(1, in.TextTargetEpoch)
		}
		if kind == "ai.project.vector" {
			projectionInput.TargetEpoch = max(1, in.VectorTargetEpoch)
		}
		child := spec(kind, projectionInput)
		child.Deadline = t.Deadline
		specs = append(specs, child)
	}
	if t.Kind == "ai.intent.apply" || t.Kind == "ai.artifact.gc" || t.Kind == "ai.object.tombstone" {
		due := t.CreatedAt.UTC().Truncate(24 * time.Hour).Add(9 * 24 * time.Hour)
		if t.Kind == "ai.artifact.gc" {
			due = t.NotBefore.Add(24 * time.Hour)
		}
		// One stable input per object and day; user requests and input pointers
		// must never give the same maintenance identity different payloads.
		gcInput := aiprotocol.Input{Intent: aiprotocol.Intent{ObjectID: in.Intent.ObjectID, ContentGeneration: in.Intent.ContentGeneration, ContentHash: in.Intent.ContentHash, Size: in.Intent.Size}}
		gc := spec("ai.artifact.gc", gcInput)
		gc.SemanticKey = fmt.Sprintf("ai.v2.gc:%s:%d:%s", in.Intent.ObjectID, in.Intent.ContentGeneration, due.UTC().Format("2006-01-02"))
		gc.NotBefore = due
		gc.Deadline = due.Add(24 * time.Hour)
		gc.Attempts = 0
		gc.Dispatches = 0
		gc.Recoveries = 0
		specs = append(specs, gc)
	}
	return specs, nil
}

func (p *Planner) Failure(t asynccontrol.Task) ([]asynccontrol.Spec, error) {
	cap := map[string]string{"ai.source.prepare": "source", "ai.text.build": "text", "ai.embedding.generate": "vector", "ai.summary.generate": "summary"}[t.Kind]
	if cap == "" {
		return nil, nil
	}
	var in aiprotocol.Input
	if json.Unmarshal(t.Input, &in) != nil {
		return nil, asynccontrol.ErrInvalid
	}
	// Descendants which will never be scheduled need a terminal observation too.
	// Each observation carries that capability's own seq; the publisher fences
	// late failures against newer builds and preserves previous active results.
	caps := []string{cap}
	if cap == "source" {
		caps = append(caps, "text", "vector", "summary")
	}
	if cap == "text" {
		caps = append(caps, "vector")
	}
	children := []asynccontrol.Spec{}
	for _, failed := range caps {
		if failed == "vector" && !in.Profile.EnableVectors || failed == "summary" && !in.Profile.EnableSummary || seq(in, failed) < 1 {
			continue
		}
		failure := in
		failure.FailureCapability = failed
		failure.ErrorCode = "ai." + cap + ".build_failed"
		failure.FailureSeq = seq(in, failed)
		child := spec("ai.capability.finish", failure)
		child.SemanticKey = "ai.v2.capability.finish:" + t.ID.String() + ":" + failed
		child.Trace = json.RawMessage(t.Trace)
		children = append(children, child)
	}
	return children, nil
}

func seq(in aiprotocol.Input, cap string) int64 {
	switch cap {
	case "source":
		return in.Intent.Seqs.Source
	case "text":
		return in.Intent.Seqs.Text
	case "vector":
		return in.Intent.Seqs.Vector
	case "summary":
		return in.Intent.Seqs.Summary
	}
	return 0
}

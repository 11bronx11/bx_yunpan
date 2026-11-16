// Package asynccontrol owns execution authority in an independent PostgreSQL
// database. It never writes business facts or calls object storage.
package asynccontrol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

var (
	ErrObsolete = errors.New("obsolete async result notice")
	ErrLost     = errors.New("async control lease or generation lost")
	ErrConflict = errors.New("async semantic identity input conflict")
	ErrInvalid  = errors.New("invalid async contract")
)

const (
	Ready          = "ready"
	Dispatched     = "dispatched"
	Running        = "running"
	RetryWait      = "retry_wait"
	Computed       = "computed"
	Succeeded      = "succeeded"
	Superseded     = "superseded"
	Rejected       = "rejected"
	Exhausted      = "exhausted"
	Converging     = "convergence_pending"
	Failed         = "failed"
	Quarantined    = "quarantined"
	NeedsAttention = "needs_attention"
)

func Terminal(state string) bool {
	switch state {
	case Succeeded, Superseded, Rejected, Failed, Quarantined, NeedsAttention:
		return true
	}
	return false
}

type Definition struct {
	Kind, Queue       string
	Timeout, Lifetime time.Duration
	BusinessEffect    bool // Deprecated: use Target() to resolve legacy definitions.
	EffectTarget      string
	Converge          bool
	Priority          int
}

// Registry is immutable after startup. Queue, budgets, completion and failure
// modes come from trusted code, never arbitrary message fields.
type Registry map[string]Definition

func CoreRegistry() Registry {
	r := Registry{}
	add := func(kind, queue string, timeout time.Duration, priority int, converge bool) {
		r[kind] = Definition{Kind: kind, Queue: queue, Timeout: timeout, Lifetime: 24 * time.Hour, EffectTarget: EffectCore, Converge: converge, Priority: priority}
	}
	for _, kind := range []string{"multipart.create", "multipart.complete", "canonical.ensure"} {
		add(kind, "upload", 10*time.Minute, 0, true)
	}
	add("upload.verify", "object", 30*time.Minute, 0, true)
	add("thumbnail", "media", 5*time.Minute, 2, false)
	for _, kind := range []string{"multipart.abort", "multipart.sweep", "staging.delete", "object.delete", "variant.delete", "output.delete"} {
		add(kind, "gc", 10*time.Minute, 2, false)
	}
	for _, kind := range []string{"upload.expire", "object.authorize_delete", "failure.converge"} {
		add(kind, "commit", time.Minute, 0, false)
	}
	r["ai.index"] = Definition{Kind: "ai.index", Queue: "ai", Timeout: 30 * time.Minute, Lifetime: 24 * time.Hour, Priority: 2}
	r["ai.reprocess"] = Definition{Kind: "ai.reprocess", Queue: "ai", Timeout: 30 * time.Minute, Lifetime: 24 * time.Hour, EffectTarget: EffectCore, Converge: true, Priority: 1}
	r["search.project"] = Definition{Kind: "search.project", Queue: "search", Timeout: 5 * time.Minute, Lifetime: 24 * time.Hour, Priority: 2}
	return r
}

type Task struct {
	ID                                                                                                       uuid.UUID `gorm:"type:uuid;primaryKey"`
	EffectTarget                                                                                             string
	SemanticKey, Kind                                                                                        string
	ContractVersion                                                                                          int
	InputDigest                                                                                              string
	Input                                                                                                    datatypes.JSON `gorm:"type:jsonb"`
	Queue, OwnerKey, ResourceKey, ShardKey                                                                   string
	LayoutVersion, Shard, Priority                                                                           int
	State                                                                                                    string
	Version, Generation, DispatchGeneration                                                                  int64
	Token                                                                                                    *uuid.UUID `gorm:"type:uuid"`
	LeaseUntil                                                                                               *time.Time
	NotBefore, Deadline                                                                                      time.Time
	CommitDeadline                                                                                           *time.Time
	Attempts, MaxAttempts, Dispatches, MaxDispatches, Recoveries, MaxRecoveries, ResultSends, MaxResultSends int
	TimeoutMS                                                                                                int64      `gorm:"column:timeout_ms"`
	ResultID, ParentID                                                                                       *uuid.UUID `gorm:"type:uuid"`
	LastError                                                                                                string
	Trace                                                                                                    datatypes.JSON `gorm:"type:jsonb"`
	StartedAt                                                                                                *time.Time
	CreatedAt, UpdatedAt                                                                                     time.Time
	StoppedAt                                                                                                *time.Time
}

func (Task) TableName() string { return "tasks" }

type Result struct {
	ID, TaskID                uuid.UUID `gorm:"type:uuid"`
	Generation                int64
	InputDigest, ResultDigest string
	EffectTarget              string
	Body                      datatypes.JSON `gorm:"type:jsonb"`
	Outcome                   string
	CommittedAt               *time.Time
	CreatedAt                 time.Time
}

func (Result) TableName() string { return "results" }

type Spec struct {
	Kind, SemanticKey, OwnerKey, ResourceKey, ShardKey string
	Input                                              json.RawMessage
	Trace                                              json.RawMessage
	NotBefore                                          time.Time
	// Explicit historical bounds may only tighten the configured defaults.
	Deadline                     time.Time
	MaxAttempts                  int
	MaxDispatches, MaxRecoveries int
	ParentID                     *uuid.UUID
	// Imported history can only consume budget; it cannot grant extra attempts.
	Attempts, Dispatches, Recoveries int
}
type Notice struct {
	EffectTarget       string          `json:"effect_target,omitempty"`
	SemanticKey        string          `json:"semantic_key,omitempty"`
	InputDigest        string          `json:"input_digest,omitempty"`
	ShardKey           string          `json:"shard_key,omitempty"`
	LayoutVersion      int             `json:"layout_version,omitempty"`
	Generation         int64           `json:"generation"`
	Trace              json.RawMessage `json:"trace,omitempty"`
	TaskID             uuid.UUID       `json:"task_id"`
	DispatchGeneration int64           `json:"dispatch_generation"`
	ResultID           *uuid.UUID      `json:"result_id,omitempty"`
	Version            int             `json:"version"`
	Shard              int             `json:"shard"`
}

func (t Task) Notice() Notice {
	return Notice{EffectTarget: t.EffectTarget, TaskID: t.ID, DispatchGeneration: t.DispatchGeneration, ResultID: t.ResultID, Version: 1, Shard: t.Shard, SemanticKey: t.SemanticKey, InputDigest: t.InputDigest, ShardKey: t.ShardKey, LayoutVersion: t.LayoutVersion, Generation: t.Generation, Trace: json.RawMessage(t.Trace)}
}

func (n Notice) validWire() bool {
	return n.Version == 1 && n.TaskID != uuid.Nil && n.LayoutVersion == 1 && n.SemanticKey != "" && n.InputDigest != "" && n.ShardKey != ""
}

func (n Notice) matches(t Task) bool {
	return (n.EffectTarget == "" && t.EffectTarget != EffectAI || n.EffectTarget == t.EffectTarget) && n.SemanticKey == t.SemanticKey && n.InputDigest == t.InputDigest && n.ShardKey == t.ShardKey && n.LayoutVersion == t.LayoutVersion && n.Shard == t.Shard && n.Generation == t.Generation
}

func Digest(input []byte) (string, []byte, error) {
	d := json.NewDecoder(bytes.NewReader(input))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return "", nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return "", nil, ErrInvalid
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), b, nil
}

func Shard(key string, count int) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum64() % uint64(count))
}

func Backoff(attempt int, base, cap time.Duration, jitter float64) time.Duration {
	if base <= 0 || cap <= 0 {
		return 0
	}
	d := min(base, cap)
	for i := 1; i < attempt && d < cap; i++ {
		if d > cap/2 {
			d = cap
		} else {
			d *= 2
		}
	}
	if jitter != jitter { // NaN must not reach a float-to-duration conversion.
		jitter = 0
	}
	jitter = max(-1, min(1, jitter))
	// Convert only the bounded adjustment, not the complete duration: a
	// float64 rounds MaxInt64 to 1<<63, which would overflow time.Duration.
	adjustment := time.Duration(float64(d) * (0.2 * jitter))
	if adjustment > 0 && d > cap-adjustment {
		return cap
	}
	return max(time.Nanosecond, d+adjustment)
}

func (r Registry) Validate(s Spec) (Definition, error) {
	d, ok := r[s.Kind]
	if !ok || (d.Target() != EffectNone && d.Target() != EffectCore && d.Target() != EffectAI) || s.SemanticKey == "" || s.ShardKey == "" || s.ResourceKey == "" || len(s.Input) == 0 || len(s.Input) > 1<<20 || s.Attempts < 0 || s.Dispatches < 0 || s.Recoveries < 0 {
		return d, fmt.Errorf("%w: %s", ErrInvalid, s.Kind)
	}
	return d, nil
}

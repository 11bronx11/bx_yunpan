package aisystem

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
	"github.com/11bronx11/bx_yunpan/backend/internal/ai/rag"
	"github.com/11bronx11/bx_yunpan/backend/internal/aiprotocol"
	"github.com/11bronx11/bx_yunpan/backend/internal/asynccontrol"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The proxy observes the actual AMQP Basic.Ack frame emitted by the production
// worker/committer loop. Holding it proves the preceding PG COMMIT returned,
// without installing a hook in the production scheduler or guessing a sleep.
func ackGate(t *testing.T, hold bool) (string, <-chan struct{}, func()) {
	t.Helper()
	u, err := url.Parse(os.Getenv("TEST_RABBITMQ_URL"))
	if err != nil || u.Host == "" {
		t.Fatal("invalid dedicated broker URL")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := u.Host
	u.Host = l.Addr().String()
	reached, stop := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	go func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				server, err := net.DialTimeout("tcp", upstream, time.Second)
				if err != nil {
					return
				}
				defer server.Close()
				go func() { <-stop; _ = client.Close(); _ = server.Close() }()
				go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
				if _, err := io.CopyN(server, client, 8); err != nil {
					return
				}
				for {
					header := make([]byte, 7)
					if _, err := io.ReadFull(client, header); err != nil {
						return
					}
					n := binary.BigEndian.Uint32(header[3:])
					if n > 4<<20 {
						return
					}
					payload := make([]byte, int(n)+1)
					if _, err := io.ReadFull(client, payload); err != nil {
						return
					}
					if header[0] == 1 && n >= 4 && binary.BigEndian.Uint16(payload) == 60 && binary.BigEndian.Uint16(payload[2:]) == 80 {
						select {
						case reached <- struct{}{}:
						default:
						}
						if hold {
							<-stop
							return
						}
					}
					if _, err := server.Write(append(header, payload...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	closeGate := func() { once.Do(func() { close(stop); _ = l.Close() }) }
	t.Cleanup(closeGate)
	return u.String(), reached, closeGate
}

func ackBroker(t *testing.T) *asynccontrol.Broker {
	t.Helper()
	base := os.Getenv("TEST_RABBITMQ_URL")
	if base == "" {
		t.Skip("dedicated test broker required")
	}
	b := asynccontrol.NewBroker(base, 32, "bx.ai.ack."+uuid.NewString())
	b.Timeout = 20 * time.Second
	t.Cleanup(func() {
		b.Close()
		conn, err := amqp.Dial(base)
		if err != nil {
			t.Error("owned queue cleanup connection failed")
			return
		}
		defer conn.Close()
		ch, err := conn.Channel()
		if err != nil {
			t.Error(err)
			return
		}
		defer ch.Close()
		for _, q := range asynccontrol.WorkQueues() {
			if _, err := ch.QueueDelete(b.WorkQueue(q), false, false, false); err != nil {
				t.Error(err)
			}
		}
		for n := 0; n < 32; n++ {
			for _, target := range []string{asynccontrol.EffectAI, asynccontrol.EffectCore} {
				if _, err := ch.QueueDelete(b.ResultQueueFor(target, n), false, false, false); err != nil {
					t.Error(err)
				}
			}
		}
		if _, err := ch.QueueDelete(b.Prefix+".dead", false, false, false); err != nil {
			t.Error(err)
		}
		if err := ch.ExchangeDelete(b.Prefix, false, false); err != nil {
			t.Error(err)
		}
	})
	if err := b.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b
}

func scopedControlConnection(t *testing.T, s *asynccontrol.Store) string {
	t.Helper()
	var schema string
	if err := s.DB.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("TEST_CONTROL_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	return u.String()
}

type ackFixture struct {
	Mode, Prefix, Queue string
	Body                json.RawMessage
}

func TestAIAckProcessHelper(t *testing.T) {
	path := os.Getenv("AI_DEEP_ACK_FIXTURE")
	if path == "" {
		return
	}
	var f ackFixture
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &f) != nil {
		t.Fatal("invalid child fixture")
	}
	open := func(key string) *gorm.DB {
		db, err := gorm.Open(postgres.Open(os.Getenv(key)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal("child DB unavailable")
		}
		pool, _ := db.DB()
		t.Cleanup(func() { _ = pool.Close() })
		return db
	}
	s := asynccontrol.New(open("AI_DEEP_CONTROL_DSN"))
	s.Lease = 3 * time.Second
	Register(s.Registry)
	planner := &Planner{}
	s.PlanContinuations = planner.Continuations
	s.PlanFailure = planner.Failure
	s.Observe = planner.Observe
	s.AfterTerminal = planner.Finish
	b := asynccontrol.NewBroker(os.Getenv("AI_DEEP_ACK_BROKER"), 32, f.Prefix)
	b.Timeout = 20 * time.Second
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if f.Mode == "worker" {
		err = s.RunWorker(ctx, b, f.Queue, 1, func(context.Context, asynccontrol.Task) (json.RawMessage, error) {
			file, err := os.OpenFile(path+".calls", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return nil, err
			}
			_, err = file.WriteString("compute\n")
			_ = file.Close()
			return f.Body, err
		}, func(error) bool { return false }, log)
	} else {
		p := &Publisher{DB: open("AI_DEEP_AI_DSN"), Control: s}
		err = s.RunCommittersFor(ctx, b, asynccontrol.EffectAI, "ack-child", p.Commit, log)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestAIProductionLoopsSIGKILLBeforeACK(t *testing.T) {
	for _, mode := range []string{"worker", "committer"} {
		t.Run(mode, func(t *testing.T) {
			db, s := databases(t)
			s.Lease = 3 * time.Second
			b := ackBroker(t)
			in := testInput()
			seedObject(t, db, in)
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			var task asynccontrol.Task
			var result asynccontrol.Result
			var body json.RawMessage
			if mode == "worker" {
				var err error
				if _, err = s.Ensure(ctx, spec("ai.source.prepare", in), "ack-test"); err != nil {
					t.Fatal(err)
				}
				var ok bool
				task, ok, err = s.Reserve(ctx, "ai-content")
				if err != nil || !ok {
					t.Fatal("reserve", ok, err)
				}
				// A deterministic compute result exercises the production Claim,
				// SaveResult and ACK sequence; no external model is called.
				data, _ := json.Marshal(sourceData{Parsed: ai.ParseResult{Sections: []rag.Section{{Text: "ack crash", Kind: "text"}}}, Coverage: "complete"})
				hash, data, _ := asynccontrol.Digest(data)
				id := uuid.New()
				computed := task
				computed.Generation++ // Claim starts the next execution generation.
				a := Artifact{ObjectID: in.Intent.ObjectID, ContentGeneration: 1, ArtifactID: uuid.New(), TaskID: task.ID, Generation: computed.Generation, Kind: task.Kind, InputDigest: task.InputDigest, ArtifactHash: hash, TypedOutputRef: id, RowCount: 1, ByteCount: int64(len(data)), Complete: true, Data: data}
				if err := db.Create(&a).Error; err != nil {
					t.Fatal(err)
				}
				body, _ = json.Marshal(aiprotocolOutput(computed, a, id))
				if err := b.Publish(ctx, b.WorkQueue(task.Queue), task.Notice()); err != nil {
					t.Fatal(err)
				}
			} else {
				task = claim(t, s, "ai.source.prepare", in)
				result, _ = stage(t, db, s, task, in, sourceData{Parsed: ai.ParseResult{Sections: []rag.Section{{Text: "report then ACK", Kind: "text"}}}, Coverage: "complete"}, uuid.New(), 1)
				n := task.Notice()
				n.ResultID = &result.ID
				if err := b.Publish(ctx, b.ResultQueueFor(asynccontrol.EffectAI, task.Shard), n); err != nil {
					t.Fatal(err)
				}
			}
			proxy, reached, stop := ackGate(t, true)
			path := filepath.Join(t.TempDir(), "ack.json")
			raw, _ := json.Marshal(ackFixture{mode, b.Prefix, task.Queue, body})
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			executable, _ := os.Executable()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestAIAckProcessHelper$")
			child.Env = append(os.Environ(), "AI_DEEP_ACK_FIXTURE="+path, "AI_DEEP_ACK_BROKER="+proxy, "AI_DEEP_CONTROL_DSN="+scopedControlConnection(t, s), "AI_DEEP_AI_DSN="+scopedAIConnection(t, db))
			var output strings.Builder
			child.Stdout = &output
			child.Stderr = &output
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Process.Kill() })
			select {
			case <-reached:
			case <-ctx.Done():
				_ = child.Process.Kill()
				_ = child.Wait()
				t.Fatalf("ACK boundary missing: %s", output.String())
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err == nil {
				t.Fatal("child not killed")
			}
			stop()
			var current asynccontrol.Task
			if err := s.DB.First(&current, "id=?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.ResultID == nil {
				t.Fatalf("ACK emitted before durable result: state=%s attempts=%d child=%s", current.State, current.Attempts, output.String())
			}
			if mode == "worker" {
				if current.State != asynccontrol.Computed {
					t.Fatal(current.State)
				}
				raw, err := os.ReadFile(path + ".calls")
				if err != nil || string(raw) != "compute\n" {
					t.Fatal("computation count mismatch")
				}
			} else if current.State != asynccontrol.Succeeded {
				t.Fatal("ACK emitted before Report", current.State)
			}
			// Reconnect through another observed ACK to prove broker redelivery
			// reached the actual production loop, rather than checking queue depth.
			proxy2, replayed, close2 := ackGate(t, false)
			defer close2()
			replayBroker := asynccontrol.NewBroker(proxy2, 32, b.Prefix)
			replayBroker.Timeout = 20 * time.Second
			defer replayBroker.Close()
			live, done := context.WithCancel(ctx)
			var recomputes atomic.Int32
			finished := make(chan error, 1)
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			// Cancellation may discard a SQL connection. Runtime pools must carry
			// schema isolation in the DSN rather than inherit the parent's SET.
			openReplay := func(dsn string) *gorm.DB {
				pool, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
				if err != nil {
					t.Fatal("replay database unavailable")
				}
				sql, _ := pool.DB()
				t.Cleanup(func() { _ = sql.Close() })
				return pool
			}
			replayStore := asynccontrol.New(openReplay(scopedControlConnection(t, s)))
			replayStore.Lease = s.Lease
			Register(replayStore.Registry)
			replayStore.PlanContinuations, replayStore.PlanFailure = s.PlanContinuations, s.PlanFailure
			replayStore.Observe, replayStore.AfterTerminal = s.Observe, s.AfterTerminal
			replayAI := openReplay(scopedAIConnection(t, db))
			go func() {
				if mode == "worker" {
					finished <- replayStore.RunWorker(live, replayBroker, task.Queue, 1, func(context.Context, asynccontrol.Task) (json.RawMessage, error) { recomputes.Add(1); return body, nil }, func(error) bool { return false }, log)
				} else {
					p := &Publisher{DB: replayAI, Control: replayStore}
					finished <- replayStore.RunCommittersFor(live, replayBroker, asynccontrol.EffectAI, "ack-replay", p.Commit, log)
				}
			}()
			select {
			case <-replayed:
			case <-ctx.Done():
				done()
				<-finished
				t.Fatal("unacknowledged message did not redeliver")
			}
			done()
			<-finished
			if recomputes.Load() != 0 {
				t.Fatal("computed task recalculated")
			}
			if err := s.DB.First(&current, "id=?", task.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.Attempts != 1 {
				t.Fatal("redelivery consumed execution budget", current.Attempts)
			}
			p := &Publisher{DB: db, Control: s}
			for range 2 {
				if err := p.Commit(ctx, current.Notice()); err != nil {
					t.Fatal(err)
				}
			}
			var receipts, sources, successors int64
			for _, err := range []error{
				db.Table("ai_commit_receipts").Count(&receipts).Error,
				db.Table("ai_sources").Count(&sources).Error,
				s.DB.Table("tasks").Where("parent_id=?", task.ID).Count(&successors).Error,
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			if receipts != 1 || sources != 1 || successors != 2 {
				t.Fatal("duplicate publication", receipts, sources, successors)
			}
		})
	}
}

func aiprotocolOutput(t asynccontrol.Task, a Artifact, id uuid.UUID) aiprotocol.Output {
	return aiprotocol.Output{Version: 2, TaskID: t.ID, Generation: t.Generation, InputDigest: t.InputDigest, ArtifactID: a.ArtifactID, ArtifactHash: a.ArtifactHash, OutputID: id, ObjectID: a.ObjectID, ContentGeneration: 1, RowCount: 1, ByteCount: a.ByteCount, Coverage: "complete", Complete: true}
}

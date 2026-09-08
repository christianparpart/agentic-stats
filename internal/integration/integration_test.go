package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/auth"
	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/ingest"
	"github.com/christianparpart/agentic-stats/internal/pricing"
	"github.com/christianparpart/agentic-stats/internal/store"
	"github.com/christianparpart/agentic-stats/internal/storetest"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

// tenant is one enrolled user with a device, as the API would see it.
type tenant struct {
	userID string
	device auth.Device
}

func newTenant(t *testing.T, db *store.DB, email string) tenant {
	t.Helper()
	svc, err := auth.NewService(db)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	ctx := context.Background()

	userID, err := svc.CreateUser(ctx, email, "correct-horse-battery-staple", auth.RoleUser)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	code, err := svc.CreateEnrollmentCode(ctx, userID, time.Hour)
	if err != nil {
		t.Fatalf("CreateEnrollmentCode: %v", err)
	}
	token, err := svc.RedeemEnrollment(ctx, code, wire.Device{
		Hostname: "host-" + email, OS: "linux", Arch: "arm64", Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("RedeemEnrollment: %v", err)
	}
	dev, err := svc.AuthenticateDevice(ctx, token)
	if err != nil {
		t.Fatalf("AuthenticateDevice: %v", err)
	}
	if dev.UserID != userID {
		t.Fatalf("device belongs to %s, want %s", dev.UserID, userID)
	}
	return tenant{userID: userID, device: dev}
}

// assistantLine builds a transcript line shaped like the real thing: one
// content block per line, every line repeating the whole usage object.
func assistantLine(requestID, model string, output, cacheRead int64) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"timestamp":"2026-09-08T12:00:00.000Z",
		"message":{"model":%q,"usage":{"input_tokens":10,"output_tokens":%d,
		"cache_read_input_tokens":%d,"output_tokens_details":{"thinking_tokens":5},
		"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":0}}}}`,
		requestID, model, output, cacheRead)
}

func records(lines ...string) []wire.Record {
	out := make([]wire.Record, len(lines))
	for i, l := range lines {
		out[i] = wire.NewRecord("claudecode", "/logs/s.jsonl", int64(i*1000), []byte(l))
	}
	return out
}

func TestEnrollmentIsSingleUse(t *testing.T) {
	db := storetest.Open(t)
	svc, err := auth.NewService(db)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	ctx := context.Background()

	userID, err := svc.CreateUser(ctx, "once@example.invalid", "pw", auth.RoleUser)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	code, err := svc.CreateEnrollmentCode(ctx, userID, time.Hour)
	if err != nil {
		t.Fatalf("CreateEnrollmentCode: %v", err)
	}
	dev := wire.Device{Hostname: "h", OS: "linux", Arch: "amd64"}

	if _, err := svc.RedeemEnrollment(ctx, code, dev); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	if _, err := svc.RedeemEnrollment(ctx, code, dev); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Errorf("replayed code: err = %v, want ErrInvalidCredential", err)
	}
	if _, err := svc.AuthenticateDevice(ctx, "not-a-real-token"); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Errorf("bogus token: err = %v, want ErrInvalidCredential", err)
	}
}

// The property the whole pipeline rests on: re-sending is free and harmless,
// which is what lets the collector advance its cursor only after an ack.
func TestIngestDeduplicatesReplayedBatches(t *testing.T) {
	db := storetest.Open(t)
	ten := newTenant(t, db, "dedupe@example.invalid")
	svc, err := ingest.NewService(db)
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	ctx := context.Background()

	batch := records(
		assistantLine("req-1", "claude-opus-5", 100, 1000),
		assistantLine("req-2", "claude-opus-5", 200, 2000),
	)

	first, err := svc.Store(ctx, ten.device, wire.Header{}, batch)
	if err != nil {
		t.Fatalf("first Store: %v", err)
	}
	if first.Stored != 2 || first.Duplicates != 0 {
		t.Errorf("first store = %+v, want 2 stored / 0 duplicates", first)
	}

	for i := range 3 {
		again, err := svc.Store(ctx, ten.device, wire.Header{}, batch)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if again.Stored != 0 || again.Duplicates != 2 {
			t.Errorf("replay %d = %+v, want 0 stored / 2 duplicates", i, again)
		}
	}
}

// The fold, end to end through the database.
func TestDeriveFoldsRepeatedUsageByRequestID(t *testing.T) {
	db := storetest.Open(t)
	ten := newTenant(t, db, "fold@example.invalid")

	ingestSvc, err := ingest.NewService(db)
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	prices, err := pricing.Load()
	if err != nil {
		t.Fatalf("pricing.Load: %v", err)
	}
	deriveSvc, err := derive.NewService(db, prices)
	if err != nil {
		t.Fatalf("derive.NewService: %v", err)
	}
	ctx := context.Background()

	// One API response written as four lines, as Claude Code does for a
	// thinking + text + two tool_use response. Every line repeats the usage.
	// A second, distinct request follows.
	batch := records(
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-B", "claude-opus-5", 500, 2000),
		// A synthetic error line carries no usable usage and must be excluded.
		`{"type":"assistant","requestId":null,"message":{"model":"<synthetic>","usage":{"output_tokens":0}}}`,
	)
	if _, err := ingestSvc.Store(ctx, ten.device, wire.Header{}, batch); err != nil {
		t.Fatalf("Store: %v", err)
	}

	sum, err := deriveSvc.Summarize(ctx, ten.userID)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	if sum.Requests != 2 {
		t.Errorf("requests = %d, want 2 (req-A folded from 4 lines, plus req-B)", sum.Requests)
	}
	if sum.AssistantLines != 5 {
		t.Errorf("assistant lines = %d, want 5", sum.AssistantLines)
	}
	if len(sum.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(sum.Models))
	}
	// Naive summing would give 4*1000 + 500 = 4500.
	if got := sum.Models[0].OutputTokens; got != 1500 {
		t.Errorf("output tokens = %d, want 1500; naive summing would give 4500", got)
	}
	if got := sum.Models[0].CacheReadTokens; got != 7000 {
		t.Errorf("cache read tokens = %d, want 7000", got)
	}
	if !sum.Models[0].Priced {
		t.Error("claude-opus-5 must be priced")
	}
	if sum.CacheSavingsUSD <= 0 {
		t.Errorf("cache savings = %v, want positive", sum.CacheSavingsUSD)
	}
}

// The invariant that protects everyone's source code from everyone else's.
func TestRowLevelSecurityIsolatesTenants(t *testing.T) {
	db := storetest.Open(t)
	alice := newTenant(t, db, "alice@example.invalid")
	bob := newTenant(t, db, "bob@example.invalid")

	ingestSvc, err := ingest.NewService(db)
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	prices, err := pricing.Load()
	if err != nil {
		t.Fatalf("pricing.Load: %v", err)
	}
	deriveSvc, err := derive.NewService(db, prices)
	if err != nil {
		t.Fatalf("derive.NewService: %v", err)
	}
	ctx := context.Background()

	if _, err := ingestSvc.Store(ctx, alice.device, wire.Header{},
		records(assistantLine("alice-req", "claude-opus-5", 999, 1))); err != nil {
		t.Fatalf("store for alice: %v", err)
	}

	aliceSum, err := deriveSvc.Summarize(ctx, alice.userID)
	if err != nil {
		t.Fatalf("summarize alice: %v", err)
	}
	if aliceSum.Lines != 1 {
		t.Errorf("alice sees %d lines, want 1", aliceSum.Lines)
	}

	bobSum, err := deriveSvc.Summarize(ctx, bob.userID)
	if err != nil {
		t.Fatalf("summarize bob: %v", err)
	}
	if bobSum.Lines != 0 {
		t.Errorf("bob sees %d of alice's lines, want 0 -- tenant isolation is broken", bobSum.Lines)
	}
	if bobSum.Requests != 0 {
		t.Errorf("bob sees %d of alice's requests, want 0", bobSum.Requests)
	}
}

// A connection that never establishes a tenant must see nothing, not everything.
func TestQueriesWithoutATenantSeeNothing(t *testing.T) {
	db := storetest.Open(t)
	alice := newTenant(t, db, "notenant@example.invalid")

	ingestSvc, err := ingest.NewService(db)
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	ctx := context.Background()
	if _, err := ingestSvc.Store(ctx, alice.device, wire.Header{},
		records(assistantLine("r", "claude-opus-5", 1, 1))); err != nil {
		t.Fatalf("Store: %v", err)
	}

	var visible int64
	err = db.InAuthTx(ctx, func(ctx context.Context, tx pgxTx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM raw_lines`).Scan(&visible)
	})
	if err != nil {
		t.Fatalf("count without tenant: %v", err)
	}
	if visible != 0 {
		t.Errorf("a tenantless connection saw %d rows, want 0; "+
			"is the application role a superuser or does it hold BYPASSRLS?", visible)
	}
}

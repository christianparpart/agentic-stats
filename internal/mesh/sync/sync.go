// Package sync converges two replicas over an authenticated connection.
//
// Anti-entropy is a version vector, not a Merkle tree. Every record carries
// (origin_id, seq), dense and monotonic per origin, so "what am I missing?" is
// answered exactly by exchanging one integer per origin. For a handful of
// nodes that summary is a few hundred bytes and has no false positives, while
// range-based reconciliation and Bloom filters exist to locate differences
// among billions of items across thousands of peers -- a problem this is not.
//
// The exchange is symmetric: neither side leads. Both announce what they hold,
// both send what the other lacks, and because records are immutable and never
// deleted there is no merge rule and nothing to resolve.
package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/christianparpart/agentic-stats/internal/store"
)

// batchSize is how many records travel in one message.
const batchSize = 500

// maxMessageBytes bounds a single decoded message, so a peer cannot exhaust
// memory even though it holds the mesh key.
const maxMessageBytes = 64 << 20

// messageKind names a protocol message.
type messageKind string

const (
	kindVector  messageKind = "vector"
	kindRecords messageKind = "records"
	kindDone    messageKind = "done"
)

// message is one protocol frame.
type message struct {
	Kind messageKind `json:"kind"`
	// Vector is present on kindVector.
	Vector store.VersionVector `json:"vector,omitempty"`
	// Digests accompanies the vector: origin -> bucket -> content digest.
	// It is what lets two replicas notice they disagree inside a range they
	// both claim to hold, which a watermark alone cannot express.
	Digests map[string]map[int64]string `json:"digests,omitempty"`
	// Records is present on kindRecords.
	Records []wireRecord `json:"records,omitempty"`
}

// announcement is what a node says about itself at the start of an exchange.
type announcement struct {
	vector  store.VersionVector
	digests map[string]map[int64]string
}

// wireRecord is a record in transit.
//
// Sealed carries ciphertext, so this format is readable only as metadata even
// by something that captures it. Base64 costs a third more bytes than a binary
// framing would; ciphertext does not compress, so the saving from a binary
// encoding would be the JSON overhead alone and is not worth a second codec.
//
// Every field of store.Record must appear here. The pull-request columns did
// not for a long time, so no pull request a session opened on one machine was
// ever visible on another: the receiver stored the record with pr_repo NULL and
// the delivery report simply had nothing to join against. Nothing failed and
// nothing was logged, which is why sync_test asserts this by reflection over
// store.Record rather than by anyone remembering to add a field twice.
//
// Adding a field is compatible in both directions: an old node ignores what it
// does not know, and a field a peer never sent decodes to its zero value, which
// the receiving node's own reprocessor then fills in from the sealed body.
type wireRecord struct {
	OriginID     string `json:"o"`
	Seq          int64  `json:"n"`
	Source       string `json:"s"`
	Path         string `json:"p"`
	ByteOffset   int64  `json:"b"`
	ContentHash  string `json:"h"`
	SessionID    string `json:"sid,omitempty"`
	LineUUID     string `json:"uid,omitempty"`
	CapturedAt   string `json:"t,omitempty"`
	RequestID    string `json:"r,omitempty"`
	Model        string `json:"m,omitempty"`
	Input        int64  `json:"i,omitempty"`
	Output       int64  `json:"ot,omitempty"`
	Thinking     int64  `json:"th,omitempty"`
	CacheRead    int64  `json:"cr,omitempty"`
	CacheWrite5m int64  `json:"c5,omitempty"`
	CacheWrite1h int64  `json:"c1,omitempty"`
	PRRepo       string `json:"pr,omitempty"`
	PRNumber     int64  `json:"prn,omitempty"`
	CWD          string `json:"cwd,omitempty"`
	GitBranch    string `json:"br,omitempty"`
	Sealed       []byte `json:"d"`
}

func toWire(r store.Record) wireRecord {
	return wireRecord{
		OriginID: r.OriginID, Seq: r.Seq, Source: r.Source, Path: r.Path,
		ByteOffset: r.ByteOffset, ContentHash: r.ContentHash,
		SessionID: r.SessionID, LineUUID: r.LineUUID, CapturedAt: r.CapturedAt,
		RequestID: r.RequestID, Model: r.Model,
		Input: r.Input, Output: r.Output, Thinking: r.Thinking,
		CacheRead: r.CacheRead, CacheWrite5m: r.CacheWrite5m, CacheWrite1h: r.CacheWrite1h,
		PRRepo: r.PRRepo, PRNumber: r.PRNumber,
		CWD: r.CWD, GitBranch: r.GitBranch,
		Sealed: r.Sealed,
	}
}

func fromWire(w wireRecord) store.Record {
	return store.Record{
		OriginID: w.OriginID, Seq: w.Seq, Source: w.Source, Path: w.Path,
		ByteOffset: w.ByteOffset, ContentHash: w.ContentHash,
		SessionID: w.SessionID, LineUUID: w.LineUUID, CapturedAt: w.CapturedAt,
		RequestID: w.RequestID, Model: w.Model,
		Input: w.Input, Output: w.Output, Thinking: w.Thinking,
		CacheRead: w.CacheRead, CacheWrite5m: w.CacheWrite5m, CacheWrite1h: w.CacheWrite1h,
		PRRepo: w.PRRepo, PRNumber: w.PRNumber,
		CWD: w.CWD, GitBranch: w.GitBranch,
		Sealed: w.Sealed,
	}
}

// Stats summarizes one exchange.
type Stats struct {
	// Sent and Received are record counts.
	Sent     int
	Received int
	// Stored is how many received records were new.
	Stored int
	// Duplicates is how many the peer had already given us. Expected.
	Duplicates int
	// Forked is how many were quarantined as evidence of a duplicated origin.
	Forked int
	// PeerVector is what the peer said it held, recorded so replication lag
	// can be computed rather than guessed. Nil if the peer never announced.
	PeerVector store.VersionVector
}

// Syncer converges a local replica with peers.
type Syncer struct {
	db  *store.DB
	log *slog.Logger
}

// New returns a Syncer over db.
func New(db *store.DB, log *slog.Logger) (*Syncer, error) {
	if db == nil {
		return nil, errors.New("sync: a store is required")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Syncer{db: db, log: log}, nil
}

// Exchange converges with the peer on the far side of rw.
//
// Sending and receiving run concurrently from the first byte, including the
// vector exchange. Doing the vectors as a blocking write-then-read would
// deadlock the moment both sides' messages exceed the transport's buffer --
// which a TCP socket hides until a mesh grows enough origins to fill it, and
// an unbuffered pipe exposes immediately.
func (s *Syncer) Exchange(ctx context.Context, rw io.ReadWriter, peerID string) (Stats, error) {
	mine, err := s.announce(ctx)
	if err != nil {
		return Stats{}, err
	}

	// The receiving side hands the peer's announcement to the sender here.
	peerVector := make(chan announcement, 1)

	type sendResult struct {
		sent int
		err  error
	}
	sendDone := make(chan sendResult, 1)
	go func() {
		if err := writeFrame(rw, message{
			Kind: kindVector, Vector: mine.vector, Digests: mine.digests,
		}); err != nil {
			sendDone <- sendResult{err: fmt.Errorf("sync: send vector: %w", err)}
			return
		}
		var theirs announcement
		select {
		case theirs = <-peerVector:
		case <-ctx.Done():
			sendDone <- sendResult{err: ctx.Err()}
			return
		}
		n, err := s.send(ctx, rw, mine, theirs)
		sendDone <- sendResult{sent: n, err: err}
	}()

	stats, recvErr := s.receive(ctx, rw, peerVector)
	sent := <-sendDone

	stats.Sent = sent.sent
	if recvErr != nil {
		return stats, recvErr
	}
	if sent.err != nil {
		return stats, sent.err
	}
	s.log.Debug("exchange complete", "peer", peerID,
		"sent", stats.Sent, "received", stats.Received,
		"stored", stats.Stored, "forked", stats.Forked)
	return stats, nil
}

// announce gathers what this node holds and how it hashes.
func (s *Syncer) announce(ctx context.Context) (announcement, error) {
	vec, err := s.db.Vector(ctx)
	if err != nil {
		return announcement{}, err
	}
	digests := make(map[string]map[int64]string, len(vec))
	for origin := range vec {
		d, err := s.db.Digests(ctx, origin)
		if err != nil {
			return announcement{}, err
		}
		if len(d) > 0 {
			digests[origin] = d
		}
	}
	return announcement{vector: vec, digests: digests}, nil
}

// resendFrom returns the sequence to send from for one origin, or -1 to send
// nothing.
//
// Normally that is the peer's watermark. But where both sides claim the same
// range and their digests disagree, the watermark is lying: rewind to the
// start of the first mismatched bucket so the receiver sees the conflicting
// records and can quarantine them. Without this a fork at an already-agreed
// sequence is invisible to both sides forever.
func resendFrom(have int64, peerSeq int64, mineDigests, theirsDigests map[int64]string) int64 {
	if peerSeq < have {
		// The ordinary case: they are simply behind.
		earliest := peerSeq
		if from := firstMismatch(mineDigests, theirsDigests); from >= 0 && from < earliest {
			earliest = from
		}
		return earliest
	}
	if from := firstMismatch(mineDigests, theirsDigests); from >= 0 {
		return from
	}
	return -1
}

// firstMismatch returns the sequence starting the lowest bucket where the two
// sides hold *different* records, or -1 when they agree wherever comparable.
//
// Only buckets holding the same number of records are compared. A differing
// count means one side is merely behind, which the watermark already handles;
// treating that as divergence would re-send the whole bucket on every ordinary
// catch-up.
func firstMismatch(mine, theirs map[int64]string) int64 {
	found := int64(-1)
	for bucket, digest := range mine {
		other, ok := theirs[bucket]
		if !ok || other == digest {
			continue
		}
		if countOf(digest) != countOf(other) {
			continue
		}
		start := bucket * store.BucketSize
		if found < 0 || start < found {
			found = start
		}
	}
	return found
}

// countOf reads the record count from a "count:hash" digest.
func countOf(digest string) string {
	if i := strings.IndexByte(digest, ':'); i >= 0 {
		return digest[:i]
	}
	return digest
}

// send streams every record the peer lacks, then closes with a done frame.
func (s *Syncer) send(ctx context.Context, w io.Writer, mine, theirs announcement) (int, error) {
	total := 0
	for origin, have := range mine.vector {
		after := resendFrom(have, theirs.vector[origin], mine.digests[origin], theirs.digests[origin])
		if after < 0 {
			continue
		}
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			batch, err := s.db.Since(ctx, origin, after, batchSize)
			if err != nil {
				return total, err
			}
			if len(batch) == 0 {
				break
			}
			out := make([]wireRecord, len(batch))
			for i, r := range batch {
				out[i] = toWire(r)
			}
			if err := writeFrame(w, message{Kind: kindRecords, Records: out}); err != nil {
				return total, fmt.Errorf("sync: send records: %w", err)
			}
			total += len(batch)
			after = batch[len(batch)-1].Seq
		}
	}
	if err := writeFrame(w, message{Kind: kindDone}); err != nil {
		return total, fmt.Errorf("sync: send done: %w", err)
	}
	return total, nil
}

// receive reads the peer's vector, publishes it to the sender, then applies
// everything the peer sends until it says it is finished.
func (s *Syncer) receive(ctx context.Context, r io.Reader, peerVector chan<- announcement) (Stats, error) {
	var stats Stats
	published := false
	defer func() {
		if !published {
			// Unblock the sender even on a failed read, or Exchange would
			// wait forever for a goroutine that can never proceed.
			close(peerVector)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		msg, err := readFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The peer hung up without a done frame. What arrived is
				// already committed, and the vector reflects only that.
				return stats, nil
			}
			return stats, fmt.Errorf("sync: read message: %w", err)
		}
		switch msg.Kind {
		case kindVector:
			if published {
				return stats, errors.New("sync: peer sent a second vector")
			}
			peerVector <- announcement{vector: msg.Vector, digests: msg.Digests}
			stats.PeerVector = msg.Vector
			published = true
		case kindDone:
			return stats, nil
		case kindRecords:
			recs := make([]store.Record, len(msg.Records))
			for i, w := range msg.Records {
				recs[i] = fromWire(w)
			}
			res, err := s.db.AppendRemote(ctx, recs)
			if err != nil {
				return stats, err
			}
			stats.Received += len(recs)
			stats.Stored += res.Stored
			stats.Duplicates += res.Duplicates
			stats.Forked += res.Forked
			if res.Forked > 0 {
				// Loud on purpose: this means two machines share an origin id.
				s.log.Error("origin fork detected; records quarantined, not merged",
					"count", res.Forked)
			}
		default:
			return stats, fmt.Errorf("sync: unknown message kind %q", msg.Kind)
		}
	}
}

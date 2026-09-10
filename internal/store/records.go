package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net"
	"strings"
	"time"
)

// ErrForked reports that a record arrived under an (origin, seq) that is
// already held with different content.
//
// This means two machines are issuing records under the same origin id — a
// cloned VM, a restored database, an rsynced config. It is the one failure
// that would otherwise corrupt silently: the receiver would discard one
// machine's records forever while reporting healthy convergence.
var ErrForked = errors.New("store: origin fork detected")

// Record is one archived line as it lives locally.
type Record struct {
	OriginID    string
	Seq         int64
	Source      string
	Path        string
	ByteOffset  int64
	ContentHash string
	SessionID   string
	LineUUID    string
	CapturedAt  string

	// RequestID is empty unless this line is a billable assistant response.
	RequestID    string
	Model        string
	Input        int64
	Output       int64
	Thinking     int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64

	// PRRepo and PRNumber are set on pr-link lines.
	PRRepo   string
	PRNumber int64

	// CWD is the working directory the line was written in, which is this
	// archive's project identity, and GitBranch the branch checked out there.
	CWD       string
	GitBranch string

	Sealed []byte
}

// AppendResult reports what an append did.
type AppendResult struct {
	// Stored is how many records were new.
	Stored int
	// Duplicates is how many were already held. Expected, not exceptional.
	Duplicates int
	// Forked is how many were quarantined as fork evidence.
	Forked int
	// LastSeq is the highest sequence assigned or accepted.
	LastSeq int64
}

// loadOrMintIdentity reads this node's origin id, creating one on first run.
func (db *DB) loadOrMintIdentity(ctx context.Context) error {
	return db.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT origin_id FROM node WHERE singleton = 1`)
		switch err := row.Scan(&db.origin); {
		case err == nil:
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: read node identity: %w", err)
		}

		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("store: mint node identity: %w", err)
		}
		db.origin = hex.EncodeToString(buf)
		_, err := tx.ExecContext(ctx,
			`INSERT INTO node (singleton, origin_id, created_at) VALUES (1, ?, ?)`,
			db.origin, db.now().Format(time.RFC3339Nano))
		return err
	})
}

// AppendLocal stores records this node collected itself, assigning sequence
// numbers.
//
// The sequence is allocated inside the same transaction as the insert, so the
// database is the counter and two writers cannot disagree. That is what makes
// the sequence gap-free, which anti-entropy depends on.
func (db *DB) AppendLocal(ctx context.Context, recs []Record) (AppendResult, error) {
	var out AppendResult
	if len(recs) == 0 {
		return out, nil
	}
	err := db.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		next, err := nextSeq(ctx, tx, db.origin)
		if err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, insertSQL)
		if err != nil {
			return fmt.Errorf("store: prepare insert: %w", err)
		}
		defer func() { _ = stmt.Close() }() // statement is scoped to this tx

		now := db.now().Format(time.RFC3339Nano)
		for i := range recs {
			r := recs[i]
			r.OriginID = db.origin
			r.Seq = next
			res, err := execInsert(ctx, stmt, r, now)
			if err != nil {
				return err
			}
			if res == 0 {
				// A duplicate under the semantic index: the same line already
				// present, so no sequence is consumed.
				out.Duplicates++
				continue
			}
			out.Stored++
			out.LastSeq = next
			next++
		}
		return nil
	})
	return out, err
}

// AppendRemote stores records received from a peer, keeping their origin and
// sequence, and quarantining anything that proves an origin fork.
func (db *DB) AppendRemote(ctx context.Context, recs []Record) (AppendResult, error) {
	var out AppendResult
	if len(recs) == 0 {
		return out, nil
	}
	err := db.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := db.now().Format(time.RFC3339Nano)
		stmt, err := tx.PrepareContext(ctx, insertSQL)
		if err != nil {
			return fmt.Errorf("store: prepare insert: %w", err)
		}
		defer func() { _ = stmt.Close() }() // statement is scoped to this tx

		for i := range recs {
			r := recs[i]
			if r.OriginID == "" || r.Seq <= 0 {
				return fmt.Errorf("store: remote record missing origin or sequence")
			}

			// Fork check before insert: a held (origin, seq) with a different
			// content hash is proof that an origin id is in use twice.
			var held string
			row := tx.QueryRowContext(ctx,
				`SELECT content_hash FROM records WHERE origin_id = ? AND seq = ?`, r.OriginID, r.Seq)
			switch err := row.Scan(&held); {
			case err == nil && held == r.ContentHash:
				out.Duplicates++
				continue
			case err == nil:
				if _, qerr := tx.ExecContext(ctx, `
					INSERT INTO quarantine (origin_id, seq, content_hash, sealed, noticed_at)
					VALUES (?, ?, ?, ?, ?)`,
					r.OriginID, r.Seq, r.ContentHash, r.Sealed, now); qerr != nil {
					return fmt.Errorf("store: quarantine fork: %w", qerr)
				}
				out.Forked++
				continue
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("store: fork check: %w", err)
			}

			res, err := execInsert(ctx, stmt, r, now)
			if err != nil {
				return err
			}
			if res == 0 {
				out.Duplicates++
				continue
			}
			out.Stored++
			if r.Seq > out.LastSeq {
				out.LastSeq = r.Seq
			}
		}
		return nil
	})
	return out, err
}

const insertSQL = `
	INSERT INTO records
	    (origin_id, seq, source, path, byte_offset, content_hash,
	     session_id, line_uuid, captured_at,
	     request_id, model, input_tokens, output_tokens, think_tokens,
	     cache_read, cache_write5m, cache_write1h,
	     pr_repo, pr_number, cwd, git_branch,
	     sealed, received_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT DO NOTHING`

// execInsert runs one insert and reports whether a row was written.
func execInsert(ctx context.Context, stmt *sql.Stmt, r Record, now string) (int64, error) {
	res, err := stmt.ExecContext(ctx,
		r.OriginID, r.Seq, r.Source, r.Path, r.ByteOffset, r.ContentHash,
		nullable(r.SessionID), nullable(r.LineUUID), nullable(r.CapturedAt),
		nullable(r.RequestID), nullable(r.Model),
		r.Input, r.Output, r.Thinking, r.CacheRead, r.CacheWrite5m, r.CacheWrite1h,
		nullable(r.PRRepo), nullableInt(r.PRNumber),
		nullable(r.CWD), nullable(r.GitBranch),
		r.Sealed, now)
	if err != nil {
		return 0, fmt.Errorf("store: insert record: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: rows affected: %w", err)
	}
	return n, nil
}

// nullable maps an empty string to SQL NULL, so the partial unique indexes
// behave as intended.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableInt maps zero to SQL NULL, so a partial index skips the rows that
// carry no value rather than indexing a wall of zeroes.
func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// nextSeq returns the next free sequence for an origin.
func nextSeq(ctx context.Context, tx *sql.Tx, origin string) (int64, error) {
	var max sql.NullInt64
	row := tx.QueryRowContext(ctx, `SELECT max(seq) FROM records WHERE origin_id = ?`, origin)
	if err := row.Scan(&max); err != nil {
		return 0, fmt.Errorf("store: read sequence: %w", err)
	}
	return max.Int64 + 1, nil
}

// VersionVector is the highest sequence held per origin.
type VersionVector map[string]int64

// Vector returns what this node holds, which is the complete summary a peer
// needs to know what to send.
func (db *DB) Vector(ctx context.Context) (VersionVector, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT origin_id, max(seq) FROM records GROUP BY origin_id`)
	if err != nil {
		return nil, fmt.Errorf("store: read version vector: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	vec := make(VersionVector)
	for rows.Next() {
		var origin string
		var seq int64
		if err := rows.Scan(&origin, &seq); err != nil {
			return nil, fmt.Errorf("store: scan version vector: %w", err)
		}
		vec[origin] = seq
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read version vector: %w", err)
	}
	// This node always announces itself, even before it has collected
	// anything, so a peer can tell a silent node from an absent one.
	if _, ok := vec[db.origin]; !ok {
		vec[db.origin] = 0
	}
	return vec, nil
}

// Since returns up to limit records for one origin above a sequence, in order.
// This is the query anti-entropy walks, and the primary key serves it directly.
func (db *DB) Since(ctx context.Context, origin string, after int64, limit int) ([]Record, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT origin_id, seq, source, path, byte_offset, content_hash,
		       coalesce(session_id, ''), coalesce(line_uuid, ''),
		       coalesce(captured_at, ''), coalesce(request_id, ''), coalesce(model, ''),
		       input_tokens, output_tokens, think_tokens,
		       cache_read, cache_write5m, cache_write1h,
		       coalesce(pr_repo, ''), coalesce(pr_number, 0),
		       coalesce(cwd, ''), coalesce(git_branch, ''), sealed
		  FROM records
		 WHERE origin_id = ? AND seq > ?
		 ORDER BY seq
		 LIMIT ?`, origin, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read records since: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.OriginID, &r.Seq, &r.Source, &r.Path, &r.ByteOffset,
			&r.ContentHash, &r.SessionID, &r.LineUUID, &r.CapturedAt,
			&r.RequestID, &r.Model, &r.Input, &r.Output, &r.Thinking,
			&r.CacheRead, &r.CacheWrite5m, &r.CacheWrite1h,
			&r.PRRepo, &r.PRNumber, &r.CWD, &r.GitBranch, &r.Sealed); err != nil {
			return nil, fmt.Errorf("store: scan record: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read records since: %w", err)
	}
	return out, nil
}

// Count reports how many records this node holds.
func (db *DB) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM records`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count records: %w", err)
	}
	return n, nil
}

// Quarantined reports how many records are held as fork evidence. A non-zero
// count means an origin id is in use on two machines and needs attention.
func (db *DB) Quarantined(ctx context.Context) (int64, error) {
	var n int64
	if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM quarantine`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count quarantine: %w", err)
	}
	return n, nil
}

// BucketSize is how many sequence numbers one digest covers.
const BucketSize = 1000

// Digests returns a content digest per sequence bucket for one origin,
// formatted as "count:hash".
//
// A version vector says "I have everything up to N" but never verifies it, so
// two replicas that disagree *within* a range they both claim -- a corrupted
// page, or two machines issuing different records under one origin id -- look
// identical to it. Comparing digests catches that for roughly one percent of
// the cost of a Merkle tree: a mismatched bucket means re-send that thousand.
//
// The count is part of the value because two nodes disagree for two very
// different reasons. Different counts mean one side is simply behind, which is
// ordinary. The same count with a different hash means they hold *different*
// records at the same positions, which is divergence and must be surfaced.
func (db *DB) Digests(ctx context.Context, origin string) (map[int64]string, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT (seq - 1) / ?, seq, content_hash
		  FROM records
		 WHERE origin_id = ?
		 ORDER BY seq`, BucketSize, origin)
	if err != nil {
		return nil, fmt.Errorf("store: read digests: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	acc := make(map[int64]hash.Hash)
	counts := make(map[int64]int)
	for rows.Next() {
		var bucket, seq int64
		var contentHash string
		if err := rows.Scan(&bucket, &seq, &contentHash); err != nil {
			return nil, fmt.Errorf("store: scan digest row: %w", err)
		}
		h, ok := acc[bucket]
		if !ok {
			h = sha256.New()
			acc[bucket] = h
		}
		// Sequence and hash both feed the digest, so a record appearing at the
		// wrong position is as visible as wrong content. hash.Hash never
		// reports an error from Write, which is why the interface documents
		// it as such.
		_, _ = fmt.Fprintf(h, "%d:%s\n", seq, contentHash)
		counts[bucket]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read digests: %w", err)
	}

	out := make(map[int64]string, len(acc))
	for bucket, h := range acc {
		out[bucket] = fmt.Sprintf("%d:%s", counts[bucket], hex.EncodeToString(h.Sum(nil)))
	}
	return out, nil
}

// Peer is a known node and where it was last reachable.
type Peer struct {
	ID       string
	Addrs    []string
	LastSeen string
	Static   bool

	// LastConverged is when data last actually moved with this peer, empty if
	// it never has. Distinct from LastSeen, which a beacon announcement
	// satisfies without anything being exchanged.
	LastConverged string
	// LastError is why the most recent attempt failed, empty if it did not.
	// Kept alongside LastConverged rather than replacing it, so the report can
	// say "converged an hour ago and has been failing since".
	LastError string
	// LastVector is what the peer said it held at the last exchange, as the
	// sync layer's JSON. Opaque here: the store keeps it, derive and the API
	// interpret it.
	LastVector string
	// Hostname is what the peer calls itself, empty until it has said so.
	//
	// Self-reported over the authenticated sync channel, not looked up. It is
	// what names a machine on a network whose resolver knows nothing about it,
	// which is the ordinary case for a home LAN.
	Hostname string
}

// SavePeer records or refreshes a peer.
//
// Learned addresses are persisted so a cold start does not depend on the
// bootstrap node being up, and so a peer that has been quiet for a week is
// still dialled.
func (db *DB) SavePeer(ctx context.Context, p Peer) error {
	if p.ID == "" {
		return errors.New("store: peer id is required")
	}
	static := 0
	if p.Static {
		static = 1
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO peers (peer_id, addrs, last_seen, static)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (peer_id) DO UPDATE SET
			addrs     = excluded.addrs,
			last_seen = excluded.last_seen,
			static    = max(peers.static, excluded.static)`,
		p.ID, strings.Join(p.Addrs, ","), db.now().Format(time.RFC3339Nano), static)
	if err != nil {
		return fmt.Errorf("store: save peer: %w", err)
	}
	return nil
}

// RecordConvergence notes the outcome of one exchange with a peer.
//
// A success clears the last error and a failure leaves the last success in
// place, so the pair together answer "when did this last work, and what has
// been happening since" -- which is the question worth asking of a mesh, and
// which neither field can answer alone.
//
// The peer row is created if this is the first thing heard from it, so a peer
// that fails on its very first exchange is still visible as a peer that is
// failing rather than not appearing at all.
func (db *DB) RecordConvergence(ctx context.Context, peerID, vector string, cause error) error {
	if peerID == "" {
		return errors.New("store: peer id is required")
	}
	now := db.now().Format(time.RFC3339Nano)

	var converged, lastErr, vec any
	if cause != nil {
		lastErr = cause.Error()
	} else {
		converged, vec = now, vector
	}

	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO peers (peer_id, addrs, last_seen, static,
		                   last_converged, last_error, last_vector)
		VALUES (?, '', ?, 0, ?, ?, ?)
		ON CONFLICT (peer_id) DO UPDATE SET
			last_seen      = excluded.last_seen,
			last_converged = coalesce(excluded.last_converged, peers.last_converged),
			last_error     = excluded.last_error,
			last_vector    = coalesce(excluded.last_vector, peers.last_vector)`,
		peerID, now, converged, lastErr, vec)
	if err != nil {
		return fmt.Errorf("store: record convergence: %w", err)
	}
	return nil
}

// SavePeerHostname records the name a peer reports for itself.
//
// Only ever called with a name the peer stated over an authenticated channel,
// and never with an empty one: a peer too old to report a name must not erase
// the name it gave before it was upgraded.
func (db *DB) SavePeerHostname(ctx context.Context, peerID, hostname string) error {
	if peerID == "" {
		return errors.New("store: peer id is required")
	}
	if hostname == "" {
		return nil
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO peers (peer_id, addrs, last_seen, static, hostname)
		VALUES (?, '', ?, 0, ?)
		ON CONFLICT (peer_id) DO UPDATE SET hostname = excluded.hostname`,
		peerID, db.now().Format(time.RFC3339Nano), hostname)
	if err != nil {
		return fmt.Errorf("store: save peer hostname: %w", err)
	}
	return nil
}

// Hostname is one address's reverse-DNS answer.
type Hostname struct {
	// Name is the name the address answers to, empty when it has none.
	//
	// Empty is an answer and not a missing one -- the missing case is no row
	// at all. Keeping the negative is what stops a mesh of unnamed addresses
	// asking a resolver the same doomed question every sweep.
	Name string
	// ResolvedAt is when the lookup was made, as RFC3339Nano.
	ResolvedAt string
}

// Hostnames returns every reverse lookup this node has made, keyed by address.
func (db *DB) Hostnames(ctx context.Context) (map[string]Hostname, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT ip, hostname, resolved_at FROM hostnames`)
	if err != nil {
		return nil, fmt.Errorf("store: read hostnames: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	out := make(map[string]Hostname)
	for rows.Next() {
		var ip string
		var h Hostname
		if err := rows.Scan(&ip, &h.Name, &h.ResolvedAt); err != nil {
			return nil, fmt.Errorf("store: scan hostname: %w", err)
		}
		out[ip] = h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read hostnames: %w", err)
	}
	return out, nil
}

// SaveHostname records one reverse lookup. An empty name records that the
// address has none.
func (db *DB) SaveHostname(ctx context.Context, ip, name string) error {
	if ip == "" {
		return errors.New("store: address is required")
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO hostnames (ip, hostname, resolved_at) VALUES (?, ?, ?)
		ON CONFLICT (ip) DO UPDATE SET
			hostname    = excluded.hostname,
			resolved_at = excluded.resolved_at`,
		ip, name, db.now().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store: save hostname: %w", err)
	}
	return nil
}

// PeerNames maps each known peer to the friendliest name this node has for it.
//
// A peer id is an origin id -- the mesh mints one from the other -- so this is
// also how a record's origin becomes something a person can read. Only peers
// with a resolved reverse-DNS name appear; a peer whose addresses have none is
// absent rather than present under a placeholder, so the caller decides what
// to show instead of unpicking one here.
//
// This node is not in the result. It has no peers row, by design: the mesh
// excludes itself from its own peer list.
func (db *DB) PeerNames(ctx context.Context) (map[string]string, error) {
	peers, err := db.Peers(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := db.Hostnames(ctx)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(peers))
	for _, p := range peers {
		// What the machine calls itself beats what a resolver was willing to
		// say about one of its addresses, and is usually the only answer.
		if p.Hostname != "" {
			out[p.ID] = p.Hostname
			continue
		}
		for _, addr := range p.Addrs {
			host := addr
			if h, _, err := net.SplitHostPort(addr); err == nil {
				host = h
			}
			if name := hosts[host].Name; name != "" {
				out[p.ID] = name
				break
			}
		}
	}
	return out, nil
}

// Peers returns every peer this node knows about.
func (db *DB) Peers(ctx context.Context) ([]Peer, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT peer_id, addrs, coalesce(last_seen, ''), static,
		       coalesce(last_converged, ''), coalesce(last_error, ''),
		       coalesce(last_vector, ''), coalesce(hostname, '')
		  FROM peers ORDER BY peer_id`)
	if err != nil {
		return nil, fmt.Errorf("store: read peers: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	var out []Peer
	for rows.Next() {
		var p Peer
		var addrs string
		var static int
		if err := rows.Scan(&p.ID, &addrs, &p.LastSeen, &static,
			&p.LastConverged, &p.LastError, &p.LastVector, &p.Hostname); err != nil {
			return nil, fmt.Errorf("store: scan peer: %w", err)
		}
		if addrs != "" {
			p.Addrs = strings.Split(addrs, ",")
		}
		p.Static = static == 1
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read peers: %w", err)
	}
	return out, nil
}

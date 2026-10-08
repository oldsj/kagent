package database

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
)

// RuntimeGeneration is private issuance authority. It never contains plaintext.
// Records survive Session deletion and permanently reserve names and URIs.
type RuntimeGeneration struct {
	ID            uuid.UUID
	SessionID     uuid.UUID
	Atespace      string
	ActorName     string
	CredentialURI string
	TokenDigest   []byte
	ActorUID      string
	Phase         string
	CreatedAt     time.Time
}

// AllocateRuntimeGeneration reserves one identity before external effects. Only
// the original CREATE operation of a fresh Session may allocate; retries return
// its original record. Restored, READY and otherwise unledgered Sessions deny.
func (c *Client) AllocateRuntimeGeneration(ctx context.Context, candidate RuntimeGeneration) (*RuntimeGeneration, bool, error) {
	var result *RuntimeGeneration
	var allocated bool
	err := c.withTx(ctx, func(tx pgx.Tx) error {
		session, err := lockSession(ctx, tx, candidate.SessionID.String())
		if err != nil {
			return notFoundOr(err)
		}
		rows, err := queryMany(ctx, tx, `SELECT id, session_id, atespace, actor_name, credential_uri, token_digest, actor_uid, phase, created_at FROM runtime_generation WHERE session_id = $1 ORDER BY created_at`, pgx.RowToStructByName[RuntimeGeneration], candidate.SessionID)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			if len(rows) != 1 || rows[0].Phase == "revoked" {
				return ErrFailedPrecondition
			}
			result = &rows[0]
			return nil
		}
		eligible, err := queryOne(ctx, tx, `SELECT runtime_generation_eligible FROM session WHERE id = $1`, pgx.RowTo[bool], candidate.SessionID)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrFailedPrecondition
		}
		if session.State != "RUNTIME_STATE_CREATING" || session.Operation != "RUNTIME_OPERATION_CREATE" || session.ExecutorID != nil {
			return ErrFailedPrecondition
		}
		row, err := queryOne(ctx, tx, `INSERT INTO runtime_generation (id, session_id, atespace, actor_name, credential_uri, token_digest, phase) VALUES ($1,$2,$3,$4,$5,$6,'allocated') RETURNING id, session_id, atespace, actor_name, credential_uri, token_digest, actor_uid, phase, created_at`, pgx.RowToStructByName[RuntimeGeneration], candidate.ID, candidate.SessionID, candidate.Atespace, candidate.ActorName, candidate.CredentialURI, candidate.TokenDigest)
		result = &row
		allocated = err == nil
		return err
	})
	return result, allocated, err
}

// GetRuntimeGeneration resolves issued authority without deriving it from a UID
// lookup. Revoked records remain available for controller cleanup, never auth.
func (c *Client) GetRuntimeGeneration(ctx context.Context, sessionID string) (*RuntimeGeneration, error) {
	row, err := queryOne(ctx, c.db, `SELECT id, session_id, atespace, actor_name, credential_uri, token_digest, actor_uid, phase, created_at FROM runtime_generation WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1`, pgx.RowToStructByName[RuntimeGeneration], sessionID)
	return &row, notFoundOr(err)
}

// GetRuntimeGenerationByDigest accepts only active server-side bindings.
func (c *Client) GetRuntimeGenerationByDigest(ctx context.Context, digest []byte) (*RuntimeGeneration, error) {
	row, err := queryOne(ctx, c.db, `SELECT id, session_id, atespace, actor_name, credential_uri, token_digest, actor_uid, phase, created_at FROM runtime_generation WHERE token_digest = $1 AND phase = 'active'`, pgx.RowToStructByName[RuntimeGeneration], digest)
	return &row, notFoundOr(err)
}

// AdvanceRuntimeGeneration compares durable phases and pins the first known UID.
// Actor issuance grants authority only to the caller that changes secret-issued.
// Later transitions can acknowledge the same UID, never replace identity.
func (c *Client) AdvanceRuntimeGeneration(ctx context.Context, id uuid.UUID, from, to, uid string) error {
	valid := from == "allocated" && to == "secret-issued" && uid == "" || from == "secret-issued" && to == "actor-issued" && uid == "" || from == "actor-issued" && to == "bound" && uid != "" || from == "bound" && to == "active" && uid != ""
	if !valid {
		return ErrFailedPrecondition
	}
	tag, err := c.db.Exec(ctx, `UPDATE runtime_generation SET phase = $3, actor_uid = CASE WHEN $4 = '' THEN actor_uid ELSE $4 END WHERE id = $1 AND (phase = $2 OR (phase = $3 AND $3 <> 'actor-issued')) AND (actor_uid = '' OR $4 = '' OR actor_uid = $4)`, id, from, to, uid)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// RevokeRuntimeGeneration serializes revocation with all callback operations.
// It retains the immutable identity permanently; no external calls occur here.
func (c *Client) RevokeRuntimeGeneration(ctx context.Context, sessionID string) error {
	return c.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockSession(ctx, tx, sessionID); err != nil {
			return notFoundOr(err)
		}
		return execSQL(ctx, tx, `UPDATE runtime_generation SET phase = 'revoked' WHERE session_id = $1`, sessionID)
	})
}

// RuntimeTaskStore is the database-only callback port. The store owns its
// transaction; the callback must never perform external network operations.
type RuntimeTaskStore interface {
	SettleSessionTask(context.Context, string, string, int64) error
	GetSessionForRuntime(context.Context, string, string) (*apiv1alpha1.Session, error)
	CreateRuntimeTask(context.Context, string, []byte, *a2a.Task, string) (int64, error)
	GetVersionedSessionTask(context.Context, string, string) (*a2a.Task, int64, error)
	UpdateSessionTask(context.Context, string, int64, []byte, *a2a.Task, a2a.Event, string) (int64, error)
	ListSessionTasks(context.Context, string, string, a2a.TaskState, *time.Time, int, *int) ([]*a2a.Task, int, error)
}

// WithRuntimeGeneration checks active authority in the same transaction that
// reads/writes task data. Lock order is Session then generation, as in revocation.
func (c *Client) WithRuntimeGeneration(ctx context.Context, binding RuntimeGeneration, fn func(RuntimeTaskStore) error) error {
	return c.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockSession(ctx, tx, binding.SessionID.String()); err != nil {
			return notFoundOr(err)
		}
		current, err := queryOne(ctx, tx, `SELECT id, session_id, atespace, actor_name, credential_uri, token_digest, actor_uid, phase, created_at FROM runtime_generation WHERE id = $1 AND phase = 'active' FOR UPDATE`, pgx.RowToStructByName[RuntimeGeneration], binding.ID)
		if err != nil {
			return notFoundOr(err)
		}
		if current.SessionID != binding.SessionID || current.Atespace != binding.Atespace || current.ActorName != binding.ActorName || current.CredentialURI != binding.CredentialURI || current.ActorUID != binding.ActorUID || !bytes.Equal(current.TokenDigest, binding.TokenDigest) {
			return fmt.Errorf("runtime binding changed: %w", ErrFailedPrecondition)
		}
		return fn(&Client{db: runtimeTransaction{Tx: tx}})
	})
}

// Nested task operations use savepoints within the already-authorized transaction.
// A read cannot escape to the pool or commit independently of revocation checking.
type runtimeTransaction struct{ pgx.Tx }

var _ transactionalDB = runtimeTransaction{}
var _ RuntimeTaskStore = (*Client)(nil)

func (t runtimeTransaction) BeginTx(ctx context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return t.Begin(ctx)
}

// SessionGenerationEligible distinguishes fresh, never-issued Session deletion
// from legacy rows. Eligibility is set only by new Session insertion.
func (c *Client) SessionGenerationEligible(ctx context.Context, sessionID string) (bool, error) {
	eligible, err := queryOne(ctx, c.db, `SELECT runtime_generation_eligible FROM session WHERE id = $1`, pgx.RowTo[bool], sessionID)
	return eligible, notFoundOr(err)
}

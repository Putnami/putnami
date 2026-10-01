// Package keyringstore is the DURABLE, database-backed implementation of the
// go.putnami.dev/security KeyringStore interface, built on the
// go.putnami.dev/database unit-of-work primitives.
//
// It ships as its OWN module so that go/framework/security keeps ZERO database
// imports: security defines the persistence seam (a store traffics in the
// serializable *keyring.PrivateKeyring owner document, never Go crypto types),
// and this module — which depends on BOTH security and database — provides the
// Postgres-backed adapter. The dependency direction is one-way (keyringstore →
// {security, database}); nothing in security ever imports database.
//
// # Persistence model
//
// A keyring is a set of signing keys scoped by a logical keyring_id (a tenant /
// keyring identity). Each key is one row of the signing_keys table whose
// `material` column carries the FULL keyring.PrivateJWK JSON — including the
// private fields (d, p, q, dp, dq, qi, k). This is the OWNER document: it holds
// private key material by design (the sign side needs it to reconstruct the Go
// crypto key). Encryption-at-rest of that column is a deployment concern and is
// deliberately OUT OF SCOPE here; see README.md. No method ever logs, returns in
// an error, or otherwise emits key material.
//
// The `state` COLUMN is the authoritative lifecycle state: Load and Rotate read
// and transition it, and it is stamped over the (possibly stale) state embedded
// in the material JSON on Load, so a rotation that moved a key active→retiring
// is reflected even though the material document was written earlier.
//
//	CREATE TABLE signing_keys (
//	  kid        TEXT        PRIMARY KEY,             -- JWK key id (unique)
//	  keyring_id TEXT        NOT NULL,                -- keyring / tenant scope
//	  state      TEXT        NOT NULL,                -- keyring.KeyState (authoritative)
//	  alg        TEXT        NOT NULL,                -- JWS alg, e.g. ES256/RS256
//	  kty        TEXT        NOT NULL,                -- JWK key type: EC / RSA / oct
//	  material   TEXT        NOT NULL,                -- full keyring.PrivateJWK JSON (incl. private fields)
//	  retired_at TIMESTAMPTZ,                         -- when the key entered "retiring"; NULL otherwise
//	  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
//	  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
//	);
//	CREATE INDEX signing_keys_keyring_state ON signing_keys (keyring_id, state);
package keyringstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"go.putnami.dev/database"
	"go.putnami.dev/protocol/keyring"
	"go.putnami.dev/protocol/transaction"
	"go.putnami.dev/security"
)

// DefaultTable is the signing-keys table name used when Config.Table is empty.
const DefaultTable = "signing_keys"

// DefaultKeyringID is the keyring/tenant scope used when Config.KeyringID is
// empty — the single-keyring deployment's default.
const DefaultKeyringID = "default"

// Column names of the signing_keys table. They are developer-supplied
// constants (never user input) and are validated as SQL identifiers before use.
const (
	colKid       = "kid"
	colKeyringID = "keyring_id"
	colState     = "state"
	colAlg       = "alg"
	colKty       = "kty"
	colMaterial  = "material"
	colRetiredAt = "retired_at"
)

// selectColumns is the fixed projection order scanSigningKeyRow expects. Load
// always queries with this exact column list so the scan and the SELECT never
// drift (a bare SELECT * would break the scan if the DDL column order changed).
const selectColumns = colKid + ", " + colKeyringID + ", " + colState + ", " + colAlg + ", " + colKty + ", " + colMaterial + ", " + colRetiredAt

// identifierRe matches a safe, unqualified SQL identifier (letters, digits,
// underscores). It mirrors the database package's own validator so a
// developer-supplied table name is never interpolated raw without a check.
var identifierRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validIdentifier(s string) bool { return s != "" && identifierRe.MatchString(s) }

// signingKeyRow is one signing_keys row. material is the serialized
// keyring.PrivateJWK JSON; it is intentionally an opaque blob to SQL and is
// never inspected in the database. retiredAt is nil for a non-retiring key; for a
// retiring key it is the authoritative instant the key entered its overlap window
// (stamped by Rotate), surfaced through Load so the window survives a restart.
type signingKeyRow struct {
	kid       string
	keyringID string
	state     keyring.KeyState
	alg       string
	kty       string
	material  string
	retiredAt *time.Time
}

// scanSigningKeyRow scans a row projected with selectColumns.
func scanSigningKeyRow(row pgx.Row) (signingKeyRow, error) {
	var (
		r         signingKeyRow
		state     string
		retiredAt *time.Time
	)
	if err := row.Scan(&r.kid, &r.keyringID, &state, &r.alg, &r.kty, &r.material, &retiredAt); err != nil {
		return signingKeyRow{}, err
	}
	r.state = keyring.KeyState(state)
	r.retiredAt = retiredAt
	return r, nil
}

// rowFromJWK renders the persisted row columns for one private JWK under
// keyringID. The full JWK (including private material) is marshaled into
// material; the state column defaults to active when the document omits it, so
// a minimal keyring persists a usable active key. retiredAt is left nil — it is
// derived state that Rotate stamps, not part of the owner document.
func rowFromJWK(keyringID string, jwk keyring.PrivateJWK) (signingKeyRow, error) {
	material, err := json.Marshal(jwk)
	if err != nil {
		return signingKeyRow{}, fmt.Errorf("keyringstore: marshal key %q: %w", jwk.Kid, err)
	}
	state := jwk.State
	if state == "" {
		state = keyring.KeyStateActive
	}
	return signingKeyRow{
		kid:       jwk.Kid,
		keyringID: keyringID,
		state:     state,
		alg:       jwk.Alg,
		kty:       jwk.Kty,
		material:  string(material),
	}, nil
}

// jwkFromRow reconstructs the private JWK stored as material. The row's state
// COLUMN is authoritative — a rotation may have transitioned it after the
// material JSON was written — so it is stamped over the document's embedded
// state, giving a Load that reflects the current lifecycle, not a stale one. The
// retired_at COLUMN is likewise authoritative and surfaced as PrivateJWK.RetiredAt
// (RFC 3339, UTC), so a retiring key's overlap window is measured from its real
// retirement instant and survives a process restart rather than restarting on
// every reload.
func jwkFromRow(r signingKeyRow) (keyring.PrivateJWK, error) {
	var jwk keyring.PrivateJWK
	if err := json.Unmarshal([]byte(r.material), &jwk); err != nil {
		return keyring.PrivateJWK{}, fmt.Errorf("keyringstore: unmarshal key %q: %w", r.kid, err)
	}
	jwk.State = r.state
	if r.retiredAt != nil {
		jwk.RetiredAt = r.retiredAt.UTC().Format(time.RFC3339Nano)
	} else {
		// A non-retiring row carries no instant; drop any stale value the material
		// JSON may have embedded so the column stays authoritative.
		jwk.RetiredAt = ""
	}
	return jwk, nil
}

// DBKeyringStore is the database-backed security.KeyringStore. It is safe for
// concurrent use: every write routes through the pool and, where atomicity is
// required (Save, Rotate), through a single database.WithTx transaction.
type DBKeyringStore struct {
	pool      *database.Pool
	repo      *database.Repository[signingKeyRow]
	keyringID string
	table     string // validated, unqualified identifier used in raw SQL
}

// compile-time assertions: the store implements both the persistence seam and
// the Rotator capability a scheduler policy drives.
var (
	_ security.KeyringStore = (*DBKeyringStore)(nil)
	_ Rotator               = (*DBKeyringStore)(nil)
)

// Config configures a DBKeyringStore.
type Config struct {
	// KeyringID scopes the keyring (a tenant / keyring identity). Defaults to
	// DefaultKeyringID.
	KeyringID string
	// Table is the signing-keys table name. Defaults to DefaultTable. It must be
	// a simple SQL identifier.
	Table string
}

// New builds a DBKeyringStore over pool. It fails on a nil pool or an invalid
// table identifier.
func New(pool *database.Pool, cfg Config) (*DBKeyringStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("keyringstore: nil pool")
	}
	keyringID := cfg.KeyringID
	if keyringID == "" {
		keyringID = DefaultKeyringID
	}
	table := cfg.Table
	if table == "" {
		table = DefaultTable
	}
	if !validIdentifier(table) {
		return nil, fmt.Errorf("keyringstore: invalid table name %q", table)
	}
	return &DBKeyringStore{
		pool:      pool,
		repo:      database.NewRepository[signingKeyRow](pool, table, scanSigningKeyRow),
		keyringID: keyringID,
		table:     table,
	}, nil
}

// Load implements security.KeyringStore. It selects the PUBLISHABLE
// (active/retiring) rows for the configured keyring and assembles them into a
// *keyring.PrivateKeyring. Revoked and expired keys are intentionally excluded —
// they neither sign nor publish, so a live provider must not receive them. When
// the keyring holds no publishable key it returns security.ErrNoSigningKey, so
// the fail-closed policy (refuse to mint under no configured key) applies
// uniformly across every KeyringStore backend.
func (s *DBKeyringStore) Load(ctx context.Context) (*keyring.PrivateKeyring, error) {
	query := "SELECT " + selectColumns + " FROM " + s.table +
		" WHERE " + colKeyringID + " = $1 AND " + colState + " IN ($2, $3)" +
		" ORDER BY " + colKid
	rows, err := s.repo.Query(ctx, query,
		s.keyringID, string(keyring.KeyStateActive), string(keyring.KeyStateRetiring))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, security.ErrNoSigningKey
	}
	kr := &keyring.PrivateKeyring{
		ProtocolVersion: keyring.ProtocolVersion,
		Keys:            make([]keyring.PrivateJWK, 0, len(rows)),
	}
	for _, r := range rows {
		jwk, err := jwkFromRow(r)
		if err != nil {
			return nil, err
		}
		kr.Keys = append(kr.Keys, jwk)
	}
	return kr, nil
}

// Save implements security.KeyringStore. It REPLACES the owner document for this
// keyring: every key of the document is upserted as a row, and any existing row
// for this keyring whose kid is no longer in the document is pruned — all within
// ONE transaction so the replace is atomic. Pruning is what makes Save honor the
// "replacing any previous document" contract: dropping the previous active key
// from the document (e.g. re-seeding a keyring) must not leave a stale second
// active row behind, which would make the next Load return two active keys and
// fail provider construction. The upsert deliberately does NOT overwrite
// retired_at on conflict: that column is derived state owned by Rotate, so
// re-persisting the owner document (which the store treats as authoritative for
// state but not for the retirement instant) never clobbers a key's retirement
// instant. Key material is written but never logged.
func (s *DBKeyringStore) Save(ctx context.Context, kr *keyring.PrivateKeyring) error {
	if kr == nil {
		return fmt.Errorf("keyringstore: cannot save a nil keyring")
	}
	upsert := "INSERT INTO " + s.table +
		" (" + colKid + ", " + colKeyringID + ", " + colState + ", " + colAlg + ", " + colKty + ", " + colMaterial + ", updated_at)" +
		" VALUES ($1, $2, $3, $4, $5, $6, now())" +
		" ON CONFLICT (" + colKid + ") DO UPDATE SET " +
		colKeyringID + " = EXCLUDED." + colKeyringID + ", " +
		colState + " = EXCLUDED." + colState + ", " +
		colAlg + " = EXCLUDED." + colAlg + ", " +
		colKty + " = EXCLUDED." + colKty + ", " +
		colMaterial + " = EXCLUDED." + colMaterial + ", " +
		"updated_at = now()"
	// Prune rows for this keyring whose kid is absent from the new document. An
	// empty kid array makes `kid <> ALL('{}')` true for every row, so saving an
	// empty document clears the keyring — a full replace, matching the contract.
	prune := "DELETE FROM " + s.table +
		" WHERE " + colKeyringID + " = $1 AND " + colKid + " <> ALL($2)"
	kids := make([]string, len(kr.Keys))
	for i := range kr.Keys {
		kids[i] = kr.Keys[i].Kid
	}
	return database.WithTx(ctx, s.pool, func(ctx context.Context) error {
		for i := range kr.Keys {
			row, err := rowFromJWK(s.keyringID, kr.Keys[i])
			if err != nil {
				return err
			}
			if _, err := s.pool.Exec(ctx, upsert,
				row.kid, row.keyringID, string(row.state), row.alg, row.kty, row.material); err != nil {
				return fmt.Errorf("keyringstore: upsert key %q: %w", row.kid, err)
			}
		}
		if _, err := s.pool.Exec(ctx, prune, s.keyringID, kids); err != nil {
			return fmt.Errorf("keyringstore: prune keyring %q: %w", s.keyringID, err)
		}
		return nil
	})
}

// errRollbackNonApplied is an internal sentinel used to force a WithTx rollback
// when a rotation did not apply. It is never surfaced to callers: Rotate
// translates it back into a clean (outcome, nil) result. Forcing the rollback
// is what makes the non-applied path ALL-OR-NOTHING even in the subtle case
// where the predecessor revoke applied but the successor insert then conflicted
// (a duplicate successor kid): without the rollback that leaves a retired
// predecessor and NO successor committed — a half-rotation. Rolling back
// guarantees the invariant "non-applied ⇒ the keyring is unchanged".
var errRollbackNonApplied = errors.New("keyringstore: rotation not applied (internal rollback signal)")

// Rotate atomically retires the active predecessor identified by predecessorKid
// and installs successor as the new active key, as ONE unit of work: a
// CompareAndSet transitions the predecessor's state column active→retiring, and
// only when that applies is the successor row inserted — both writes commit or
// roll back together (database.WithTx around database.Repository.Rotate).
//
// It reports the typed transaction.Outcome:
//
//   - OutcomeApplied: predecessor retired, successor installed, its retired_at
//     stamped — all committed.
//   - OutcomeNotFound: no key with predecessorKid — nothing installed.
//   - OutcomeAlreadyConsumedConflict: the predecessor was not active (already
//     rotated/retired), OR the successor kid already exists — nothing installed.
//   - OutcomeRetryableSerializationFailure: a serialization/deadlock abort.
//
// Every non-applied outcome leaves the keyring UNCHANGED (the internal rollback
// above closes the successor-conflict half-rotation hole) and returns a nil
// error — a business outcome, not a failure. A genuine I/O error rolls the unit
// back and is returned as an error. The active→retiring transition is validated
// against the closed key-state machine (keyring.CanTransition), and the
// successor must be an active key.
func (s *DBKeyringStore) Rotate(ctx context.Context, predecessorKid string, successor keyring.PrivateJWK) (transaction.Outcome, error) {
	if !keyring.CanTransition(keyring.KeyStateActive, keyring.KeyStateRetiring) {
		// Defensive: the closed transition table must permit active→retiring; if a
		// future protocol version removed it, refuse rather than write an illegal state.
		return "", fmt.Errorf("keyringstore: illegal %s→%s transition", keyring.KeyStateActive, keyring.KeyStateRetiring)
	}
	successorState := successor.State
	if successorState == "" {
		successorState = keyring.KeyStateActive
	}
	if successorState != keyring.KeyStateActive {
		return "", fmt.Errorf("keyringstore: successor key %q must be active, got %q", successor.Kid, successorState)
	}
	succ, err := rowFromJWK(s.keyringID, successor)
	if err != nil {
		return "", err
	}

	outcome := transaction.OutcomeApplied
	txErr := database.WithTx(ctx, s.pool, func(ctx context.Context) error {
		o, err := s.repo.Rotate(ctx, database.RotateSpec{
			KeyColumn:        colKid,
			PredecessorKey:   predecessorKid,
			StateColumn:      colState,
			Expected:         string(keyring.KeyStateActive),
			Revoked:          string(keyring.KeyStateRetiring),
			SuccessorColumns: []string{colKid, colKeyringID, colState, colAlg, colKty, colMaterial},
			SuccessorValues:  []any{succ.kid, succ.keyringID, string(keyring.KeyStateActive), succ.alg, succ.kty, succ.material},
		})
		if err != nil {
			return err
		}
		if o != transaction.OutcomeApplied {
			// Predecessor not revoked, or successor conflicted: surface the outcome
			// and force a rollback so nothing is left installed.
			outcome = o
			return errRollbackNonApplied
		}
		// Applied: stamp the predecessor's retirement instant in the SAME tx, so a
		// committed rotation always records when the retiring key entered its
		// overlap window.
		if _, err := s.pool.Exec(ctx,
			"UPDATE "+s.table+" SET "+colRetiredAt+" = now(), updated_at = now() WHERE "+colKid+" = $1",
			predecessorKid); err != nil {
			return fmt.Errorf("keyringstore: stamp retired_at on %q: %w", predecessorKid, err)
		}
		return nil
	})
	if txErr != nil && !errors.Is(txErr, errRollbackNonApplied) {
		return "", txErr
	}
	return outcome, nil
}

// SuccessorInstalled reports whether a Rotate outcome means the successor key
// was installed (and the predecessor retired). ONLY OutcomeApplied installs a
// successor; a conflict / not-found / retryable outcome leaves the keyring
// untouched. It is the pure, DB-free mapping the non-applied branch relies on,
// exposed so callers can classify a Rotate result without re-deriving it.
func SuccessorInstalled(o transaction.Outcome) bool {
	return o == transaction.OutcomeApplied
}

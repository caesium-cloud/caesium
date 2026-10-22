package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
	"unicode"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CLI login transactions (identity-and-access E5, design §13.1). `caesium
// login` starts a transaction with a random state and the S256 challenge of a
// verifier only it holds; a signed-in user confirms it in the browser
// (AuthorizeCLILogin), which issues a one-time code; the CLI redeems the code
// with the verifier (ExchangeCLILogin), which mints the user-bound key in the
// same transaction that consumes the code. Nothing secret is stored in
// plaintext: the state and code are keyed hashes, the challenge is already a
// hash, and no key material is stored at all — the key exists only from the
// exchange's mint to its response.

const (
	// CLILoginTransactionTTL bounds a whole login from start to exchange.
	CLILoginTransactionTTL = 5 * time.Minute
	// CLILoginCodeTTL bounds the one-time code from the confirmation click.
	CLILoginCodeTTL = 60 * time.Second
	// CLILoginMaxPendingPerIP caps the unconfirmed transactions one source IP
	// may hold, so the public start route cannot fill the table.
	CLILoginMaxPendingPerIP = 10
	// cliLoginSweepBatch bounds how many expired rows one start deletes.
	cliLoginSweepBatch = 100
	// cliLoginHostnameHintMax bounds the display-only hostname hint.
	cliLoginHostnameHintMax = 64
	// CLILoginMinPort and CLILoginMaxPort bound the loopback callback port.
	CLILoginMinPort = 1024
	CLILoginMaxPort = 65535
)

var (
	// ErrCLILoginInvalid is a malformed start request (400).
	ErrCLILoginInvalid = errors.New("invalid cli login request")
	// ErrCLILoginStateInUse is a start whose state already names a
	// transaction (409).
	ErrCLILoginStateInUse = errors.New("cli login state already in use")
	// ErrCLILoginTooMany is a start over the per-IP pending cap (429).
	ErrCLILoginTooMany = errors.New("too many pending cli logins")
	// ErrCLILoginNotFound is an unknown state (404).
	ErrCLILoginNotFound = errors.New("cli login not found")
	// ErrCLILoginExpired is a transaction past its deadline (410).
	ErrCLILoginExpired = errors.New("cli login expired")
	// ErrCLILoginConsumed is a transaction already confirmed (409).
	ErrCLILoginConsumed = errors.New("cli login already used")
	// ErrCLILoginCodeInvalid is every refused exchange: unknown, reused,
	// expired or burned code, wrong verifier, malformed input, or a user that
	// can no longer be minted for (401, uniformly).
	ErrCLILoginCodeInvalid = errors.New("invalid or expired login code")
)

// CLILoginRefusal is a refused exchange; errors.Is(err, ErrCLILoginCodeInvalid)
// holds for every one. Reason is for audit and logs only, never the response.
// Audited reports whether the refusal's audit row was already written inside
// the transaction that burned the code.
type CLILoginRefusal struct {
	Reason  string
	Audited bool
}

func (e *CLILoginRefusal) Error() string { return ErrCLILoginCodeInvalid.Error() + ": " + e.Reason }

// Is makes every refusal match ErrCLILoginCodeInvalid.
func (e *CLILoginRefusal) Is(target error) bool { return target == ErrCLILoginCodeInvalid }

// StartCLILoginRequest is a validated-by-the-service `GET /auth/cli/start`.
type StartCLILoginRequest struct {
	State     string
	Challenge string
	// Port is the loopback callback port; 0 with Manual set.
	Port         int
	Manual       bool
	HostnameHint string
	SourceIP     string
}

// AuthorizeCLILoginRequest is a signed-in user's confirmation.
type AuthorizeCLILoginRequest struct {
	State string
	// UserID and DisableGeneration are the authorising session's user as
	// validated for this request; the exchange mints with this generation.
	UserID            uuid.UUID
	DisableGeneration int64
	// SessionID is the cookie session that authenticated the confirmation.
	// It is re-checked inside the transaction, under the user-row lock the
	// user lifecycle also takes, so a session revoked after the request was
	// authenticated cannot confirm a login.
	SessionID uuid.UUID
	Email     string
	SourceIP  string
}

// AuthorizeCLILoginResult carries the one-time code, returned once.
type AuthorizeCLILoginResult struct {
	Code     string
	Mode     string
	Port     int
	RedeemBy time.Time
}

// ExchangeCLILoginRequest is a `POST /auth/cli/exchange`.
type ExchangeCLILoginRequest struct {
	Code     string
	Verifier string
	SourceIP string
}

// ExchangeCLILoginResult is the minted key (plaintext returned once) and the
// user it authenticates.
type ExchangeCLILoginResult struct {
	Key     *CreateKeyResponse
	Subject string
}

// S256Challenge is the PKCE S256 transform (RFC 7636 §4.2):
// BASE64URL-ENCODE(SHA256(ASCII(verifier))) without padding.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ValidCLILoginVerifier reports whether v is an RFC 7636 code verifier:
// 43-128 characters of [A-Za-z0-9-._~].
func ValidCLILoginVerifier(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !isAlnum(c) && c != '-' && c != '.' && c != '_' && c != '~' {
			return false
		}
	}
	return true
}

// validCLILoginToken reports whether v is a base64url token of 43-128
// characters (the CLI's state, and an S256 challenge).
func validCLILoginToken(v string, exact int) bool {
	if exact > 0 && len(v) != exact {
		return false
	}
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !isAlnum(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// randomCLILoginToken is 32 random bytes, base64url without padding (43
// characters): the one-time code.
func randomCLILoginToken() (string, error) {
	raw := make([]byte, randomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// SanitizeCLIHostnameHint keeps a display-only hint of at most 64 printable
// runes; anything else is dropped (the hint is never trusted).
func SanitizeCLIHostnameHint(hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return ""
	}
	n := 0
	for _, r := range hint {
		if !unicode.IsPrint(r) {
			return ""
		}
		n++
	}
	if n > cliLoginHostnameHintMax {
		return ""
	}
	return hint
}

func (s *Service) cliLoginHash(kind, value string) (string, error) {
	return HashKey("caesium-cli-login-"+kind+":"+value, s.keyHashSecret)
}

// cliLoginDeadline is the single effective redemption deadline of a
// transaction: its expiry, or min(expiry, authorize + CLILoginCodeTTL) once
// confirmed.
func cliLoginDeadline(txn *models.CLILoginTransaction) time.Time {
	if txn.RedeemBy != nil && txn.RedeemBy.Before(txn.ExpiresAt) {
		return *txn.RedeemBy
	}
	return txn.ExpiresAt
}

// StartCLILogin stores a new pending transaction. In one transaction it
// sweeps a batch of rows past their deadline, takes the per-IP admission lock
// (PostgreSQL advisory lock; SQLite and dqlite writers are serialised), counts
// the IP's live pending rows against CLILoginMaxPendingPerIP and inserts.
func (s *Service) StartCLILogin(ctx context.Context, req StartCLILoginRequest) (*models.CLILoginTransaction, error) {
	if !validCLILoginToken(req.State, 0) || !validCLILoginToken(req.Challenge, 43) {
		return nil, ErrCLILoginInvalid
	}
	mode := models.CLILoginModeLoopback
	switch {
	case req.Manual && req.Port == 0:
		mode = models.CLILoginModeManual
	case !req.Manual && req.Port >= CLILoginMinPort && req.Port <= CLILoginMaxPort:
	default:
		return nil, ErrCLILoginInvalid
	}
	stateHash, err := s.cliLoginHash("state", req.State)
	if err != nil {
		return nil, err
	}
	var txn *models.CLILoginTransaction
	err = s.keyTransaction(ctx, func(tx *gorm.DB) error {
		now := s.nowUTC()
		if err := sweepCLILoginsTx(tx, now); err != nil {
			return err
		}
		if err := lockCLILoginAdmissionTx(tx, req.SourceIP); err != nil {
			return err
		}
		var pending int64
		if err := tx.Model(&models.CLILoginTransaction{}).
			Where("source_ip = ? AND status = ? AND expires_at > ?", req.SourceIP, models.CLILoginPending, now).
			Count(&pending).Error; err != nil {
			return fmt.Errorf("count pending cli logins: %w", err)
		}
		if pending >= CLILoginMaxPendingPerIP {
			return ErrCLILoginTooMany
		}
		var existing int64
		if err := tx.Model(&models.CLILoginTransaction{}).Where("state_hash = ?", stateHash).Count(&existing).Error; err != nil {
			return fmt.Errorf("check cli login state: %w", err)
		}
		if existing > 0 {
			return ErrCLILoginStateInUse
		}
		row := &models.CLILoginTransaction{
			ID:           uuid.New(),
			StateHash:    stateHash,
			Challenge:    req.Challenge,
			Mode:         mode,
			Port:         req.Port,
			HostnameHint: SanitizeCLIHostnameHint(req.HostnameHint),
			SourceIP:     req.SourceIP,
			Status:       models.CLILoginPending,
			CreatedAt:    now,
			ExpiresAt:    now.Add(CLILoginTransactionTTL),
		}
		if err := tx.Create(row).Error; err != nil {
			return fmt.Errorf("store cli login: %w", err)
		}
		txn = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return txn, nil
}

// sweepCLILoginsTx deletes up to cliLoginSweepBatch rows whose redemption
// deadline has passed. A swept row minted nothing that outlives it: a key is
// minted only by a successful exchange, which the key row records.
func sweepCLILoginsTx(tx *gorm.DB, now time.Time) error {
	expired := tx.Model(&models.CLILoginTransaction{}).
		Select("id").
		Where("expires_at <= ? OR (redeem_by IS NOT NULL AND redeem_by <= ?)", now, now).
		Limit(cliLoginSweepBatch)
	if err := tx.Where("id IN (?)", expired).Delete(&models.CLILoginTransaction{}).Error; err != nil {
		return fmt.Errorf("sweep cli logins: %w", err)
	}
	return nil
}

// lockCLILoginAdmissionTx serialises the per-IP pending cap on PostgreSQL
// (transaction-scoped advisory lock keyed on the IP; a hash collision only
// serialises two IPs). SQLite and dqlite write transactions are already
// serialised, and a snapshot that cannot upgrade is retried as a whole.
func lockCLILoginAdmissionTx(tx *gorm.DB, sourceIP string) error {
	if tx == nil || tx.Dialector == nil {
		return errors.New("cli login: database dialect required")
	}
	switch tx.Name() {
	case "postgres":
		h := fnv.New64a()
		_, _ = h.Write([]byte("caesium/cli-login/" + sourceIP))
		return tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(h.Sum64())).Error //nolint:gosec // a lock key; wrapping is intended
	case "dqlite", "sqlite", "sqlite3":
		return nil
	default:
		return fmt.Errorf("cli login: unsupported dialect %q", tx.Name())
	}
}

// LookupCLILogin returns the pending transaction named by state, for the
// confirmation page: ErrCLILoginNotFound for an unknown state,
// ErrCLILoginExpired past its deadline, ErrCLILoginConsumed once confirmed.
func (s *Service) LookupCLILogin(ctx context.Context, state string) (*models.CLILoginTransaction, error) {
	if !validCLILoginToken(state, 0) {
		return nil, ErrCLILoginNotFound
	}
	stateHash, err := s.cliLoginHash("state", state)
	if err != nil {
		return nil, err
	}
	var txn models.CLILoginTransaction
	if err := s.db.WithContext(ctx).Where("state_hash = ?", stateHash).Take(&txn).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCLILoginNotFound
		}
		return nil, fmt.Errorf("lookup cli login: %w", err)
	}
	if err := pendingCLILoginState(&txn, s.nowUTC()); err != nil {
		return nil, err
	}
	return &txn, nil
}

func pendingCLILoginState(txn *models.CLILoginTransaction, now time.Time) error {
	if txn.Status != models.CLILoginPending {
		return ErrCLILoginConsumed
	}
	if !now.Before(cliLoginDeadline(txn)) {
		return ErrCLILoginExpired
	}
	return nil
}

// AuthorizeCLILogin records a signed-in user's confirmation. In one
// transaction it locks the pending row, fences on the user (enabled, at
// req.DisableGeneration), issues a one-time code (only its keyed hash is
// stored) with the deadline min(expiry, now + CLILoginCodeTTL), consumes the
// transaction with a conditional update (a second confirmation is
// ErrCLILoginConsumed) and writes the auth.cli_authorize audit row. It mints
// nothing.
func (s *Service) AuthorizeCLILogin(ctx context.Context, req AuthorizeCLILoginRequest) (*AuthorizeCLILoginResult, error) {
	if req.UserID == uuid.Nil {
		return nil, ErrUserNotFound
	}
	if !validCLILoginToken(req.State, 0) {
		return nil, ErrCLILoginNotFound
	}
	stateHash, err := s.cliLoginHash("state", req.State)
	if err != nil {
		return nil, err
	}
	var result *AuthorizeCLILoginResult
	var audit *models.AuditLog
	err = s.keyTransaction(ctx, func(tx *gorm.DB) error {
		result, audit = nil, nil
		// Lock order is user row, then login row, as in ExchangeCLILogin and
		// the user lifecycle (DisableUser updates the user, then voids its
		// logins in revokeUserCredentialsTx).
		if err := lockMintableUser(tx, req.UserID, req.DisableGeneration); err != nil {
			return err
		}
		now := s.nowUTC()
		if err := liveSessionTx(tx, req.SessionID, req.UserID, now); err != nil {
			return err
		}
		var txn models.CLILoginTransaction
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("state_hash = ?", stateHash).Take(&txn).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrCLILoginNotFound
			}
			return fmt.Errorf("lookup cli login: %w", err)
		}
		if err := pendingCLILoginState(&txn, now); err != nil {
			return err
		}
		code, err := randomCLILoginToken()
		if err != nil {
			return fmt.Errorf("generate cli login code: %w", err)
		}
		codeHash, err := s.cliLoginHash("code", code)
		if err != nil {
			return err
		}
		redeemBy := now.Add(CLILoginCodeTTL)
		if txn.ExpiresAt.Before(redeemBy) {
			redeemBy = txn.ExpiresAt
		}
		userID := req.UserID
		res := tx.Model(&models.CLILoginTransaction{}).
			Where("id = ? AND status = ? AND expires_at > ?", txn.ID, models.CLILoginPending, now).
			Updates(map[string]any{
				"status":             models.CLILoginAuthorized,
				"user_id":            userID,
				"disable_generation": req.DisableGeneration,
				"authorized_by":      req.Email,
				"authorized_at":      now,
				"code_hash":          codeHash,
				"redeem_by":          redeemBy,
			})
		if res.Error != nil {
			return fmt.Errorf("authorize cli login: %w", res.Error)
		}
		if res.RowsAffected != 1 {
			return ErrCLILoginConsumed
		}
		audit = cliLoginAuditRow(now, req.Email, ActionAuthCLIAuthorize, "cli_login", txn.ID.String(), req.SourceIP, OutcomeSuccess, map[string]any{
			"mode":          txn.Mode,
			"port":          txn.Port,
			"hostname_hint": txn.HostnameHint,
		})
		if err := tx.Create(audit).Error; err != nil {
			return fmt.Errorf("write cli login audit: %w", err)
		}
		result = &AuthorizeCLILoginResult{Code: code, Mode: txn.Mode, Port: txn.Port, RedeemBy: redeemBy}
		return nil
	})
	if err != nil {
		return nil, err
	}
	metrics.AuditLogEntriesTotal.WithLabelValues(audit.Action, audit.Outcome).Inc()
	return result, nil
}

// ExchangeCLILogin redeems a one-time code. In one transaction it locks the
// confirmed row by the code's hash, requires it unexpired, compares
// S256(verifier) with the stored challenge in constant time, and then either
// mints the user-bound key through the shared mint core with the generation
// recorded at confirmation (so a disable since then refuses the mint) and
// marks the row exchanged, or — on a wrong verifier or a refused mint — burns
// the code. The audit row rides the same transaction. Every refusal is
// ErrCLILoginCodeInvalid.
func (s *Service) ExchangeCLILogin(ctx context.Context, req ExchangeCLILoginRequest) (*ExchangeCLILoginResult, error) {
	if !validCLILoginToken(req.Code, 0) || !ValidCLILoginVerifier(req.Verifier) {
		return nil, &CLILoginRefusal{Reason: "malformed"}
	}
	codeHash, err := s.cliLoginHash("code", req.Code)
	if err != nil {
		return nil, err
	}
	computed := S256Challenge(req.Verifier)
	var result *ExchangeCLILoginResult
	var audit *models.AuditLog
	var refusal string
	err = s.keyTransaction(ctx, func(tx *gorm.DB) error {
		result, audit, refusal = nil, nil, ""
		// Lock the user row before the login row (the order authorize and the
		// user lifecycle use), then read the login row under its own lock.
		var peek models.CLILoginTransaction
		if err := tx.Select("id", "user_id").Where("code_hash = ?", codeHash).Take(&peek).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				refusal = "unknown"
				return nil
			}
			return fmt.Errorf("lookup cli login code: %w", err)
		}
		if peek.UserID != nil {
			if err := lockUserRowTx(tx, *peek.UserID); err != nil {
				return err
			}
		}
		var txn models.CLILoginTransaction
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND code_hash = ?", peek.ID, codeHash).Take(&txn).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				refusal = "unknown"
				return nil
			}
			return fmt.Errorf("lookup cli login code: %w", err)
		}
		now := s.nowUTC()
		if txn.Status != models.CLILoginAuthorized || txn.UserID == nil {
			refusal = "used"
			return nil
		}
		if !now.Before(cliLoginDeadline(&txn)) {
			refusal = "expired"
			return nil
		}
		if subtle.ConstantTimeCompare([]byte(computed), []byte(txn.Challenge)) != 1 {
			refusal = "verifier_mismatch"
			return s.burnCLILoginTx(tx, &txn, now, req.SourceIP, refusal, &audit)
		}
		description := "caesium login"
		if txn.HostnameHint != "" {
			description += " on " + txn.HostnameHint
		}
		minted, err := s.mintUserBoundKeyTx(tx, MintUserKeyRequest{
			UserID:            *txn.UserID,
			DisableGeneration: txn.DisableGeneration,
			Description:       description,
			CreatedBy:         txn.AuthorizedBy,
		})
		switch {
		case errors.Is(err, ErrUserDisabled), errors.Is(err, ErrUserChanged), errors.Is(err, ErrUserNotFound):
			refusal = "user_unavailable"
			return s.burnCLILoginTx(tx, &txn, now, req.SourceIP, refusal, &audit)
		case err != nil:
			return err
		}
		res := tx.Model(&models.CLILoginTransaction{}).
			Where("id = ? AND status = ?", txn.ID, models.CLILoginAuthorized).
			Updates(map[string]any{
				"status":       models.CLILoginExchanged,
				"key_id":       minted.Key.ID,
				"exchanged_at": now,
			})
		if res.Error != nil {
			return fmt.Errorf("exchange cli login: %w", res.Error)
		}
		if res.RowsAffected != 1 {
			return errCLILoginRaced
		}
		audit = cliLoginAuditRow(now, txn.AuthorizedBy, ActionAuthCLILogin, "api_key", minted.Key.ID.String(), req.SourceIP, OutcomeSuccess, map[string]any{
			"transaction_id": txn.ID.String(),
			"key_prefix":     minted.Key.KeyPrefix,
			"mode":           txn.Mode,
			"hostname_hint":  txn.HostnameHint,
		})
		if err := tx.Create(audit).Error; err != nil {
			return fmt.Errorf("write cli login audit: %w", err)
		}
		result = &ExchangeCLILoginResult{Key: minted, Subject: txn.AuthorizedBy}
		return nil
	})
	if errors.Is(err, errCLILoginRaced) {
		return nil, &CLILoginRefusal{Reason: "used"}
	}
	if err != nil {
		return nil, err
	}
	if audit != nil {
		metrics.AuditLogEntriesTotal.WithLabelValues(audit.Action, audit.Outcome).Inc()
	}
	if refusal != "" {
		return nil, &CLILoginRefusal{Reason: refusal, Audited: audit != nil}
	}
	return result, nil
}

// errCLILoginRaced rolls back an exchange whose row changed under it (only
// possible where row locks are not taken); the mint rolls back with it.
var errCLILoginRaced = errors.New("cli login exchanged concurrently")

// burnCLILoginTx marks a confirmed transaction failed (its code can never be
// redeemed) and writes the refused auth.cli_login audit row; the caller's
// transaction commits both.
func (s *Service) burnCLILoginTx(tx *gorm.DB, txn *models.CLILoginTransaction, now time.Time, sourceIP, reason string, audit **models.AuditLog) error {
	res := tx.Model(&models.CLILoginTransaction{}).
		Where("id = ? AND status = ?", txn.ID, models.CLILoginAuthorized).
		Updates(map[string]any{"status": models.CLILoginFailed, "exchanged_at": now})
	if res.Error != nil {
		return fmt.Errorf("burn cli login: %w", res.Error)
	}
	row := cliLoginAuditRow(now, txn.AuthorizedBy, ActionAuthCLILogin, "cli_login", txn.ID.String(), sourceIP, OutcomeDenied, map[string]any{
		"reason": reason,
		"mode":   txn.Mode,
	})
	if err := tx.Create(row).Error; err != nil {
		return fmt.Errorf("write cli login audit: %w", err)
	}
	*audit = row
	return nil
}

// cliLoginAuditRow builds an identity audit row (no namespace) written inside
// the login transaction, as dataset hold release does: a row written after
// the commit is one a crash in between could lose.
func cliLoginAuditRow(now time.Time, actor, action, resourceType, resourceID, sourceIP, outcome string, metadata map[string]any) *models.AuditLog {
	var meta []byte
	if metadata != nil {
		meta, _ = json.Marshal(metadata)
	}
	return &models.AuditLog{
		ID:           uuid.New(),
		Timestamp:    now,
		Actor:        actor,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		SourceIP:     sourceIP,
		Outcome:      outcome,
		Metadata:     meta,
	}
}

// liveSessionTx requires the session to belong to userID and be neither
// revoked nor past its absolute expiry, read inside the caller's transaction.
func liveSessionTx(tx *gorm.DB, sessionID, userID uuid.UUID, now time.Time) error {
	if sessionID == uuid.Nil {
		return ErrSessionInvalid
	}
	var sess models.Session
	err := tx.Select("id", "user_id", "revoked_at", "absolute_expires_at").Where("id = ?", sessionID).Take(&sess).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrSessionInvalid
	}
	if err != nil {
		return fmt.Errorf("lookup cli login session: %w", err)
	}
	switch {
	case sess.UserID != userID:
		return ErrSessionInvalid
	case sess.IsRevoked():
		return ErrSessionRevoked
	case !now.Before(sess.AbsoluteExpiresAt):
		return ErrSessionExpired
	}
	return nil
}

// lockUserRowTx takes the user row's lock (PostgreSQL; the SQLite and dqlite
// dialects drop the clause because their writers are serialised). A missing
// user is not an error here: the mint's own fence refuses it.
func lockUserRowTx(tx *gorm.DB, userID uuid.UUID) error {
	var user models.User
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("id = ?", userID).Take(&user).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("lock cli login user: %w", err)
	}
	return nil
}

// voidUserCLILoginsTx burns every confirmed, unredeemed CLI login of a user,
// so a code confirmed before the user's credentials were revoked can never
// mint a key afterwards (revokeUserCredentialsTx).
func voidUserCLILoginsTx(tx *gorm.DB, userID uuid.UUID, now time.Time) error {
	if err := tx.Model(&models.CLILoginTransaction{}).
		Where("user_id = ? AND status = ?", userID, models.CLILoginAuthorized).
		Updates(map[string]any{"status": models.CLILoginFailed, "exchanged_at": now}).Error; err != nil {
		return fmt.Errorf("void cli logins: %w", err)
	}
	return nil
}

// RevokeOwnUserKey revokes the user-bound key keyID of userID, the credential
// that authenticated a `POST /auth/logout`. A key already revoked (a race
// with another revocation) is not an error.
func (s *Service) RevokeOwnUserKey(ctx context.Context, keyID, userID uuid.UUID) error {
	if keyID == uuid.Nil || userID == uuid.Nil {
		return ErrKeyNotFound
	}
	return s.keyTransaction(ctx, func(tx *gorm.DB) error {
		return tx.Model(&models.APIKey{}).
			Where("id = ? AND user_id = ? AND revoked_at IS NULL", keyID, userID).
			Update("revoked_at", s.nowUTC()).Error
	})
}

package pki

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

const (
	DefaultCATTL                = 43800 * time.Hour
	DefaultLeafTTL              = 720 * time.Hour
	DefaultEnrollmentTimeout    = 2 * time.Minute
	DefaultPollInterval         = 500 * time.Millisecond
	DefaultRotationInterval     = 1 * time.Minute
	DefaultTrustRefreshInterval = 5 * time.Minute
	defaultSignBatchSize        = 32
)

// Clock lets tests make renewal and roll decisions deterministic.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type LeaderCheckFunc func(context.Context) (bool, error)

// Material is the currently usable node certificate and trust bundle.
type Material struct {
	Certificate tls.Certificate
	Leaf        *x509.Certificate
	Pool        *x509.CertPool
	CACerts     []*x509.Certificate
}

// MaterialHolder atomically publishes TLS material to server/client callbacks.
type MaterialHolder struct {
	current atomic.Pointer[Material]
}

func NewMaterialHolder() *MaterialHolder {
	return &MaterialHolder{}
}

func NewStaticMaterialHolder(caFile, certFile, keyFile string) (*MaterialHolder, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA %q: %w", caFile, err)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read cert %q: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read key %q: %w", keyFile, err)
	}
	holder := NewMaterialHolder()
	cert, err := TLSCertificate(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	pool, caCerts, err := CertPool(caPEM)
	if err != nil {
		return nil, err
	}
	holder.Set(cert, pool, caCerts)
	return holder, nil
}

func TLSCertificate(certPEM, keyPEM []byte) (tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: load keypair: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, fmt.Errorf("mtls: keypair contains no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: parse leaf certificate: %w", err)
	}
	cert.Leaf = leaf
	return cert, nil
}

func CertPool(caPEM []byte) (*x509.CertPool, []*x509.Certificate, error) {
	certs, err := ParseCertificatesPEM(caPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: parse CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool, certs, nil
}

func (h *MaterialHolder) Set(cert tls.Certificate, pool *x509.CertPool, caCerts []*x509.Certificate) {
	h.current.Store(&Material{
		Certificate: cert,
		Leaf:        cert.Leaf,
		Pool:        clonePool(pool),
		CACerts:     append([]*x509.Certificate(nil), caCerts...),
	})
}

func (h *MaterialHolder) UpdateTrust(pool *x509.CertPool, caCerts []*x509.Certificate) error {
	mat, ok := h.Material()
	if !ok {
		return fmt.Errorf("mtls: TLS material not initialized")
	}
	h.Set(mat.Certificate, pool, caCerts)
	return nil
}

func (h *MaterialHolder) Material() (*Material, bool) {
	if h == nil {
		return nil, false
	}
	mat := h.current.Load()
	if mat == nil {
		return nil, false
	}
	return mat, true
}

func (h *MaterialHolder) Certificate() (*tls.Certificate, error) {
	mat, ok := h.Material()
	if !ok {
		return nil, fmt.Errorf("mtls: TLS material not initialized")
	}
	return &mat.Certificate, nil
}

func (h *MaterialHolder) CertPool() (*x509.CertPool, error) {
	mat, ok := h.Material()
	if !ok {
		return nil, fmt.Errorf("mtls: TLS material not initialized")
	}
	return clonePool(mat.Pool), nil
}

func clonePool(pool *x509.CertPool) *x509.CertPool {
	if pool == nil {
		return x509.NewCertPool()
	}
	return pool.Clone()
}

// Config controls internal mTLS auto-provisioning.
type Config struct {
	Store                *Store
	NodeID               string
	Token                string
	CATTL                time.Duration
	LeafTTL              time.Duration
	LeafRenewBefore      time.Duration
	CARenewBefore        time.Duration
	EnrollmentTimeout    time.Duration
	PollInterval         time.Duration
	RotationInterval     time.Duration
	TrustRefreshInterval time.Duration
	SignBatchSize        int
	LeaderCheck          LeaderCheckFunc
	Clock                Clock
}

type Provisioner struct {
	store                *Store
	holder               *MaterialHolder
	keys                 DerivedKeys
	nodeID               string
	caTTL                time.Duration
	leafTTL              time.Duration
	leafRenewBefore      time.Duration
	caRenewBefore        time.Duration
	enrollmentTimeout    time.Duration
	pollInterval         time.Duration
	rotationInterval     time.Duration
	trustRefreshInterval time.Duration
	signBatchSize        int
	leaderCheck          LeaderCheckFunc
	clock                Clock
}

func NewProvisioner(cfg Config) (*Provisioner, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("pki: store is required")
	}
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("pki: node ID is required")
	}
	if cfg.LeaderCheck == nil {
		return nil, fmt.Errorf("pki: leader check is required")
	}
	keys, err := DeriveKeys(cfg.Token)
	if err != nil {
		return nil, err
	}
	caTTL := cfg.CATTL
	if caTTL <= 0 {
		caTTL = DefaultCATTL
	}
	leafTTL := cfg.LeafTTL
	if leafTTL <= 0 {
		leafTTL = DefaultLeafTTL
	}
	leafRenewBefore := cfg.LeafRenewBefore
	if leafRenewBefore <= 0 {
		leafRenewBefore = leafTTL / 3
	}
	caRenewBefore := cfg.CARenewBefore
	if caRenewBefore <= 0 {
		caRenewBefore = 30 * 24 * time.Hour
		if caRenewBefore >= caTTL {
			caRenewBefore = caTTL / 3
		}
	}
	enrollmentTimeout := cfg.EnrollmentTimeout
	if enrollmentTimeout <= 0 {
		enrollmentTimeout = DefaultEnrollmentTimeout
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	rotationInterval := cfg.RotationInterval
	if rotationInterval <= 0 {
		rotationInterval = DefaultRotationInterval
	}
	trustRefreshInterval := cfg.TrustRefreshInterval
	if trustRefreshInterval <= 0 {
		trustRefreshInterval = DefaultTrustRefreshInterval
	}
	signBatchSize := cfg.SignBatchSize
	if signBatchSize <= 0 {
		signBatchSize = defaultSignBatchSize
	}
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Provisioner{
		store:                cfg.Store,
		holder:               NewMaterialHolder(),
		keys:                 keys,
		nodeID:               cfg.NodeID,
		caTTL:                caTTL,
		leafTTL:              leafTTL,
		leafRenewBefore:      leafRenewBefore,
		caRenewBefore:        caRenewBefore,
		enrollmentTimeout:    enrollmentTimeout,
		pollInterval:         pollInterval,
		rotationInterval:     rotationInterval,
		trustRefreshInterval: trustRefreshInterval,
		signBatchSize:        signBatchSize,
		leaderCheck:          cfg.LeaderCheck,
		clock:                clock,
	}, nil
}

func (p *Provisioner) Holder() *MaterialHolder {
	return p.holder
}

// Bootstrap blocks until this node has a signed leaf and a non-empty trust
// pool. The caller should complete this before starting the internal mTLS
// listener or dispatch loop.
func (p *Provisioner) Bootstrap(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.enrollmentTimeout)
	defer cancel()

	for {
		if err := p.ensureCA(ctx); err != nil {
			return err
		}
		gen, err := p.store.NewestActiveCAGeneration(ctx, p.clock.Now())
		if err != nil {
			return err
		}
		if gen != nil {
			break
		}
		if err := sleep(ctx, p.pollInterval); err != nil {
			return fmt.Errorf("pki: wait for internal CA: %w", err)
		}
	}
	return p.Enroll(ctx)
}

// Run starts the signer/rotation loop. It returns when ctx is cancelled or a
// non-recoverable PKI operation fails.
func (p *Provisioner) Run(ctx context.Context) error {
	rotationTicker := time.NewTicker(p.rotationInterval)
	defer rotationTicker.Stop()
	trustTicker := time.NewTicker(p.trustRefreshInterval)
	defer trustTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-rotationTicker.C:
			if err := p.rotationTick(ctx); err != nil {
				log.Error("pki: rotation tick failed", "error", err)
			}
		case <-trustTicker.C:
			if err := p.RefreshTrust(ctx); err != nil {
				log.Error("pki: trust refresh failed", "error", err)
			}
		}
	}
}

func (p *Provisioner) rotationTick(ctx context.Context) error {
	leader, err := p.isLeader(ctx)
	if err != nil {
		return err
	}
	if leader {
		if err := p.RollCAIfNeeded(ctx); err != nil {
			return err
		}
		if _, err := p.SignPending(ctx, p.signBatchSize); err != nil {
			return err
		}
		if _, err := p.PruneExpiredCAs(ctx); err != nil {
			return err
		}
	}
	if err := p.RefreshTrust(ctx); err != nil {
		return err
	}
	if p.LeafRenewalDue() {
		enrollCtx, cancel := context.WithTimeout(ctx, p.enrollmentTimeout)
		defer cancel()
		if err := p.Enroll(enrollCtx); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provisioner) Enroll(ctx context.Context) error {
	gen, _, _, err := p.activeTrust(ctx)
	if err != nil {
		return err
	}
	req, err := GenerateLeafRequest(p.nodeID)
	if err != nil {
		return err
	}
	id := uuid.NewString()
	now := p.clock.Now()
	if err := p.store.CreateEnrollment(ctx, &models.InternalNodeEnrollment{
		ID:           id,
		NodeID:       p.nodeID,
		CSRPEM:       string(req.CSRPEM),
		CSRMac:       CSRMac(p.keys.CSRMac, req.CSRDER),
		CAGeneration: gen.Generation,
		Status:       EnrollmentStatusPending,
		RequestedAt:  now.UTC(),
	}); err != nil {
		return fmt.Errorf("pki: create node enrollment: %w", err)
	}
	// The enrollment row is a short-lived signing request. Delete it on every
	// exit path (success, rejection, timeout, error) using a fresh background
	// context, since the method's ctx may already be cancelled on timeout.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.store.DeleteEnrollment(cleanupCtx, id)
	}()

	for {
		if _, err := p.SignPending(ctx, p.signBatchSize); err != nil {
			return err
		}
		enrollment, err := p.store.Enrollment(ctx, id)
		if err != nil {
			return fmt.Errorf("pki: read node enrollment: %w", err)
		}
		switch enrollment.Status {
		case EnrollmentStatusSigned:
			if enrollment.CertPEM == nil || *enrollment.CertPEM == "" {
				return fmt.Errorf("pki: signed enrollment has no certificate")
			}
			_, pool, caCerts, err := p.activeTrust(ctx)
			if err != nil {
				return err
			}
			cert, err := TLSCertificate([]byte(*enrollment.CertPEM), req.KeyPEM)
			if err != nil {
				return err
			}
			p.holder.Set(cert, pool, caCerts)
			return nil
		case EnrollmentStatusRejected:
			return fmt.Errorf("pki: internal mTLS enrollment rejected; token mismatch or invalid CSR")
		case EnrollmentStatusPending:
			if err := sleep(ctx, p.pollInterval); err != nil {
				return fmt.Errorf("pki: wait for signed enrollment: %w", err)
			}
		default:
			return fmt.Errorf("pki: unknown enrollment status %q", enrollment.Status)
		}
	}
}

func (p *Provisioner) RefreshTrust(ctx context.Context) error {
	_, pool, caCerts, err := p.activeTrust(ctx)
	if err != nil {
		return err
	}
	return p.holder.UpdateTrust(pool, caCerts)
}

func (p *Provisioner) SignPending(ctx context.Context, limit int) (int, error) {
	leader, err := p.isLeader(ctx)
	if err != nil {
		return 0, err
	}
	if !leader {
		return 0, nil
	}
	gen, err := p.store.NewestActiveCAGeneration(ctx, p.clock.Now())
	if err != nil {
		return 0, err
	}
	if gen == nil {
		return 0, nil
	}
	caCert, caKey, err := p.signerForGeneration(gen)
	if err != nil {
		return 0, err
	}
	pending, err := p.store.PendingEnrollments(ctx, limit)
	if err != nil {
		return 0, err
	}
	signed := 0
	for _, enrollment := range pending {
		csr, parseErr := ParseCSRPEM([]byte(enrollment.CSRPEM))
		if parseErr != nil {
			if _, err := p.store.MarkEnrollmentRejected(ctx, enrollment.ID, p.clock.Now()); err != nil {
				return signed, err
			}
			continue
		}
		if !VerifyCSRMac(p.keys.CSRMac, csr.Raw, enrollment.CSRMac) {
			if _, err := p.store.MarkEnrollmentRejected(ctx, enrollment.ID, p.clock.Now()); err != nil {
				return signed, err
			}
			continue
		}
		certPEM, _, signErr := SignCSR([]byte(enrollment.CSRPEM), enrollment.NodeID, caCert, caKey, p.clock.Now(), p.leafTTL)
		if signErr != nil {
			if _, err := p.store.MarkEnrollmentRejected(ctx, enrollment.ID, p.clock.Now()); err != nil {
				return signed, err
			}
			continue
		}
		updated, err := p.store.MarkEnrollmentSigned(ctx, enrollment.ID, gen.Generation, string(certPEM), p.clock.Now())
		if err != nil {
			return signed, err
		}
		if updated {
			signed++
		}
	}
	return signed, nil
}

func (p *Provisioner) RollCAIfNeeded(ctx context.Context) error {
	leader, err := p.isLeader(ctx)
	if err != nil {
		return err
	}
	if !leader {
		return nil
	}
	gen, err := p.store.NewestActiveCAGeneration(ctx, p.clock.Now())
	if err != nil {
		return err
	}
	if gen != nil && !ShouldRollCA(gen, p.clock.Now(), p.caRenewBefore) {
		return nil
	}
	if latest, err := p.store.NewestActiveCAGeneration(ctx, p.clock.Now()); err != nil || latest != nil && !ShouldRollCA(latest, p.clock.Now(), p.caRenewBefore) {
		return err
	}
	next, err := p.store.MaxCAGeneration(ctx)
	if err != nil {
		return err
	}
	return p.createCAGeneration(ctx, next+1)
}

func (p *Provisioner) PruneExpiredCAs(ctx context.Context) (int64, error) {
	if p.leafTTL <= 0 {
		return 0, nil
	}
	cutoff := p.clock.Now().Add(-p.leafTTL)
	return p.store.PruneExpiredCAGenerations(ctx, cutoff)
}

func (p *Provisioner) LeafRenewalDue() bool {
	mat, ok := p.holder.Material()
	if !ok || mat.Leaf == nil {
		return true
	}
	return ShouldRenewLeaf(mat.Leaf, p.clock.Now(), p.leafRenewBefore)
}

func ShouldRenewLeaf(cert *x509.Certificate, now time.Time, renewBefore time.Duration) bool {
	if cert == nil {
		return true
	}
	return !cert.NotAfter.After(now.Add(renewBefore))
}

func ShouldRollCA(gen *models.InternalCAGeneration, now time.Time, renewBefore time.Duration) bool {
	if gen == nil {
		return true
	}
	return !gen.NotAfter.After(now.Add(renewBefore))
}

func (p *Provisioner) ensureCA(ctx context.Context) error {
	if gen, err := p.store.NewestActiveCAGeneration(ctx, p.clock.Now()); err != nil || gen != nil {
		return err
	}
	leader, err := p.isLeader(ctx)
	if err != nil {
		return err
	}
	if !leader {
		return nil
	}
	count, err := p.store.CountCAGenerations(ctx)
	if err != nil {
		return err
	}
	if count == 0 {
		return p.createCAGeneration(ctx, 1)
	}
	if gen, err := p.store.NewestActiveCAGeneration(ctx, p.clock.Now()); err != nil || gen != nil {
		return err
	}
	maxGen, err := p.store.MaxCAGeneration(ctx)
	if err != nil {
		return err
	}
	return p.createCAGeneration(ctx, maxGen+1)
}

func (p *Provisioner) createCAGeneration(ctx context.Context, generation int) error {
	if generation < 1 {
		generation = 1
	}
	gen, _, err := NewCAGeneration(generation, p.keys.CAKEK, p.clock.Now(), p.caTTL)
	if err != nil {
		return err
	}
	_, err = p.store.CreateCAGenerationIfAbsent(ctx, gen)
	return err
}

func (p *Provisioner) activeTrust(ctx context.Context) (*models.InternalCAGeneration, *x509.CertPool, []*x509.Certificate, error) {
	now := p.clock.Now()
	gens, err := p.store.ActiveCAGenerations(ctx, now)
	if err != nil {
		return nil, nil, nil, err
	}
	pool, caCerts, err := TrustPoolFromGenerations(gens, now)
	if err != nil {
		return nil, nil, nil, err
	}
	active := &gens[len(gens)-1]
	return active, pool, caCerts, nil
}

func (p *Provisioner) signerForGeneration(gen *models.InternalCAGeneration) (*x509.Certificate, crypto.Signer, error) {
	certs, err := ParseCertificatesPEM([]byte(gen.CertPEM))
	if err != nil {
		return nil, nil, fmt.Errorf("pki: parse CA generation %d certificate: %w", gen.Generation, err)
	}
	keyPEM, err := OpenCAKey(p.keys.CAKEK, gen.KeyCiphertext, gen.KeyNonce)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: decrypt CA generation %d key: %w", gen.Generation, err)
	}
	key, err := ParsePrivateKeyPEM(keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: parse CA generation %d key: %w", gen.Generation, err)
	}
	return certs[0], key, nil
}

func (p *Provisioner) isLeader(ctx context.Context) (bool, error) {
	leader, err := p.leaderCheck(ctx)
	if err != nil {
		return false, fmt.Errorf("pki: check dqlite leader: %w", err)
	}
	return leader, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

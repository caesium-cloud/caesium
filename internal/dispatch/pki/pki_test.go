package pki

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

const testToken = "0123456789abcdef0123456789abcdef"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func leader(context.Context) (bool, error)    { return true, nil }
func nonLeader(context.Context) (bool, error) { return false, nil }

func TestDeriveKeysDeterministic(t *testing.T) {
	first, err := DeriveKeys(testToken)
	require.NoError(t, err)
	second, err := DeriveKeys(testToken)
	require.NoError(t, err)
	other, err := DeriveKeys(testToken + "-other")
	require.NoError(t, err)

	require.Equal(t, first.CAKEK, second.CAKEK)
	require.Equal(t, first.CSRMac, second.CSRMac)
	require.NotEqual(t, first.CAKEK, first.CSRMac)
	require.NotEqual(t, first.CAKEK, other.CAKEK)
	require.Len(t, first.CAKEK, 32)
	require.Len(t, first.CSRMac, 32)
}

func TestSealCAKeyRoundTripAndWrongKeyFailure(t *testing.T) {
	keys, err := DeriveKeys(testToken)
	require.NoError(t, err)
	wrong, err := DeriveKeys(testToken + "-wrong")
	require.NoError(t, err)

	plaintext := []byte("ca-private-key-pem")
	ciphertext, nonce, err := SealCAKey(keys.CAKEK, plaintext)
	require.NoError(t, err)
	require.Len(t, nonce, 12)
	require.NotEqual(t, plaintext, ciphertext)

	opened, err := OpenCAKey(keys.CAKEK, ciphertext, nonce)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)

	_, err = OpenCAKey(wrong.CAKEK, ciphertext, nonce)
	require.Error(t, err)
}

func TestCAGenesisSignsVerifiableLeaf(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	keys, err := DeriveKeys(testToken)
	require.NoError(t, err)
	gen, caCert, err := NewCAGeneration(1, keys.CAKEK, now, time.Hour)
	require.NoError(t, err)
	require.True(t, caCert.IsCA)
	require.Equal(t, x509.KeyUsageCertSign|x509.KeyUsageCRLSign, caCert.KeyUsage)

	caKey := signerForTestGeneration(t, gen, keys.CAKEK)
	req, err := GenerateLeafRequest("node-a:9001")
	require.NoError(t, err)
	leafPEM, leaf, err := SignCSR(req.CSRPEM, "node-a:9001", caCert, caKey, now, 30*time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, leafPEM)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	require.NoError(t, err)
}

func TestLeafTemplateHasBothEKUsAndIsNonCA(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	_, caCert, caKey := caForTest(t, now)
	req, err := GenerateLeafRequest("127.0.0.1:9001")
	require.NoError(t, err)

	_, leaf, err := SignCSR(req.CSRPEM, "127.0.0.1:9001", caCert, caKey, now, time.Hour)
	require.NoError(t, err)
	require.False(t, leaf.IsCA)
	require.Contains(t, leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	require.Contains(t, leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	require.Equal(t, x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment, leaf.KeyUsage)
	require.Equal(t, now.Add(-leafBackdate), leaf.NotBefore)
}

func TestSignerIgnoresCSRRequestedExtensions(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	_, caCert, caKey := caForTest(t, now)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	basicConstraints, err := asn1.Marshal(struct {
		IsCA bool `asn1:"optional"`
	}{IsCA: true})
	require.NoError(t, err)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "node-b:9001"},
		ExtraExtensions: []pkix.Extension{
			{Id: []int{2, 5, 29, 19}, Critical: true, Value: basicConstraints},
		},
	}, key)
	require.NoError(t, err)
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	_, leaf, err := SignCSR(csrPEM, "node-b:9001", caCert, caKey, now, time.Hour)
	require.NoError(t, err)
	require.False(t, leaf.IsCA)
	require.Contains(t, leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	require.Contains(t, leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
}

func TestCSRMacAcceptReject(t *testing.T) {
	keys, err := DeriveKeys(testToken)
	require.NoError(t, err)
	wrong, err := DeriveKeys(testToken + "-wrong")
	require.NoError(t, err)
	req, err := GenerateLeafRequest("node-c:9001")
	require.NoError(t, err)

	mac := CSRMac(keys.CSRMac, req.CSRDER)
	require.True(t, VerifyCSRMac(keys.CSRMac, req.CSRDER, mac))
	require.False(t, VerifyCSRMac(wrong.CSRMac, req.CSRDER, mac))
	require.False(t, VerifyCSRMac(keys.CSRMac, []byte("different-csr"), mac))
}

func TestTrustPoolUnionAcrossGenerations(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	keys, err := DeriveKeys(testToken)
	require.NoError(t, err)
	oldGen, oldCA, err := NewCAGeneration(1, keys.CAKEK, now.Add(-time.Hour), 2*time.Hour)
	require.NoError(t, err)
	newGen, _, err := NewCAGeneration(2, keys.CAKEK, now, 3*time.Hour)
	require.NoError(t, err)
	req, err := GenerateLeafRequest("node-old:9001")
	require.NoError(t, err)
	_, oldLeaf, err := SignCSR(req.CSRPEM, "node-old:9001", oldCA, signerForTestGeneration(t, oldGen, keys.CAKEK), now, time.Hour)
	require.NoError(t, err)

	pool, _, err := TrustPoolFromGenerations([]models.InternalCAGeneration{*oldGen, *newGen}, now)
	require.NoError(t, err)
	_, err = oldLeaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	require.NoError(t, err)
}

func TestRenewAndRollTriggersUseInjectedClock(t *testing.T) {
	base := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	p, err := NewProvisioner(Config{
		Store:             NewStore(db),
		NodeID:            "node-renew:9001",
		Token:             testToken,
		CATTL:             30 * time.Minute,
		LeafTTL:           20 * time.Minute,
		LeafRenewBefore:   5 * time.Minute,
		CARenewBefore:     5 * time.Minute,
		EnrollmentTimeout: time.Second,
		PollInterval:      time.Millisecond,
		LeaderCheck:       leader,
		Clock:             clock,
	})
	require.NoError(t, err)
	require.NoError(t, p.Bootstrap(context.Background()))
	require.False(t, p.LeafRenewalDue())

	clock.Set(base.Add(26 * time.Minute))
	require.True(t, p.LeafRenewalDue())
	require.NoError(t, p.RollCAIfNeeded(context.Background()))
	count, err := p.store.CountCAGenerations(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}

func TestStoreEndToEndEnrollmentAndTrust(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	store := NewStore(db)

	first := newTestProvisioner(t, store, "node-1:9001", testToken, leader, newFakeClock(now))
	require.NoError(t, first.Bootstrap(context.Background()))
	second := newTestProvisioner(t, store, "node-2:9001", testToken, leader, newFakeClock(now))
	require.NoError(t, second.Bootstrap(context.Background()))

	firstMat, ok := first.Holder().Material()
	require.True(t, ok)
	secondPool, err := second.Holder().CertPool()
	require.NoError(t, err)
	_, err = firstMat.Leaf.Verify(x509.VerifyOptions{
		Roots:       secondPool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	require.NoError(t, err)
}

func TestGenesisRaceIsIdempotent(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	store := NewStore(db)
	const contenders = 8

	provisioners := make([]*Provisioner, contenders)
	for i := 0; i < contenders; i++ {
		provisioners[i] = newTestProvisioner(t, store, "node-race-"+string(rune('a'+i))+":9001", testToken, leader, newFakeClock(now))
	}
	start := make(chan struct{})
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(p *Provisioner) {
			defer wg.Done()
			<-start
			errs <- p.ensureCA(context.Background())
		}(provisioners[i])
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	count, err := store.CountCAGenerations(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

func TestTokenMismatchEnrollmentRejected(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	store := NewStore(db)

	good := newTestProvisioner(t, store, "good-node:9001", testToken, leader, newFakeClock(now))
	require.NoError(t, good.Bootstrap(context.Background()))
	bad := newTestProvisioner(t, store, "bad-node:9001", testToken+"-wrong", nonLeader, newFakeClock(now))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- bad.Enroll(ctx)
	}()

	for {
		select {
		case err := <-errCh:
			require.Error(t, err)
			require.True(t, strings.Contains(err.Error(), "rejected"))
			return
		default:
			_, err := good.SignPending(ctx, 32)
			require.NoError(t, err)
			time.Sleep(time.Millisecond)
		}
	}
}

func newTestProvisioner(t *testing.T, store *Store, nodeID, token string, leaderCheck LeaderCheckFunc, clock Clock) *Provisioner {
	t.Helper()
	p, err := NewProvisioner(Config{
		Store:             store,
		NodeID:            nodeID,
		Token:             token,
		CATTL:             time.Hour,
		LeafTTL:           30 * time.Minute,
		LeafRenewBefore:   10 * time.Minute,
		CARenewBefore:     10 * time.Minute,
		EnrollmentTimeout: time.Second,
		PollInterval:      time.Millisecond,
		LeaderCheck:       leaderCheck,
		Clock:             clock,
	})
	require.NoError(t, err)
	return p
}

func caForTest(t *testing.T, now time.Time) (*models.InternalCAGeneration, *x509.Certificate, crypto.Signer) {
	t.Helper()
	keys, err := DeriveKeys(testToken)
	require.NoError(t, err)
	gen, caCert, err := NewCAGeneration(1, keys.CAKEK, now, time.Hour)
	require.NoError(t, err)
	return gen, caCert, signerForTestGeneration(t, gen, keys.CAKEK)
}

func signerForTestGeneration(t *testing.T, gen *models.InternalCAGeneration, kek []byte) crypto.Signer {
	t.Helper()
	keyPEM, err := OpenCAKey(kek, gen.KeyCiphertext, gen.KeyNonce)
	require.NoError(t, err)
	key, err := ParsePrivateKeyPEM(keyPEM)
	require.NoError(t, err)
	return key
}

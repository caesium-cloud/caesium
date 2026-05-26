package models

import "time"

// InternalCAGeneration stores one internal mTLS CA generation. The certificate
// is public; the private key is AES-GCM sealed by the dispatch/pki package.
type InternalCAGeneration struct {
	Generation    int       `gorm:"primaryKey" json:"generation"`
	CertPEM       string    `gorm:"type:text;not null" json:"cert_pem"`
	KeyCiphertext []byte    `gorm:"type:blob;not null" json:"key_ciphertext"`
	KeyNonce      []byte    `gorm:"type:blob;not null" json:"key_nonce"`
	NotBefore     time.Time `gorm:"not null;index" json:"not_before"`
	NotAfter      time.Time `gorm:"not null;index" json:"not_after"`
	CreatedAt     time.Time `gorm:"not null" json:"created_at"`
}

// InternalNodeEnrollment is the catalog rendezvous for a node CSR and the
// leader-signed certificate produced from it.
type InternalNodeEnrollment struct {
	ID           string     `gorm:"type:text;primaryKey" json:"id"`
	NodeID       string     `gorm:"type:text;not null;index" json:"node_id"`
	CSRPEM       string     `gorm:"type:text;not null" json:"csr_pem"`
	CSRMac       []byte     `gorm:"type:blob;not null" json:"csr_mac"`
	CAGeneration int        `gorm:"not null;index" json:"ca_generation"`
	CertPEM      *string    `gorm:"type:text" json:"cert_pem,omitempty"`
	Status       string     `gorm:"type:text;not null;index" json:"status"`
	RequestedAt  time.Time  `gorm:"not null;index" json:"requested_at"`
	SignedAt     *time.Time `json:"signed_at,omitempty"`
}

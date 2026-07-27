// Package nodeenroll binds an SSH-authenticated roster node to a short-lived
// mTLS certificate issued by the gateway's internal CA.
package nodeenroll

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
)

// CertificateRecorder is the single roster mutation issuance requires.
type CertificateRecorder interface {
	RecordCertificate(name, serial string, expiresAt time.Time) error
}

// ActiveCertificateLookup is the roster allowlist queried during TLS
// authentication.
type ActiveCertificateLookup interface {
	LookupActiveCertificate(serial string, at time.Time) (nodes.Node, bool)
}

// Roster is satisfied by *nodes.Store.
type Roster interface {
	CertificateRecorder
	ActiveCertificateLookup
}

var _ Roster = (*nodes.Store)(nil)

// Issuer signs node leaves and records the one serial currently accepted for
// each approved roster name.
type Issuer struct {
	authority       *nodecert.CA
	roster          CertificateRecorder
	caCertificate   []byte
	gatewayIdentity string
	ttl             time.Duration
}

// New constructs an issuer. ttl defaults to nodecert.DefaultTTL and is capped
// at nodecert.MaxTTL even though the CA enforces the same ceiling itself.
func New(authority *nodecert.CA, roster CertificateRecorder, clusterID string, ttl time.Duration) (*Issuer, error) {
	if authority == nil || authority.Certificate() == nil {
		return nil, errors.New("nodeenroll: certificate authority is required")
	}
	if roster == nil {
		return nil, errors.New("nodeenroll: roster is required")
	}
	gatewayIdentity, err := nodecert.Identity(nodecert.RoleGateway, clusterID)
	if err != nil {
		return nil, fmt.Errorf("nodeenroll: gateway identity: %w", err)
	}
	if ttl <= 0 {
		ttl = nodecert.DefaultTTL
	}
	if ttl > nodecert.MaxTTL {
		ttl = nodecert.MaxTTL
	}
	caCertificate := pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: authority.Certificate().Raw,
	})
	return &Issuer{
		authority: authority, roster: roster, caCertificate: caCertificate,
		gatewayIdentity: gatewayIdentity.String(), ttl: ttl,
	}, nil
}

// Issue is directly assignable to nodelink.Hooks.OnCertificateEnroll.
// authenticatedNode must come from the approved SSH link; the request has no
// name field and the CSR's subject/SANs are ignored by nodecert.CA.
func (i *Issuer) Issue(
	ctx context.Context,
	authenticatedNode string,
	request nodelink.CertificateEnrollRequest,
) (nodelink.CertificateEnrollResponse, error) {
	if i == nil || i.authority == nil || i.roster == nil {
		return nodelink.CertificateEnrollResponse{}, &ctlops.Error{
			Kind: ctlops.KindDisabled, Op: nodelink.TypeCertificateEnroll,
			Code: nodelink.CodeNoCertificateIssuer, Verbatim: true,
			Msg: "this gateway does not issue node control certificates.",
		}
	}
	if _, err := nodecert.Identity(nodecert.RoleNode, authenticatedNode); err != nil {
		return nodelink.CertificateEnrollResponse{}, ctlops.Invalid(
			nodelink.TypeCertificateEnroll, "bad_authenticated_node",
			"the authenticated node name cannot be used as a certificate identity")
	}
	if len(request.CSRPEM) == 0 || len(request.CSRPEM) > nodelink.MaxCSRPEMBytes {
		return nodelink.CertificateEnrollResponse{}, ctlops.Invalid(
			nodelink.TypeCertificateEnroll, "bad_certificate_request",
			"the certificate request contains an invalid CSR")
	}
	if err := ctx.Err(); err != nil {
		return nodelink.CertificateEnrollResponse{}, err
	}

	certificate, serial, expiresAt, err := i.authority.SignCSR(
		request.CSRPEM,
		nodecert.Peer{Role: nodecert.RoleNode, Name: authenticatedNode},
		i.ttl,
	)
	if err != nil {
		return nodelink.CertificateEnrollResponse{}, &ctlops.Error{
			Kind: ctlops.KindInvalid, Op: nodelink.TypeCertificateEnroll,
			Code: "bad_certificate_csr", Verbatim: true,
			Msg: "the node CSR is malformed or has an invalid signature",
			Err: err,
		}
	}
	if err := ctx.Err(); err != nil {
		return nodelink.CertificateEnrollResponse{}, err
	}
	// The roster write is intentionally after successful signing. Its own
	// transaction re-checks approval and atomically replaces the prior serial;
	// a pending/disabled race therefore discloses no usable certificate.
	if err := i.roster.RecordCertificate(authenticatedNode, serial, expiresAt); err != nil {
		return nodelink.CertificateEnrollResponse{}, rosterError(err)
	}
	return nodelink.CertificateEnrollResponse{
		CertificatePEM:   append([]byte(nil), certificate...),
		CACertificatePEM: append([]byte(nil), i.caCertificate...),
		GatewayIdentity:  i.gatewayIdentity,
		Serial:           serial,
		ExpiresAt:        expiresAt,
	}, nil
}

func rosterError(err error) error {
	switch {
	case errors.Is(err, nodes.ErrNodeNotApproved), errors.Is(err, nodes.ErrNoSuchNode):
		return &ctlops.Error{
			Kind: ctlops.KindDenied, Op: nodelink.TypeCertificateEnroll,
			Code: nodelink.CodeCertificateNotApproved, Verbatim: true,
			Msg: "this node is not approved to receive a control certificate",
			Err: err,
		}
	default:
		return fmt.Errorf("record node certificate: %w", err)
	}
}

// RevocationCallback returns the fail-closed callback consumed by
// nodecert.ClientTLSConfig and nodecert.ServerTLSConfig. A serial is revoked
// unless it is the roster row's current, unexpired, approved certificate.
func RevocationCallback(roster ActiveCertificateLookup, now func() time.Time) nodecert.Revoked {
	if now == nil {
		now = time.Now
	}
	return func(serial string) bool {
		if roster == nil {
			return true
		}
		_, active := roster.LookupActiveCertificate(serial, now().UTC())
		return !active
	}
}

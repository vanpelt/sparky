package nodelink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

var ErrBadCertificateResponse = errors.New("nodelink: malformed certificate enrollment response")

// registerCertificateEnroll installs the CSR request that travels from an
// approved node to its gateway. node is the authenticated roster name closed
// over by the gateway; there is no caller-controlled identity in the body.
func registerCertificateEnroll(conn *Conn, node string, hooks Hooks) {
	conn.Handle(TypeCertificateEnroll, func(ctx context.Context, raw json.RawMessage) (any, error) {
		request, err := certificateEnrollRequest(raw)
		if err != nil {
			return nil, err
		}
		if hooks.OnCertificateEnroll == nil {
			return nil, &ctlops.Error{
				Kind: ctlops.KindDisabled, Op: TypeCertificateEnroll,
				Code: CodeNoCertificateIssuer, Verbatim: true,
				Msg: "this gateway does not issue node control certificates.",
			}
		}
		response, err := hooks.OnCertificateEnroll(ctx, node, request)
		if err != nil {
			return nil, err
		}
		if err := validateCertificateEnrollResponse(response); err != nil {
			return nil, &ctlops.Error{
				Kind: ctlops.KindInternal, Op: TypeCertificateEnroll,
				Code: "bad_certificate_response", Verbatim: true,
				Msg: "the gateway certificate issuer produced an invalid response",
				Err: err,
			}
		}
		return response, nil
	})
}

func certificateEnrollRequest(raw json.RawMessage) (CertificateEnrollRequest, error) {
	var request CertificateEnrollRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return CertificateEnrollRequest{}, ctlops.Invalid(TypeCertificateEnroll, "bad_certificate_request",
			"that certificate request could not be read: %v", err)
	}
	if len(request.CSRPEM) == 0 {
		return CertificateEnrollRequest{}, ctlops.Invalid(TypeCertificateEnroll, "bad_certificate_request",
			"a certificate request must contain a CSR")
	}
	if len(request.CSRPEM) > MaxCSRPEMBytes {
		return CertificateEnrollRequest{}, ctlops.Invalid(TypeCertificateEnroll, "certificate_request_too_large",
			"that certificate CSR is %d bytes; the limit is %d", len(request.CSRPEM), MaxCSRPEMBytes)
	}
	return request, nil
}

// EnrollCertificate sends a bounded CSR over the current approved SSH control
// link and validates the response envelope. nodepki performs the cryptographic
// chain, private-key, and leaf-identity checks before persistence.
func (u *Uplink) EnrollCertificate(ctx context.Context, csrPEM []byte) (CertificateEnrollResponse, error) {
	if len(csrPEM) == 0 {
		return CertificateEnrollResponse{}, ctlops.Invalid(TypeCertificateEnroll, "bad_certificate_request",
			"a certificate request must contain a CSR")
	}
	if len(csrPEM) > MaxCSRPEMBytes {
		return CertificateEnrollResponse{}, ctlops.Invalid(TypeCertificateEnroll, "certificate_request_too_large",
			"that certificate CSR is %d bytes; the limit is %d", len(csrPEM), MaxCSRPEMBytes)
	}
	var response CertificateEnrollResponse
	if err := u.Request(ctx, TypeCertificateEnroll, CertificateEnrollRequest{CSRPEM: csrPEM}, &response); err != nil {
		return CertificateEnrollResponse{}, err
	}
	if err := validateCertificateEnrollResponse(response); err != nil {
		return CertificateEnrollResponse{}, err
	}
	response.CertificatePEM = append([]byte(nil), response.CertificatePEM...)
	response.CACertificatePEM = append([]byte(nil), response.CACertificatePEM...)
	return response, nil
}

func validateCertificateEnrollResponse(response CertificateEnrollResponse) error {
	switch {
	case len(response.CertificatePEM) == 0:
		return fmt.Errorf("%w: empty leaf certificate", ErrBadCertificateResponse)
	case len(response.CertificatePEM) > MaxCertificatePEMBytes:
		return fmt.Errorf("%w: leaf certificate exceeds %d bytes", ErrBadCertificateResponse, MaxCertificatePEMBytes)
	case len(response.CACertificatePEM) == 0:
		return fmt.Errorf("%w: empty CA certificate", ErrBadCertificateResponse)
	case len(response.CACertificatePEM) > MaxCertificatePEMBytes:
		return fmt.Errorf("%w: CA certificate exceeds %d bytes", ErrBadCertificateResponse, MaxCertificatePEMBytes)
	case response.Serial == "" || len(response.Serial) > MaxCertificateSerialBytes ||
		strings.TrimSpace(response.Serial) != response.Serial:
		return fmt.Errorf("%w: invalid certificate serial", ErrBadCertificateResponse)
	case response.ExpiresAt.IsZero():
		return fmt.Errorf("%w: empty certificate expiry", ErrBadCertificateResponse)
	case len(response.GatewayIdentity) == 0 || len(response.GatewayIdentity) > MaxGatewayIdentityBytes:
		return fmt.Errorf("%w: invalid gateway identity", ErrBadCertificateResponse)
	case len(response.GatewayGRPCAddr) > MaxGatewayGRPCAddrBytes:
		return fmt.Errorf("%w: gateway gRPC address exceeds %d bytes", ErrBadCertificateResponse, MaxGatewayGRPCAddrBytes)
	}
	identity, err := url.Parse(response.GatewayIdentity)
	if err != nil || identity.Scheme != "spiffe" || identity.Host != "sparkbox" ||
		identity.User != nil || identity.Opaque != "" ||
		identity.RawQuery != "" || identity.Fragment != "" {
		return fmt.Errorf("%w: invalid gateway identity", ErrBadCertificateResponse)
	}
	segments := strings.Split(strings.TrimPrefix(identity.EscapedPath(), "/"), "/")
	if len(segments) != 2 || segments[0] != "gateway" || segments[1] == "" {
		return fmt.Errorf("%w: invalid gateway identity", ErrBadCertificateResponse)
	}
	if response.GatewayGRPCAddr != "" {
		host, portText, err := net.SplitHostPort(response.GatewayGRPCAddr)
		if err != nil || strings.TrimSpace(host) == "" ||
			strings.TrimSpace(response.GatewayGRPCAddr) != response.GatewayGRPCAddr {
			return fmt.Errorf("%w: invalid gateway gRPC address", ErrBadCertificateResponse)
		}
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || port == 0 ||
			net.JoinHostPort(host, strconv.FormatUint(port, 10)) != response.GatewayGRPCAddr {
			return fmt.Errorf("%w: invalid gateway gRPC address", ErrBadCertificateResponse)
		}
	}
	return nil
}

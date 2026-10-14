package cdn

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Lenart12/sloflixfs/internal/logx"
)

// expiredOK is the only domain whose expired certificates are accepted: some DoodStream video servers
// still serve files under a *.cloudatacdn.com certificate that expired on 2026-08-01.
const expiredOK = ".cloudatacdn.com"

var expiredSeen sync.Map // host -> struct{}, to log each accepted expired certificate once

// verifyCert verifies the server's chain and hostname against roots (nil: the system's). For hosts under
// expiredOK, an expired certificate is accepted if it verifies as of its own expiry: same chain and
// hostname checks, only the expiry is waived.
func verifyCert(cs tls.ConnectionState, roots *x509.CertPool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("tls: no server certificate")
	}
	// ServerName is the SNI name, empty for an IP address, and an empty DNSName skips the hostname check.
	// Video servers have names, so refuse rather than accept any trusted certificate.
	if cs.ServerName == "" {
		return errors.New("tls: no server name to verify (an IP address?)")
	}
	leaf := cs.PeerCertificates[0]
	opts := x509.VerifyOptions{DNSName: cs.ServerName, Roots: roots, Intermediates: x509.NewCertPool()}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := leaf.Verify(opts)
	var invalid x509.CertificateInvalidError
	if err == nil || !strings.HasSuffix(cs.ServerName, expiredOK) || !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		return err
	}
	opts.CurrentTime = leaf.NotAfter
	if _, err := leaf.Verify(opts); err != nil {
		return err
	}
	if _, seen := expiredSeen.LoadOrStore(cs.ServerName, struct{}{}); !seen {
		logx.Infof("cdn: accepting expired certificate of %s (expired %s)", cs.ServerName, leaf.NotAfter.Format(time.DateOnly))
	}
	return nil
}

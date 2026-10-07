/*
Copyright 2026 The kubepkg Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package admission

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Certs keeps the webhook's serving certificate: a CA and a certificate
// for the webhook Service, in a Secret all replicas share, written to the
// directory the webhook server reads and given to the webhook
// configuration as its CA bundle. kubepkg issues them itself, so it does
// not depend on a certificate manager it might be the one installing.
type Certs struct {
	Client client.Client
	// Namespace and Service name the webhook Service; Secret holds the
	// certificates; Dir is the webhook server's certificate directory;
	// Webhook names the MutatingWebhookConfiguration.
	Namespace, Service, Secret, Dir, Webhook string
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

const renewBefore = 30 * 24 * time.Hour

func (c *Certs) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Ensure makes sure a valid certificate exists, is written to Dir and is
// trusted by the webhook configuration.
func (c *Certs) Ensure(ctx context.Context) error {
	sec := &corev1.Secret{}
	err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Secret}, sec)
	switch {
	case apierrors.IsNotFound(err):
		sec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: c.Secret}, Type: corev1.SecretTypeOpaque}
		if sec.Data, err = c.issue(); err != nil {
			return err
		}
		if err := c.Client.Create(ctx, sec); apierrors.IsAlreadyExists(err) {
			// Another replica was first; use what it made.
			if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Secret}, sec); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	case err != nil:
		return err
	case !c.valid(sec.Data):
		if sec.Data, err = c.issue(); err != nil {
			return err
		}
		if err := c.Client.Update(ctx, sec); err != nil {
			return err
		}
	}
	if err := c.write(sec.Data); err != nil {
		return err
	}
	return c.trust(ctx, sec.Data["ca.crt"])
}

// Start re-checks the certificate twice a day; it runs on every replica.
func (c *Certs) Start(ctx context.Context) error {
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := c.Ensure(ctx); err != nil {
				log.FromContext(ctx).Error(err, "renew the webhook certificate")
			}
		}
	}
}

// NeedLeaderElection is false: every replica serves the webhook.
func (c *Certs) NeedLeaderElection() bool { return false }

func (c *Certs) dnsNames() []string {
	return []string{c.Service + "." + c.Namespace + ".svc", c.Service + "." + c.Namespace + ".svc.cluster.local"}
}

// valid reports whether data holds a certificate for the Service that
// lasts beyond the renewal margin.
func (c *Certs) valid(data map[string][]byte) bool {
	block, _ := pem.Decode(data["tls.crt"])
	if block == nil || len(data["tls.key"]) == 0 || len(data["ca.crt"]) == 0 {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || c.now().Add(renewBefore).After(cert.NotAfter) {
		return false
	}
	for _, n := range c.dnsNames() {
		if cert.VerifyHostname(n) != nil {
			return false
		}
	}
	return true
}

func (c *Certs) issue() (map[string][]byte, error) {
	now := c.now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	ca := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "kubepkg webhook CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leaf := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: c.dnsNames()[0]}, DNSNames: c.dnsNames(),
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"ca.crt":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

// write puts the certificate where the webhook server reads it; the
// server notices the change.
func (c *Certs) write(data map[string][]byte) error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	for _, f := range []string{"tls.crt", "tls.key"} {
		tmp := filepath.Join(c.Dir, "."+f)
		if err := os.WriteFile(tmp, data[f], 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(c.Dir, f)); err != nil {
			return err
		}
	}
	return nil
}

// trust gives the webhook configuration the CA bundle.
func (c *Certs) trust(ctx context.Context, ca []byte) error {
	if c.Webhook == "" {
		return nil
	}
	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := c.Client.Get(ctx, client.ObjectKey{Name: c.Webhook}, cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("webhook configuration %s is missing", c.Webhook)
		}
		return err
	}
	patch := client.MergeFrom(cfg.DeepCopy())
	changed := false
	for i := range cfg.Webhooks {
		if !bytes.Equal(cfg.Webhooks[i].ClientConfig.CABundle, ca) {
			cfg.Webhooks[i].ClientConfig.CABundle, changed = ca, true
		}
	}
	if !changed {
		return nil
	}
	return c.Client.Patch(ctx, cfg, patch)
}

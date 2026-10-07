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

package build

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"

	"helm.sh/helm/v4/pkg/provenance"

	"github.com/tym83/kubepkg/pkg/images"
	"github.com/tym83/kubepkg/pkg/source"
)

// ImageVerifier checks that image carries the signature rule describes.
type ImageVerifier func(ctx context.Context, image string, rule ImageSignature, recipeDir string) error

// CosignVerifier checks signatures with the cosign executable.
func CosignVerifier(binary string) ImageVerifier {
	return func(ctx context.Context, image string, rule ImageSignature, recipeDir string) error {
		args := []string{"verify", "--output", "json"}
		if rule.SignatureDigest != "" {
			args = append(args, "--signature-digest-algorithm", rule.SignatureDigest)
		}
		if rule.IgnoreTransparencyLog {
			args = append(args, "--insecure-ignore-tlog")
		}
		if rule.Key != "" {
			key, err := within(recipeDir, rule.Key)
			if err != nil {
				return err
			}
			args = append(args, "--key", key)
		} else {
			args = append(args, "--certificate-identity", rule.Identity, "--certificate-oidc-issuer", rule.Issuer)
		}
		cmd := exec.CommandContext(ctx, binary, append(args, image)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			return fmt.Errorf("%s is not signed as the recipe says: %s", image, lines[len(lines)-1])
		}
		return nil
	}
}

// checkRule makes sure a rule names one way to verify.
func checkRule(r ImageSignature) error {
	if len(r.Repositories) == 0 {
		return errors.New("an image signature rule names no repositories")
	}
	keyless := r.Identity != "" || r.Issuer != ""
	switch {
	case r.Key != "" && keyless:
		return fmt.Errorf("rule for %s: give a key or a keyless identity, not both", strings.Join(r.Repositories, ", "))
	case r.Key == "" && (r.Identity == "" || r.Issuer == ""):
		return fmt.Errorf("rule for %s: give a key, or both identity and issuer", strings.Join(r.Repositories, ", "))
	case r.IgnoreTransparencyLog && r.Key == "":
		// A keyless certificate is short-lived; only the log proves it
		// was valid when the image was signed.
		return fmt.Errorf("rule for %s: a keyless signature cannot be checked without the transparency log", strings.Join(r.Repositories, ", "))
	}
	switch r.SignatureDigest {
	case "", "sha256", "sha384", "sha512":
	default:
		return fmt.Errorf("rule for %s: signatureDigest %q is not sha256, sha384 or sha512", strings.Join(r.Repositories, ", "), r.SignatureDigest)
	}
	return nil
}

// ruleFor returns the first rule covering an image's repository.
func ruleFor(rules []ImageSignature, image string) (ImageSignature, bool) {
	repo, _, _ := images.Split(image)
	for _, r := range rules {
		for _, pattern := range r.Repositories {
			if ok, _ := path.Match(pattern, repo); ok {
				return r, true
			}
		}
	}
	return ImageSignature{}, false
}

// VerifyImages checks the signatures of the recipe's pinned images.
// It returns the images no rule covers; an image a rule covers but that
// is not signed that way is an error.
func VerifyImages(ctx context.Context, r *Recipe, dir string, verify ImageVerifier) (unchecked []string, err error) {
	if r.Spec.Verify == nil || len(r.Spec.Verify.Images) == 0 {
		return nil, nil
	}
	for _, rule := range r.Spec.Verify.Images {
		if err := checkRule(rule); err != nil {
			return nil, err
		}
	}
	if verify == nil {
		return nil, errors.New("the recipe asks to verify image signatures, and nothing here can: install cosign")
	}
	for _, img := range r.Spec.Package.Images {
		rule, ok := ruleFor(r.Spec.Verify.Images, img)
		if !ok {
			unchecked = append(unchecked, img)
			continue
		}
		if err := verify(ctx, img, rule, dir); err != nil {
			return nil, err
		}
	}
	return unchecked, nil
}

// HasImageRules reports whether a recipe asks to verify image signatures.
func HasImageRules(r *Recipe) bool { return r.Spec.Verify != nil && len(r.Spec.Verify.Images) > 0 }

// VerifyCharts checks the chart sources the recipe names against their
// upstream's provenance files: the signature, by a key in the keyring, and
// the archive's hash in the signed message.
func VerifyCharts(ctx context.Context, r *Recipe, dir string, f *source.Fetcher) error {
	if r.Spec.Verify == nil {
		return nil
	}
	for _, rule := range r.Spec.Verify.Charts {
		if rule.Keyring == "" || len(rule.Sources) == 0 {
			return errors.New("a chart signature rule needs sources and a keyring")
		}
		keyring, err := within(dir, rule.Keyring)
		if err != nil {
			return err
		}
		signatory, err := provenance.NewFromKeyring(keyring, "")
		if err != nil {
			return fmt.Errorf("keyring %s: %w", rule.Keyring, err)
		}
		for _, name := range rule.Sources {
			src, ok := r.Spec.Sources[name]
			if !ok || src.Chart == nil {
				return fmt.Errorf("chart signature rule: %s is not a chart source", name)
			}
			c := source.Chart{Repository: src.Chart.Repository, Name: src.Chart.Name, Version: src.Chart.Version, Digest: src.Chart.Digest}
			archive, _, err := f.ChartArchive(ctx, c)
			if err != nil {
				return err
			}
			prov, filename, err := f.ChartProvenance(ctx, c)
			if err != nil {
				return err
			}
			if _, err := signatory.Verify(archive, prov, filename); err != nil {
				return fmt.Errorf("chart %s %s is not signed by the keyring %s: %w", c.Name, c.Version, rule.Keyring, err)
			}
		}
	}
	return nil
}

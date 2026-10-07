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

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tym83/kubepkg/pkg/images"
	"github.com/tym83/kubepkg/pkg/sbom"
	"github.com/tym83/kubepkg/pkg/source"
)

// severities in the order reports show them.
var severities = []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"}

// Scanner reports the vulnerabilities of an image by severity.
type Scanner func(ctx context.Context, image string) (map[string]int, error)

// TrivyScanner runs trivy image on each image.
func TrivyScanner(binary string, extra []string) Scanner {
	return func(ctx context.Context, image string) (map[string]int, error) {
		args := append([]string{"image", "--quiet", "--format", "json", "--scanners", "vuln"}, extra...)
		out, err := exec.CommandContext(ctx, binary, append(args, image)...).Output()
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return nil, fmt.Errorf("trivy %s: %s", image, strings.TrimSpace(string(ee.Stderr)))
			}
			return nil, fmt.Errorf("trivy: %w", err)
		}
		var report struct {
			Results []struct {
				Vulnerabilities []struct {
					VulnerabilityID string
					Severity        string
				}
			}
		}
		if err := json.Unmarshal(out, &report); err != nil {
			return nil, fmt.Errorf("trivy %s: %w", image, err)
		}
		counts := map[string]int{}
		seen := map[string]bool{}
		for _, r := range report.Results {
			for _, v := range r.Vulnerabilities {
				if seen[v.VulnerabilityID] {
					continue
				}
				seen[v.VulnerabilityID] = true
				counts[v.Severity]++
			}
		}
		return counts, nil
	}
}

// ScanResult is one image of one package.
type ScanResult struct {
	Package, Image string
	Counts         map[string]int
}

// Scan scans every image the packages pin, each once. Packages that pin
// no images are returned in unpinned.
func Scan(ctx context.Context, pkgs []sbom.Package, scan Scanner, mirror string) (results []ScanResult, unpinned []string, err error) {
	done := map[string]map[string]int{}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })
	for _, p := range pkgs {
		if len(p.Spec.Images) == 0 {
			if hasCharts(p) {
				unpinned = append(unpinned, p.Name)
			}
			continue
		}
		for _, img := range p.Spec.Images {
			target := img
			if mirror != "" {
				repo, tag, digest := images.Split(img)
				target = strings.TrimPrefix(strings.TrimSuffix(mirror, "/"), "oci://") + "/" + source.MirrorPath(repo)
				if tag != "" {
					target += ":" + tag
				}
				target += "@" + digest
			}
			counts, ok := done[target]
			if !ok {
				if counts, err = scan(ctx, target); err != nil {
					return nil, nil, err
				}
				done[target] = counts
			}
			results = append(results, ScanResult{Package: p.Name, Image: img, Counts: counts})
		}
	}
	return results, unpinned, nil
}

func hasCharts(p sbom.Package) bool {
	for _, v := range p.Spec.Variants {
		if len(v.Components) > 0 {
			return true
		}
	}
	return false
}

func printScan(w io.Writer, results []ScanResult, unpinned []string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprint(tw, "PACKAGE\tIMAGE")
	for _, s := range severities {
		fmt.Fprintf(tw, "\t%s", s)
	}
	fmt.Fprintln(tw)
	for _, r := range results {
		repo, tag, _ := images.Split(r.Image)
		fmt.Fprintf(tw, "%s\t%s:%s", r.Package, repo, tag)
		for _, s := range severities {
			fmt.Fprintf(tw, "\t%d", r.Counts[s])
		}
		fmt.Fprintln(tw)
	}
	_ = tw.Flush()
	if len(unpinned) > 0 {
		fmt.Fprintf(w, "\nnot scanned, their images are not pinned: %s\n", strings.Join(unpinned, ", "))
	}
}

func scanCmd(cl *cluster) *cobra.Command {
	var (
		variant, trivy, failOn, mirror string
		repos, trivyArgs               []string
		trust                          trustFlags
		installed                      bool
	)
	cmd := &cobra.Command{
		Use:   "scan (<package>[@constraint]... | --cluster)",
		Short: "Report the known vulnerabilities of the images packages run",
		Long: `Scan runs trivy on every container image the packages pin, by digest,
and reports the vulnerabilities by severity, per package and image. Like
sbom, it resolves packages and their requirements from the repositories,
or with --cluster scans what the cluster runs. With --mirror it scans the
copies in an air-gapped mirror; trivy then needs its database available
offline. --fail-on makes it exit non-zero at a severity, for CI.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if _, err := exec.LookPath(trivy); err != nil {
				return fmt.Errorf("scan needs trivy (https://trivy.dev); install it or give --trivy: %w", err)
			}
			var pkgs []sbom.Package
			var err error
			switch {
			case installed && len(args) > 0:
				return errors.New("give packages or --cluster, not both")
			case installed:
				pkgs, err = clusterPackages(ctx, cl)
			case len(args) == 0:
				return errors.New("give packages, or --cluster for what the cluster runs")
			default:
				pkgs, err = resolvedPackages(ctx, cl, repos, trust, args, variant, cmd.ErrOrStderr())
			}
			if err != nil {
				return err
			}
			results, unpinned, err := Scan(ctx, pkgs, TrivyScanner(trivy, trivyArgs), mirror)
			if err != nil {
				return err
			}
			printScan(cmd.OutOrStdout(), results, unpinned)
			return failAt(results, failOn)
		},
	}
	cmd.Flags().StringArrayVar(&repos, "repo", nil, "repository index URL, highest priority first (repeatable; default: the cluster's repositories)")
	cmd.Flags().StringVar(&variant, "variant", "", "variant whose requirements are resolved (default: default)")
	cmd.Flags().BoolVar(&installed, "cluster", false, "scan the packages the cluster runs")
	cmd.Flags().StringVar(&trivy, "trivy", "trivy", "trivy executable")
	cmd.Flags().StringArrayVar(&trivyArgs, "trivy-arg", nil, "extra argument for trivy image, e.g. --skip-db-update (repeatable)")
	cmd.Flags().StringVar(&mirror, "mirror", "", "oci:// mirror to scan the copies in instead of the original registries")
	cmd.Flags().StringVar(&failOn, "fail-on", "", "exit non-zero when an image has a vulnerability of this severity or worse: CRITICAL, HIGH, MEDIUM, LOW")
	trust.bind(cmd)
	return cmd
}

// failAt returns an error when any image has a vulnerability at severity
// or above.
func failAt(results []ScanResult, severity string) error {
	if severity == "" {
		return nil
	}
	severity = strings.ToUpper(severity)
	limit := -1
	for i, s := range severities[:4] {
		if s == severity {
			limit = i
		}
	}
	if limit < 0 {
		return fmt.Errorf("--fail-on %q: one of CRITICAL, HIGH, MEDIUM, LOW", severity)
	}
	for _, r := range results {
		for _, s := range severities[:limit+1] {
			if r.Counts[s] > 0 {
				return fmt.Errorf("%s runs %s with %s vulnerabilities", r.Package, r.Image, strings.ToLower(s))
			}
		}
	}
	return nil
}

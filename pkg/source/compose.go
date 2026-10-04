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

package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// ChartSpec says how to build one component's chart from a tree.
type ChartSpec struct {
	// Path of the chart inside the tree, relative to the base path.
	Path string
	// Libraries maps a library name to its path inside the tree; each is
	// placed under the chart's charts/<name>.
	Libraries map[string]string
	// ValuesFiles are file names inside the chart. The first replaces
	// values.yaml, the rest are merged over it in order.
	ValuesFiles []string
}

// Compose builds a chart directory under dst from a tree and returns the chart directory and a digest of
// its content. The digest changes exactly when the chart Helm would load
// changes, so the operator can tell a real upgrade from a no-op.
func Compose(tree, base string, spec ChartSpec, dst string) (string, string, error) {
	src, err := within(tree, path.Join(base, spec.Path))
	if err != nil {
		return "", "", err
	}
	if _, err := os.Stat(filepath.Join(src, "Chart.yaml")); err != nil {
		return "", "", fmt.Errorf("%s is not a chart: %w", spec.Path, err)
	}
	chartDir := filepath.Join(dst, path.Base(spec.Path))
	if err := os.RemoveAll(chartDir); err != nil {
		return "", "", err
	}
	if err := copyTree(src, chartDir); err != nil {
		return "", "", err
	}
	names := make([]string, 0, len(spec.Libraries))
	for n := range spec.Libraries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		lib, err := within(tree, path.Join(base, spec.Libraries[n]))
		if err != nil {
			return "", "", err
		}
		if err := copyTree(lib, filepath.Join(chartDir, "charts", n)); err != nil {
			return "", "", fmt.Errorf("library %s: %w", n, err)
		}
	}
	if len(spec.ValuesFiles) > 0 {
		merged := map[string]any{}
		for _, vf := range spec.ValuesFiles {
			p, err := within(chartDir, vf)
			if err != nil {
				return "", "", err
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return "", "", fmt.Errorf("values file %s: %w", vf, err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(raw, &m); err != nil {
				return "", "", fmt.Errorf("values file %s: %w", vf, err)
			}
			merged = MergeValues(merged, m)
		}
		out, err := yaml.Marshal(merged)
		if err != nil {
			return "", "", err
		}
		if err := os.WriteFile(filepath.Join(chartDir, "values.yaml"), out, 0o644); err != nil {
			return "", "", err
		}
	}
	digest, err := DirDigest(chartDir)
	if err != nil {
		return "", "", err
	}
	return chartDir, digest, nil
}

// MergeValues deep-merges over into base: maps merge key by key, anything
// else in over replaces what base had. Neither input is modified.
func MergeValues(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		bm, bok := out[k].(map[string]any)
		om, ook := v.(map[string]any)
		if bok && ook {
			out[k] = MergeValues(bm, om)
			continue
		}
		out[k] = v
	}
	return out
}

// DirDigest hashes file paths and contents in a stable order.
func DirDigest(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		fmt.Fprintf(h, "%s\x00", filepath.ToSlash(rel))
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// within joins rel onto root and refuses results outside root, so a
// PackageSource cannot point a chart at files outside its tree.
func within(root, rel string) (string, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	r, err := filepath.Rel(root, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the package tree", rel)
	}
	return p, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

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

package bundle

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bundle crosses the air gap from wherever it was made: its paths must
// never lead out of the directory it is unpacked into.

func FuzzSafePath(f *testing.F) {
	for _, s := range []string{"index.yaml", "root/1.yaml", "../x", "a/../../b", "/etc/passwd", "a\\..\\b", ".", "", "a//b"} {
		f.Add(s)
	}
	root := filepath.FromSlash("/bundle/dir")
	f.Fuzz(func(t *testing.T, rel string) {
		p, err := safePath(root, rel)
		if err != nil {
			return
		}
		r, err := filepath.Rel(root, p)
		if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
			t.Fatalf("%q resolves to %s, outside %s", rel, p, root)
		}
	})
}

func FuzzUnpack(f *testing.F) {
	tarOf := func(entries map[string]byte) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, typ := range entries {
			_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Mode: 0o644, Linkname: "../../outside"})
		}
		_ = tw.Close()
		return buf.Bytes()
	}
	f.Add(tarOf(map[string]byte{"bundle.yaml": tar.TypeReg, "charts/": tar.TypeDir}))
	f.Add(tarOf(map[string]byte{"../escape": tar.TypeReg}))
	f.Add(tarOf(map[string]byte{"link": tar.TypeSymlink}))
	f.Fuzz(func(t *testing.T, raw []byte) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "in")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = Unpack(bytes.NewReader(raw), dir)
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("unpacking wrote next to the bundle directory: %v", entries)
		}
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Type()&os.ModeSymlink != 0 {
				t.Fatalf("unpacking made a symlink %s", p)
			}
			return nil
		})
	})
}

func FuzzManifest(f *testing.F) {
	f.Add([]byte("apiVersion: kubepkg.dev/v1alpha1\nkind: Bundle\nrepositories: [{name: main, url: 'https://x/index.yaml', files: [index.yaml]}]\npackages: []\n"))
	f.Add([]byte("kind: Bundle\nrepositories: [{name: ../../etc, url: x, files: []}]\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ManifestFile), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := Read(dir)
		if err != nil {
			return
		}
		for _, r := range m.Repositories {
			if strings.ContainsAny(r.Name, "/\\.") {
				t.Fatalf("repository name %q accepted", r.Name)
			}
		}
	})
}

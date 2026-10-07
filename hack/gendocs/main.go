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

// Command gendocs writes the CLI and API reference pages from the
// commands and the CRDs themselves, so they cannot drift from the code.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/tym83/kubepkg/pkg/cli"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: gendocs <crd-dir> <cli.md> <api.md>")
		os.Exit(2)
	}
	if err := writeCLI(os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := writeAPI(os.Args[1], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func writeCLI(out string) error {
	root := cli.NewRootCommand(cli.DefaultOptions())
	var b strings.Builder
	b.WriteString("# CLI reference\n\n")
	b.WriteString("Generated from the commands by `make docs`; do not edit.\n\n")
	b.WriteString("Global flags:\n\n```text\n" + root.PersistentFlags().FlagUsages() + "```\n")
	walk(&b, root)
	return os.WriteFile(out, []byte(b.String()), 0o644)
}

func walk(b *strings.Builder, c *cobra.Command) {
	for _, sub := range c.Commands() {
		if !sub.IsAvailableCommand() || sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		if sub.Runnable() {
			fmt.Fprintf(b, "\n## %s\n\n%s\n\n", sub.CommandPath(), sub.Short)
			if sub.Long != "" {
				fmt.Fprintf(b, "%s\n\n", sub.Long)
			}
			fmt.Fprintf(b, "```text\n%s\n", sub.UseLine())
			if f := sub.LocalFlags().FlagUsages(); f != "" {
				fmt.Fprintf(b, "\n%s", f)
			}
			b.WriteString("```\n")
			if sub.Example != "" {
				fmt.Fprintf(b, "\nExample:\n\n```bash\n%s\n```\n", sub.Example)
			}
		}
		walk(b, sub)
	}
}

func writeAPI(dir, out string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return err
	}
	var crds []apiextensionsv1.CustomResourceDefinition
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		crds = append(crds, crd)
	}
	sort.Slice(crds, func(i, j int) bool { return crds[i].Spec.Names.Kind < crds[j].Spec.Names.Kind })
	var b strings.Builder
	b.WriteString("# API reference\n\nGenerated from the CRDs by `make docs`; do not edit.\n\n")
	b.WriteString("All resources are cluster-scoped and served as `kubepkg.dev/v1`, or under the group a platform chooses with `--api-group`. `v1beta1` and `v1alpha1` are still served, deprecated, with the same schema; see [Compatibility](compatibility.md).\n\n")
	for _, crd := range crds {
		b.WriteString("- [" + crd.Spec.Names.Kind + "](#" + strings.ToLower(crd.Spec.Names.Kind) + ")\n")
	}
	for _, crd := range crds {
		v := crd.Spec.Versions[0]
		for _, sv := range crd.Spec.Versions {
			if sv.Storage {
				v = sv
			}
		}
		fmt.Fprintf(&b, "\n## %s\n\n", crd.Spec.Names.Kind)
		if d := v.Schema.OpenAPIV3Schema.Description; d != "" {
			fmt.Fprintf(&b, "%s\n\n", oneLine(d))
		}
		if len(crd.Spec.Names.ShortNames) > 0 {
			fmt.Fprintf(&b, "Short names: `%s`.\n\n", strings.Join(crd.Spec.Names.ShortNames, "`, `"))
		}
		for _, part := range []string{"spec", "status"} {
			p, ok := v.Schema.OpenAPIV3Schema.Properties[part]
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "### %s.%s\n\n| Field | Type | Description |\n|---|---|---|\n", crd.Spec.Names.Kind, part)
			fields(&b, "", p)
			b.WriteString("\n")
		}
	}
	return os.WriteFile(out, []byte(b.String()), 0o644)
}

// fields writes one row per field, nested fields with dotted paths.
func fields(b *strings.Builder, prefix string, s apiextensionsv1.JSONSchemaProps) {
	if s.Type == "array" && s.Items != nil && s.Items.Schema != nil {
		s = *s.Items.Schema
		prefix += "[]"
	}
	names := make([]string, 0, len(s.Properties))
	for n := range s.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	for _, n := range names {
		p := s.Properties[n]
		path := strings.TrimPrefix(prefix+"."+n, ".")
		typ := p.Type
		if typ == "array" && p.Items != nil && p.Items.Schema != nil {
			typ = "[]" + p.Items.Schema.Type
		}
		if p.XIntOrString {
			typ = "int or string"
		}
		if p.XPreserveUnknownFields != nil && *p.XPreserveUnknownFields {
			typ = "any"
		}
		if len(p.Enum) > 0 {
			var vals []string
			for _, e := range p.Enum {
				vals = append(vals, strings.Trim(string(e.Raw), `"`))
			}
			typ += ": " + strings.Join(vals, ", ")
		}
		if required[n] {
			typ += ", required"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s |\n", path, typ, oneLine(p.Description))
		if path == "conditions" || strings.HasSuffix(path, ".conditions") {
			continue // standard metav1.Condition
		}
		if p.Type == "object" || (p.Type == "array" && p.Items != nil && p.Items.Schema != nil && p.Items.Schema.Type == "object") {
			fields(b, path, p)
		}
	}
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	return strings.ReplaceAll(s, "|", "\\|")
}

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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// SourcePlugin fills Out with the files of a source.
type SourcePlugin interface {
	Fetch(ctx context.Context, in PluginInput) error
}

// StepPlugin changes the chart in Chart in place.
type StepPlugin interface {
	Run(ctx context.Context, in PluginInput) error
}

// PluginInput is what a plugin gets.
type PluginInput struct {
	// With holds the recipe's parameters for this call.
	With map[string]any
	// RecipeDir is the recipe directory, read-only.
	RecipeDir string
	// Out is an empty directory a source plugin fills.
	Out string
	// Chart is the chart directory a step plugin changes.
	Chart string
	// Package and Version identify the package being built.
	Package, Version string
}

// SourceFunc and StepFunc adapt functions to the plugin interfaces.
type (
	SourceFunc func(ctx context.Context, in PluginInput) error
	StepFunc   func(ctx context.Context, in PluginInput) error
)

// Fetch implements SourcePlugin.
func (f SourceFunc) Fetch(ctx context.Context, in PluginInput) error { return f(ctx, in) }

// Run implements StepPlugin.
func (f StepFunc) Run(ctx context.Context, in PluginInput) error { return f(ctx, in) }

// Plugins are the plugins a build can call by name. Names not registered
// here are looked up as executables on PATH: kubepkg-source-<name> and
// kubepkg-step-<name>.
type Plugins struct {
	Sources map[string]SourcePlugin
	Steps   map[string]StepPlugin
}

func (p Plugins) source(name string) (SourcePlugin, error) {
	if sp, ok := p.Sources[name]; ok {
		return sp, nil
	}
	path, err := exec.LookPath("kubepkg-source-" + name)
	if err != nil {
		return nil, fmt.Errorf("no source plugin %q: not registered and kubepkg-source-%s is not on PATH", name, name)
	}
	return execPlugin{path: path}, nil
}

func (p Plugins) step(name string) (StepPlugin, error) {
	if sp, ok := p.Steps[name]; ok {
		return sp, nil
	}
	path, err := exec.LookPath("kubepkg-step-" + name)
	if err != nil {
		return nil, fmt.Errorf("no step plugin %q: not registered and kubepkg-step-%s is not on PATH", name, name)
	}
	return execPlugin{path: path}, nil
}

// execPlugin runs an executable plugin. The protocol is small enough for a
// shell script: With arrives as JSON on stdin, directories and the package
// in KUBEPKG_* environment variables, the working directory is the recipe,
// and a non-zero exit fails the build with the plugin's stderr.
type execPlugin struct{ path string }

func (e execPlugin) Fetch(ctx context.Context, in PluginInput) error { return e.run(ctx, in) }
func (e execPlugin) Run(ctx context.Context, in PluginInput) error   { return e.run(ctx, in) }

func (e execPlugin) run(ctx context.Context, in PluginInput) error {
	with := in.With
	if with == nil {
		with = map[string]any{}
	}
	raw, err := json.Marshal(with)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, e.path)
	cmd.Dir = in.RecipeDir
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Env = append(os.Environ(),
		"KUBEPKG_RECIPE_DIR="+in.RecipeDir,
		"KUBEPKG_OUT="+in.Out,
		"KUBEPKG_CHART="+in.Chart,
		"KUBEPKG_PACKAGE="+in.Package,
		"KUBEPKG_VERSION="+in.Version,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", e.path, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

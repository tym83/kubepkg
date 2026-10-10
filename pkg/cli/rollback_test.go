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
	"testing"

	"github.com/kuberoot-dev/kubepkg/api/v1"
)

func rev(n int64, phase string) v1.PackageRevision {
	return v1.PackageRevision{
		Spec:   v1.PackageRevisionSpec{Revision: n},
		Status: v1.PackageRevisionStatus{Phase: phase},
	}
}

func TestPreviousApplied(t *testing.T) {
	revs := []v1.PackageRevision{
		rev(1, v1.PhaseSuperseded),
		rev(2, v1.PhaseFailed),
		rev(4, v1.PhaseApplied),
		rev(3, v1.PhaseSuperseded),
	}
	cases := []struct {
		current, want int64
	}{
		{4, 3},
		{3, 1},
		{1, 0},
	}
	for _, c := range cases {
		if got := previousApplied(revs, c.current); got != c.want {
			t.Errorf("current %d: got %d, want %d", c.current, got, c.want)
		}
	}
}

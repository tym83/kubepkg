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

// Package admission keeps pods in package namespaces on the images their
// packages pin: a tag a package pins becomes that tag at its digest, and
// in enforce mode an image no installed package vouches for is refused.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kuberoot-dev/kubepkg/api/v1"
	"github.com/kuberoot-dev/kubepkg/pkg/images"
)

// LabelImagePolicy on a namespace turns the image policy on there; the
// value is warn or enforce. The operator sets it on package namespaces.
const LabelImagePolicy = "kubepkg.dev/image-policy"

// Modes of the image policy.
const (
	ModeOff     = "off"
	ModeWarn    = "warn"
	ModeEnforce = "enforce"
)

// Pinned is what the installed packages pin.
type Pinned struct {
	// byTag maps registry/repository:tag to the pinned digest.
	byTag map[string]string
	// allowed holds registry/repository@digest of every pinned image.
	allowed map[string]bool
}

// Collect gathers the images the installed packages pin.
func Collect(ctx context.Context, c client.Reader) (*Pinned, error) {
	var srcs v1.PackageSourceList
	if err := c.List(ctx, &srcs); err != nil {
		return nil, err
	}
	p := &Pinned{byTag: map[string]string{}, allowed: map[string]bool{}}
	for _, s := range srcs.Items {
		for _, img := range s.Spec.Images {
			repo, tag, digest := images.Split(img)
			if digest == "" {
				continue
			}
			p.allowed[repo+"@"+digest] = true
			if tag != "" {
				p.byTag[repo+":"+tag] = digest
			}
		}
	}
	return p, nil
}

// Resolve returns the image a container should run and whether a package
// vouches for it: a pinned tag gets its digest; an image with a digest is
// vouched for when a package pins that digest.
func (p *Pinned) Resolve(image string) (string, bool) {
	n, err := images.Normalize(image)
	if err != nil {
		return image, false
	}
	repo, tag, digest := images.Split(n)
	if digest == "" {
		d, ok := p.byTag[repo+":"+tag]
		if !ok {
			return image, false
		}
		return n + "@" + d, true
	}
	return image, p.allowed[repo+"@"+digest]
}

// Handler is the pod admission webhook.
type Handler struct {
	// Reader reads PackageSources and Namespaces, from the cache.
	Reader  client.Reader
	Decoder admission.Decoder
	// Recorder, when set, gets an Event on the pod's owner for every
	// image no package pins. Admission warnings go back to whoever made
	// the pod, usually a controller, so without Events nobody sees them.
	Recorder events.EventRecorder
}

// Handle implements admission.Handler.
func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := h.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	ns := &corev1.Namespace{}
	if err := h.Reader.Get(ctx, client.ObjectKey{Name: req.Namespace}, ns); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("namespace %s: %w", req.Namespace, err))
	}
	mode := ns.Labels[LabelImagePolicy]
	if mode != ModeWarn && mode != ModeEnforce {
		return admission.Allowed("")
	}
	pinned, err := Collect(ctx, h.Reader)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	var unvouched []string
	changed := false
	fix := func(cs []corev1.Container) {
		for i := range cs {
			resolved, ok := pinned.Resolve(cs[i].Image)
			if !ok {
				unvouched = append(unvouched, cs[i].Image)
				continue
			}
			if resolved != cs[i].Image {
				cs[i].Image, changed = resolved, true
			}
		}
	}
	fix(pod.Spec.InitContainers)
	fix(pod.Spec.Containers)
	if len(unvouched) > 0 {
		msg := fmt.Sprintf("no installed package pins %s", strings.Join(unvouched, ", "))
		h.record(pod, req.Namespace, mode, msg)
		if mode == ModeEnforce {
			return admission.Denied(msg + "; namespace " + req.Namespace + " runs only images packages pin (" + LabelImagePolicy + "=enforce)")
		}
		resp := h.patched(req, pod, changed)
		resp.Warnings = append(resp.Warnings, msg)
		return resp
	}
	return h.patched(req, pod, changed)
}

func (h *Handler) patched(req admission.Request, pod *corev1.Pod, changed bool) admission.Response {
	if !changed {
		return admission.Allowed("")
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}

// record puts an Event on the object that made the pod, or on the
// namespace for a pod nothing owns.
func (h *Handler) record(pod *corev1.Pod, namespace, mode, msg string) {
	if h.Recorder == nil {
		return
	}
	ref := &corev1.ObjectReference{APIVersion: "v1", Kind: "Namespace", Name: namespace}
	for _, o := range pod.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			ref = &corev1.ObjectReference{APIVersion: o.APIVersion, Kind: o.Kind, Name: o.Name, Namespace: namespace, UID: o.UID}
			break
		}
	}
	reason, action := "UnpinnedImage", "Warned"
	if mode == ModeEnforce {
		reason, action = "ImageRefused", "Refused"
	}
	h.Recorder.Eventf(ref, nil, corev1.EventTypeWarning, reason, action, "%s (%s=%s)", msg, LabelImagePolicy, mode)
}

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package render builds kustomizations in-process and answers questions
// about the objects they produce: which images they run, and whether a
// given object is there and shaped the way the platform needs.
//
// It uses the kustomize library rather than the kubectl binary, so tests
// that hold the profiles to their invariants need nothing installed.
package render

import (
	"fmt"
	"strings"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// Object is one rendered Kubernetes object.
type Object struct {
	*yaml.RNode
}

// Kind returns the object's kind.
func (o Object) Kind() string { return o.GetKind() }

// Name returns the object's name.
func (o Object) Name() string { return o.GetName() }

// Namespace returns the object's namespace.
func (o Object) Namespace() string { return o.GetNamespace() }

// Objects is a rendered kustomization.
type Objects []Object

// Build renders the kustomization in dir with kustomize's default
// options (files outside the kustomization root are refused, as
// `kubectl kustomize` refuses them).
func Build(dir string) (Objects, error) {
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	m, err := k.Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, err
	}
	var objs Objects
	for _, r := range m.Resources() {
		objs = append(objs, Object{r.Copy()})
	}
	return objs, nil
}

// YAML returns the objects as one multi-document YAML stream.
func (objs Objects) YAML() (string, error) {
	var b strings.Builder
	for i, o := range objs {
		if i > 0 {
			b.WriteString("---\n")
		}
		s, err := o.String()
		if err != nil {
			return "", err
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

// Find returns the object with the given kind, namespace and name.
func (objs Objects) Find(kind, namespace, name string) (Object, bool) {
	for _, o := range objs {
		if o.Kind() == kind && o.Namespace() == namespace && o.Name() == name {
			return o, true
		}
	}
	return Object{}, false
}

// Image is a container image reference found in a rendered object.
type Image struct {
	// Object is "<Kind>/<namespace>/<name>".
	Object string
	// Container is the container's name.
	Container string
	// Ref is the image reference as rendered.
	Ref string
}

// Images lists every container and init container image in objs. It
// looks for `containers` and `initContainers` lists anywhere in an
// object, as kustomize's image transformer does, so a workload kind or a
// custom resource that embeds a pod template is covered without a list
// of kinds to keep current.
func (objs Objects) Images() ([]Image, error) {
	var out []Image
	for _, o := range objs {
		id := fmt.Sprintf("%s/%s/%s", o.Kind(), o.Namespace(), o.Name())
		if err := walkContainers(o.RNode, func(c *yaml.RNode) {
			out = append(out, Image{Object: id, Container: str(c, "name"), Ref: str(c, "image")})
		}); err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
	}
	return out, nil
}

func walkContainers(n *yaml.RNode, visit func(*yaml.RNode)) error {
	switch n.YNode().Kind {
	case yaml.MappingNode:
		fields, err := n.Fields()
		if err != nil {
			return err
		}
		for _, name := range fields {
			v := n.Field(name).Value
			if (name == "containers" || name == "initContainers") && v.YNode().Kind == yaml.SequenceNode {
				elems, err := v.Elements()
				if err != nil {
					return err
				}
				for _, c := range elems {
					if c.YNode().Kind == yaml.MappingNode && c.Field("image") != nil {
						visit(c)
					}
				}
				continue
			}
			if err := walkContainers(v, visit); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		elems, err := n.Elements()
		if err != nil {
			return err
		}
		for _, e := range elems {
			if err := walkContainers(e, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// str returns the string value of field name in n, or "" if unset.
func str(n *yaml.RNode, name string) string {
	f := n.Field(name)
	if f == nil {
		return ""
	}
	return yaml.GetValue(f.Value)
}

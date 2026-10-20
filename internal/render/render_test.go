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

package render

import (
	"slices"
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	objs, err := Build("testdata/app")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(objs) != 6 {
		t.Fatalf("got %d objects, want 6", len(objs))
	}
	if _, ok := objs.Find("ConfigMap", "demo", "settings"); !ok {
		t.Error("ConfigMap demo/settings not found")
	}
	if _, ok := objs.Find("ConfigMap", "other", "settings"); ok {
		t.Error("found ConfigMap in the wrong namespace")
	}

	imgs, err := objs.Images()
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	var got []string
	for _, i := range imgs {
		got = append(got, i.Object+"/"+i.Container+"="+i.Ref)
	}
	want := []string{
		"Deployment/demo/web/init=busybox:1",
		"Deployment/demo/web/web=nginx:1",
		"CronJob/demo/nightly/run=runner:2",
		"ReplicationController/demo/legacy/old=legacy:3",
		"Widget/demo/custom/w=widget:4",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Images = %v\nwant %v", got, want)
	}

	y, err := objs.YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	if strings.Count(y, "\n---\n") != 5 || !strings.Contains(y, "name: nightly") {
		t.Errorf("YAML = %q", y)
	}
}

func TestBuildErrors(t *testing.T) {
	if _, err := Build("testdata/missing"); err == nil {
		t.Error("Build(missing) succeeded")
	}
}

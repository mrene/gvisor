// Copyright 2024 The gVisor Authors.
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

//go:build darwin && arm64

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseBindMount(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for _, test := range []struct {
		spec    string
		want    bindMount
		wantErr bool
	}{
		{spec: "/src", want: bindMount{host: "/src", guest: "/src"}},
		{spec: "/src:ro", want: bindMount{host: "/src", guest: "/src", readOnly: true}},
		{spec: "/src:/work", want: bindMount{host: "/src", guest: "/work"}},
		{spec: "/src:/work:ro", want: bindMount{host: "/src", guest: "/work", readOnly: true}},
		{spec: "/src:/work/:rw", want: bindMount{host: "/src", guest: "/work"}},
		{spec: "data", want: bindMount{host: filepath.Join(wd, "data"), guest: filepath.Join(wd, "data")}},
		{spec: "", wantErr: true},
		{spec: ":/work", wantErr: true},
		{spec: "/src:work", wantErr: true},
		{spec: "/src:/work:/other", wantErr: true},
	} {
		got, err := parseBindMount(test.spec)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseBindMount(%q) = %v, want error", test.spec, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBindMount(%q): %v", test.spec, err)
			continue
		}
		if got != test.want {
			t.Errorf("parseBindMount(%q) = %+v, want %+v", test.spec, got, test.want)
		}
	}
}

func TestPlanMounts(t *testing.T) {
	store := bindMount{host: "/nix/store", guest: "/nix/store", readOnly: true}
	for _, test := range []struct {
		name     string
		explicit []bindMount
		home     string
		cwd      string
		want     mountPlan
		wantErr  bool
	}{
		{
			name: "cwd shared at its host path",
			cwd:  "/Users/me/proj",
			want: mountPlan{
				mounts:  []bindMount{{host: "/Users/me/proj", guest: "/Users/me/proj"}},
				workDir: "/Users/me/proj",
			},
		},
		{
			name: "cwd inside shared home",
			home: "/Users/me",
			cwd:  "/Users/me/proj",
			want: mountPlan{
				mounts:  []bindMount{{host: "/Users/me", guest: "/Users/me"}},
				workDir: "/Users/me/proj",
			},
		},
		{
			name: "cwd inside nix store",
			cwd:  "/nix/store/abc-pkg/bin",
			want: mountPlan{workDir: "/nix/store/abc-pkg/bin"},
		},
		{
			name:     "cwd inside explicit mount at another guest path",
			explicit: []bindMount{{host: "/Users/me/src", guest: "/src"}},
			cwd:      "/Users/me/src/app",
			want: mountPlan{
				mounts:  []bindMount{{host: "/Users/me/src", guest: "/src"}},
				workDir: "/src/app",
			},
		},
		{
			name:     "explicit mount replaces automatic share at the same guest path",
			explicit: []bindMount{{host: "/Volumes/other", guest: "/Users/me/proj", readOnly: true}},
			cwd:      "/Users/me/proj",
			want: mountPlan{
				mounts:  []bindMount{{host: "/Volumes/other", guest: "/Users/me/proj", readOnly: true}},
				workDir: "",
			},
		},
		{
			name: "root cwd is not shared",
			cwd:  "/",
			want: mountPlan{},
		},
		{
			name:     "parents are mounted before children",
			explicit: []bindMount{{host: "/data/cache", guest: "/Users/me/cache"}},
			home:     "/Users/me",
			want: mountPlan{
				mounts: []bindMount{
					{host: "/Users/me", guest: "/Users/me"},
					{host: "/data/cache", guest: "/Users/me/cache"},
				},
			},
		},
		{
			name:     "reserved guest path",
			explicit: []bindMount{{host: "/tmp/x", guest: "/proc/x"}},
			wantErr:  true,
		},
		{
			name: "duplicate guest path",
			explicit: []bindMount{
				{host: "/a", guest: "/work"},
				{host: "/b", guest: "/work"},
			},
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := planMounts(test.explicit, test.home, test.cwd, []bindMount{store})
			if test.wantErr {
				if err == nil {
					t.Fatalf("planMounts() = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("planMounts(): %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("planMounts() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestGuestExecPath(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	mk := func(p string) string {
		p = filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return p
	}
	symlink := func(target, name string) string {
		name = filepath.Join(dir, name)
		if err := os.Symlink(target, name); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
		return name
	}
	mk("store/pkg/bin/prog")
	mk("work/tool")
	outside := mk("outside/prog")
	result := symlink(filepath.Join(dir, "store/pkg"), "work/result")
	workLink := symlink(filepath.Join(dir, "work"), "worklink")
	mounts := []bindMount{
		{host: filepath.Join(dir, "store"), guest: "/nix/store"},
		// Mounted by a host path through a symlink, like macOS /tmp.
		{host: workLink, guest: "/work"},
	}
	for _, test := range []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{"symlink into another mount", filepath.Join(result, "bin/prog"), "/nix/store/pkg/bin/prog", true},
		{"real path under symlinked mount", filepath.Join(dir, "work/tool"), "/work/tool", true},
		{"symlinked path under symlinked mount", filepath.Join(workLink, "tool"), "/work/tool", true},
		{"outside every mount", outside, "", false},
		{"missing", filepath.Join(dir, "work/missing"), "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := guestExecPath(test.path, mounts)
			if got != test.want || ok != test.wantOK {
				t.Errorf("guestExecPath(%q) = (%q, %v), want (%q, %v)", test.path, got, ok, test.want, test.wantOK)
			}
		})
	}
}

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

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func validNixBuild(tmpDir string) nixBuild {
	return nixBuild{
		Version:         1,
		Builder:         "/nix/store/aaaa-bash/bin/bash",
		Args:            []string{"-e", "/nix/store/bbbb-builder.sh"},
		Env:             map[string]string{"out": "/nix/store/cccc-hello"},
		Outputs:         map[string]string{"out": "/nix/store/cccc-hello"},
		StoreDir:        "/nix/store",
		RealStoreDir:    "/nix/store",
		TmpDir:          tmpDir,
		TmpDirInSandbox: "/build",
		TopTmpDir:       filepath.Dir(tmpDir),
		System:          "aarch64-linux",
	}
}

func writeNixBuild(t *testing.T, b nixBuild) string {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "build.json")
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadNixBuild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*nixBuild)
		ok     bool
	}{
		{name: "valid", modify: func(*nixBuild) {}, ok: true},
		{name: "chroot store", modify: func(b *nixBuild) { b.RealStoreDir = "/private/tmp/store/nix/store" }, ok: true},
		{name: "unknown protocol version", modify: func(b *nixBuild) { b.Version = 2 }},
		{name: "other system", modify: func(b *nixBuild) { b.System = "x86_64-linux" }},
		{name: "other store directory", modify: func(b *nixBuild) { b.StoreDir = "/gnu/store" }},
		{name: "output outside store", modify: func(b *nixBuild) { b.Outputs["out"] = "/tmp/out" }},
		{name: "output in store subdirectory", modify: func(b *nixBuild) { b.Outputs["out"] = "/nix/store/cccc-hello/bin" }},
		{name: "no outputs", modify: func(b *nixBuild) { b.Outputs = nil }},
		{name: "relative build directory", modify: func(b *nixBuild) { b.TmpDirInSandbox = "build" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := validNixBuild(t.TempDir())
			tc.modify(&b)
			_, err := readNixBuild(writeNixBuild(t, b))
			if (err == nil) != tc.ok {
				t.Errorf("readNixBuild: got error %v, want ok=%t", err, tc.ok)
			}
		})
	}
}

func TestNixBuildFixedOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		attrs map[string]any // written to .attrs.json in the build directory
		want  bool
	}{
		{name: "input-addressed", env: map[string]string{"out": "/nix/store/cccc-hello"}},
		{name: "fixed-output", env: map[string]string{"outputHash": "sha256-AAAA"}, want: true},
		{
			name:  "structured attrs, input-addressed",
			env:   map[string]string{"NIX_ATTRS_JSON_FILE": "/build/.attrs.json"},
			attrs: map[string]any{"name": "hello"},
		},
		{
			name:  "structured attrs, fixed-output",
			env:   map[string]string{"NIX_ATTRS_JSON_FILE": "/build/.attrs.json"},
			attrs: map[string]any{"name": "src", "outputHash": "sha256-AAAA"},
			want:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			b := validNixBuild(tmpDir)
			b.Env = tc.env
			if tc.attrs != nil {
				data, err := json.Marshal(tc.attrs)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(tmpDir, ".attrs.json"), data, 0644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := b.fixedOutput()
			if err != nil {
				t.Fatalf("fixedOutput: %v", err)
			}
			if got != tc.want {
				t.Errorf("fixedOutput = %t, want %t", got, tc.want)
			}
		})
	}
}

/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package filesapi

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// Filestore holds information about a filestore (e.g. GCS or S3 bucket),
// to be written in a manifest file.
type Filestore struct {
	// Base is the leading part of an artifact path, including the scheme.
	// It is everything that is not the actual file name itself.
	// e.g. "gs://prod-artifacts/myproject"
	Base           string `yaml:"base,omitempty"`
	ServiceAccount string `yaml:"service-account,omitempty"`
	Src            bool   `yaml:"src,omitempty"`
}

// File holds information about a file artifact.
// File artifacts are copied from a source Filestore to N destination Filestores
type File struct {
	// Name is the relative path of the file, relative to the Filestore base
	Name string `yaml:"name"`
	// SHA256 holds the SHA256 hash of the specified file (hex encoded)
	SHA256 string `yaml:"sha256,omitempty"`
}

// Manifest stores the information in a manifest file (describing the
// desired state of a Docker Registry).
type Manifest struct {
	// Filestores contains the source and destination (Src/Dest) filestores.
	// Filestores are (for example) GCS or S3 buckets.
	// It is possible that in the future, we support promoting to multiple
	// filestores, in which case we would have more than just Src/Dest.
	Filestores []Filestore `yaml:"filestores,omitempty"`
	Files      []File      `yaml:"files,omitempty"`
}

// ParseManifest parses a Manifest.
func ParseManifest(b []byte) (*Manifest, error) {
	m := &Manifest{}
	if err := yaml.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("error parsing manifest: %v", err)
	}
	return m, nil
}

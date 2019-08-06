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
	"strings"
	"testing"
)

func TestValidateFilestores(t *testing.T) {
	var tests = []struct {
		filestores    []Filestore
		expectedError string
	}{
		{
			// Filestores are required
			filestores:    []Filestore{},
			expectedError: "filestore must be specified",
		},
		{
			// Filestores are required
			filestores:    nil,
			expectedError: "filestore must be specified",
		},
		{
			filestores: []Filestore{
				{Src: true, Base: "gs://src"},
			},
			expectedError: "no destination filestores found",
		},
		{
			filestores: []Filestore{
				{Base: "gs://dest1"},
			},
			expectedError: "source filestore not found",
		},
		{
			filestores: []Filestore{
				{Src: true, Base: "gs://src1"},
				{Src: true, Base: "gs://src2"},
			},
			expectedError: "found multiple source filestores",
		},
		{
			filestores: []Filestore{
				{Src: true, Base: "gs://src"},
				{Base: "gs://dest1"},
				{Base: "gs://dest2"},
			},
		},
		{
			filestores: []Filestore{
				{Src: true},
				{Base: "gs://dest"},
			},
			expectedError: "filestore did not have base set",
		},
		{
			filestores: []Filestore{
				{Src: true, Base: "gs://src"},
				{Base: "s3://dest"},
			},
			expectedError: "unsupported scheme in base",
		},
	}
	for _, test := range tests {
		err := validateFilestores(test.filestores)
		checkErrorMatchesExpected(t, err, test.expectedError)

	}
}

func TestValidateFiles(t *testing.T) {
	oksha := "4f2f040fa2bfe9bea64911a2a756e8a1727a8bfd757c5e031631a6e699fcf246"

	var tests = []struct {
		files         []File
		expectedError string
	}{
		{
			// Files are required
			files:         []File{},
			expectedError: "file must be specified",
		},
		{
			// Files are required
			files:         nil,
			expectedError: "file must be specified",
		},
		{
			files: []File{
				{Name: "foo", SHA256: oksha},
			},
		},
		{
			files: []File{
				{SHA256: oksha},
			},
			expectedError: "name is required for file",
		},
		{
			files: []File{
				{Name: "foo", SHA256: "bad"},
			},
			expectedError: "sha256 was not valid (not hex)",
		},
		{
			files: []File{
				{Name: "foo"},
			},
			expectedError: "sha256 is required",
		},
		{
			files: []File{
				{Name: "foo", SHA256: "abcd"},
			},
			expectedError: "sha256 was not valid (bad length)",
		},
	}
	for _, test := range tests {
		err := validateFiles(test.files)
		checkErrorMatchesExpected(t, err, test.expectedError)
	}
}

func checkErrorMatchesExpected(t *testing.T, err error, expected string) {
	if err != nil && expected == "" {
		t.Errorf("unexpected error: %v", err)
	}
	if err != nil && expected != "" {
		actual := fmt.Sprintf("%v", err)
		if !strings.Contains(actual, expected) {
			t.Errorf("error %q did not contain expected %q", err, expected)
		}
	}
	if err == nil && expected != "" {
		t.Errorf("expected error %q", expected)
	}

}

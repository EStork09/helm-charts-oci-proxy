// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Catalog struct {
	Repos []string `json:"repositories"`
}

type listTags struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type Manifest struct {
	ContentType string    `json:"contentType"`
	Blob        []byte    `json:"blob"`
	Refs        []string  `json:"refs"` // referenced blobs digests
	CreatedAt   time.Time `json:"createdAt"`
}

type Manifests struct {
	// maps repo -> Manifest tag/digest -> Manifest
	manifests map[string]map[string]Manifest
	registry  *registry
	lock      sync.Mutex
	log       *log.Logger
}

func isManifest(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "manifests"
}

func isTags(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "tags"
}

func isCatalog(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 2 {
		return false
	}

	return elems[len(elems)-1] == "_catalog"
}

// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulling-an-image-manifest
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushing-an-image
func (m *Manifests) handle(resp http.ResponseWriter, req *http.Request) *regError {
	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	target := elem[len(elem)-1]
	repo := strings.Join(elem[1:len(elem)-2], "/")
	hostnameParts := strings.Split(repo, "/")

	switch req.Method {
	case http.MethodGet:
		m.lock.Lock()
		defer m.lock.Unlock()

		c, ok := m.manifests[repo]
		if !ok {
			if len(hostnameParts) < 2 {
				return &regError{
					Status:  http.StatusBadRequest,
					Code:    "INVALID PARAMS",
					Message: "No chart name specified",
				}
			}
			err := m.registry.PrepareChart(req.Context(), hostnameParts[0], hostnameParts[1], target)
			if err != nil {
				return err
			}
		}

		ma, ok := c[target]
		if !ok {
			err := m.registry.PrepareChart(req.Context(), hostnameParts[0], hostnameParts[1], target)
			if err != nil {
				return err
			}
			ma, ok = c[target]
			if !ok {
				// we failed
				return &regError{
					Status:  http.StatusNotFound,
					Code:    "NOT FOUND",
					Message: "Chart prepare error",
				}
			}
		}
		rd := sha256.Sum256(ma.Blob)
		d := "sha256:" + hex.EncodeToString(rd[:])
		resp.Header().Set("Docker-Content-Digest", d)
		resp.Header().Set("Content-Type", ma.ContentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(ma.Blob)))
		resp.WriteHeader(http.StatusOK)
		io.Copy(resp, bytes.NewReader(ma.Blob))
		return nil

	case http.MethodHead:
		m.lock.Lock()
		defer m.lock.Unlock()
		if _, ok := m.manifests[repo]; !ok {
			if len(hostnameParts) < 2 {
				return &regError{
					Status:  http.StatusBadRequest,
					Code:    "INVALID PARAMS",
					Message: "No chart name specified",
				}
			}
			err := m.registry.PrepareChart(req.Context(), hostnameParts[0], hostnameParts[1], target)
			if err != nil {
				return err
			}
		}
		ma, ok := m.manifests[repo][target]
		if !ok {
			err := m.registry.PrepareChart(req.Context(), hostnameParts[0], hostnameParts[1], target)
			if err != nil {
				return err
			}
			ma, ok = m.manifests[repo][target]
			if !ok {
				// we failed
				return &regError{
					Status:  http.StatusNotFound,
					Code:    "NOT FOUND",
					Message: "Chart prepare error",
				}
			}
		}
		rd := sha256.Sum256(ma.Blob)
		d := "sha256:" + hex.EncodeToString(rd[:])
		resp.Header().Set("Docker-Content-Digest", d)
		resp.Header().Set("Content-Type", ma.ContentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(ma.Blob)))
		resp.WriteHeader(http.StatusOK)
		return nil

	default:
		return &regError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
}

func (m *Manifests) handleTags(resp http.ResponseWriter, req *http.Request) *regError {
	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	repo := strings.Join(elem[1:len(elem)-2], "/")
	hostnameParts := strings.Split(repo, "/")

	if req.Method == "GET" {
		m.lock.Lock()
		defer m.lock.Unlock()

		err := m.registry.PrepareChart(req.Context(), hostnameParts[0], hostnameParts[1], "")
		if err != nil {
			return err
		}

		c, ok := m.manifests[repo]
		if !ok {
			if !ok {
				return &regError{
					Status:  http.StatusNotFound,
					Code:    "NOT FOUND",
					Message: "Chart prepare error",
				}
			}
		}

		var tags []string
		for tag := range c {
			if !strings.Contains(tag, "sha256:") {
				tags = append(tags, tag)
			}
		}
		sort.Strings(tags)

		// https://github.com/opencontainers/distribution-spec/blob/b505e9cc53ec499edbd9c1be32298388921bb705/detail.md#tags-paginated
		// Offset using last query parameter.
		if last := req.URL.Query().Get("last"); last != "" {
			for i, t := range tags {
				if t > last {
					tags = tags[i:]
					break
				}
			}
		}

		// Limit using n query parameter.
		if ns := req.URL.Query().Get("n"); ns != "" {
			if n, err := strconv.Atoi(ns); err != nil {
				return &regError{
					Status:  http.StatusBadRequest,
					Code:    "BAD_REQUEST",
					Message: fmt.Sprintf("parsing n: %v", err),
				}
			} else if n < len(tags) {
				tags = tags[:n]
			}
		}

		tagsToList := listTags{
			Name: repo,
			Tags: tags,
		}

		msg, _ := json.Marshal(tagsToList)
		resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
		resp.WriteHeader(http.StatusOK)
		io.Copy(resp, bytes.NewReader(msg))
		return nil
	}

	return &regError{
		Status:  http.StatusBadRequest,
		Code:    "METHOD_UNKNOWN",
		Message: "We don't understand your method + url",
	}
}

func (m *Manifests) handleCatalog(resp http.ResponseWriter, req *http.Request) *regError {
	query := req.URL.Query()
	nStr := query.Get("n")
	n := 10000
	if nStr != "" {
		n, _ = strconv.Atoi(nStr)
	}

	if req.Method == "GET" {
		m.lock.Lock()
		defer m.lock.Unlock()

		var repos []string
		countRepos := 0
		// TODO: implement pagination
		for key := range m.manifests {
			if countRepos >= n {
				break
			}
			countRepos++
			repos = append(repos, key)
		}
		repositoriesToList := Catalog{
			Repos: repos,
		}

		msg, _ := json.Marshal(repositoriesToList)
		resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
		resp.WriteHeader(http.StatusOK)
		io.Copy(resp, bytes.NewReader([]byte(msg)))
		return nil
	}

	return &regError{
		Status:  http.StatusBadRequest,
		Code:    "METHOD_UNKNOWN",
		Message: "We don't understand your method + url",
	}
}
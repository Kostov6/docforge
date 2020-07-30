// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved.
// This file is licensed under the Apache Software License, v.2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reactor

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gardener/docode/pkg/util/tests"
	"github.com/google/go-github/v32/github"
	"github.com/stretchr/testify/assert"
)

const (
	// baseURLPath is a non-empty Client.BaseURL path to use during tests,
	// to ensure relative URLs are used for all endpoints. See issue #752.
	baseURLPath = "/api-v3"
)

func init() {
	tests.SetGlogV(6)
}

// setup sets up a test HTTP server along with a github.Client that is
// configured to talk to that test server. Tests should register handlers on
// mux which provide mock responses for the API method being tested.
func setup() (client *github.Client, mux *http.ServeMux, serverURL string, teardown func()) {
	// mux is the HTTP request multiplexer used with the test server.
	mux = http.NewServeMux()

	// We want to ensure that tests catch mistakes where the endpoint URL is
	// specified as absolute rather than relative. It only makes a difference
	// when there's a non-empty base URL path. So, use that. See issue #752.
	apiHandler := http.NewServeMux()
	apiHandler.Handle(baseURLPath+"/", http.StripPrefix(baseURLPath, mux))
	apiHandler.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintln(os.Stderr, "FAIL: Client.BaseURL path prefix is not preserved in the request URL:")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "\t"+req.URL.String())
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "\tDid you accidentally use an absolute endpoint URL rather than relative?")
		fmt.Fprintln(os.Stderr, "\tSee https://github.com/google/go-github/issues/752 for information.")
		http.Error(w, "Client.BaseURL path prefix is not preserved in the request URL.", http.StatusInternalServerError)
	})

	// server is a test HTTP server used to provide mock API responses.
	server := httptest.NewServer(apiHandler)

	// client is the GitHub client being tested and is
	// configured to use test server.
	client = github.NewClient(nil)
	url, _ := url.Parse(server.URL + baseURLPath + "/")
	client.BaseURL = url
	client.UploadURL = url

	return client, mux, server.URL, server.Close
}

func testMethod(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.Method; got != want {
		t.Errorf("Request method: %v, want %v", got, want)
	}
}

// GitHubWorker tests
func TestGitHubWorker(t *testing.T) {
	var (
		actual               bool
		err                  error
		backendRequestsCount int
	)

	client, mux, _, teardown := setup()
	defer teardown()

	parentDir, err := ioutil.TempDir("", "TestGitHubWorker")
	if err != nil {
		t.Errorf("%v", err)
	}
	task := &GitHubTask{
		parentDir:  parentDir,
		owner:      "foo",
		repository: "bar",
		entrySHA:   "123",
		entryPath:  "a/b/c",
	}
	defer os.RemoveAll(task.parentDir)

	blobsAPIEndpoint := fmt.Sprintf("/repos/%s/%s/git/blobs/%s", task.owner, task.repository, task.entrySHA)
	mux.HandleFunc(blobsAPIEndpoint, func(w http.ResponseWriter, r *http.Request) {
		backendRequestsCount++
		defer r.Body.Close()
		if _, err = ioutil.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		actual = true
		testMethod(t, r, "GET")
		w.Write([]byte(fmt.Sprintf(`{
			"content": "Q29udGVudCBvZiB0aGUgYmxvYg==\n",
			"encoding": "base64",
			"url": %s,
			"sha": %s,
			"size": 19
		}`, blobsAPIEndpoint, task.entrySHA)))
	})

	w := &GitHubWorker{
		Client: client,
	}

	workerError := w.Work(context.Background(), task)

	assert.Nil(t, err)
	assert.Nil(t, workerError)
	assert.True(t, actual)
	assert.Equal(t, 1, backendRequestsCount)
}

func TestGitHubWorkerResponseFault(t *testing.T) {
	client, mux, serverURL, teardown := setup()
	defer teardown()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	w := &GitHubWorker{
		Client: client,
	}

	err := w.Work(context.Background(), &GitHubTask{})

	assert.NotNil(t, err)
	if gotError := err.error; gotError != nil {
		errorSegments := strings.Split(err.error.Error(), " ")
		assert.True(t, len(errorSegments) == 5)
		assert.Equal(t, "GET", errorSegments[0])
		assert.Equal(t, fmt.Sprintf("%s/api-v3/repos/git/blobs/:", serverURL), errorSegments[1])
		assert.Equal(t, "500", errorSegments[2])
	}
}

func TestGitHubWorkerCtxTimeout(t *testing.T) {
	client, mux, _, teardown := setup()
	defer teardown()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
	})

	w := &GitHubWorker{
		Client: client,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := w.Work(ctx, &GitHubTask{})

	assert.NotNil(t, err)
	assert.Equal(t, "context deadline exceeded", err.Error())
}

func TestGitHubWorkerCtxCancel(t *testing.T) {
	client, mux, _, teardown := setup()
	defer teardown()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
	})

	w := &GitHubWorker{
		Client: client,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := w.Work(ctx, &GitHubTask{})

	assert.NotNil(t, err)
	assert.Equal(t, "context canceled", err.Error())
}

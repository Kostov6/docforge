// SPDX-FileCopyrightText: 2020 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gardener/docforge/pkg/api"
	"github.com/gardener/docforge/pkg/git"
	"github.com/gardener/docforge/pkg/resourcehandlers"
	"github.com/google/go-github/v32/github"
)

// fsHandler implements
// - resourcehandlers/URIValidator
// - resourcehandlers/NodeResolver
// - resourcehandlers/LinkControl
// - readers/ContextResourceReader
// - readers/GitInfoReader
type fsHandler struct{}

// NewFSResourceHandler create file system resource handler
func NewFSResourceHandler() resourcehandlers.URIValidator {
	return &fsHandler{}
}

// Accept implements resourcehandlers.URIValidator#Accept
func (fs *fsHandler) Accept(uri string) bool {
	_, err := os.Stat(uri)
	return err == nil
}

// ResolveNodeSelector implements resourcehandlers.ResourceHandler#ResolveNodeSelector
func (fs *fsHandler) ResolveNodeSelector(ctx context.Context, node *api.Node, excludePaths []string, frontMatter map[string]interface{}, excludeFrontMatter map[string]interface{}, depth int32) ([]*api.Node, error) {
	var (
		fileInfo os.FileInfo
		err      error
	)
	if node.NodeSelector == nil {
		return nil, nil
	}
	if fileInfo, err = os.Stat(node.NodeSelector.Path); err != nil {
		return nil, err
	}
	if !fileInfo.IsDir() && filepath.Ext(fileInfo.Name()) == ".md" {
		return nil, fmt.Errorf("nodeSelector path is neither directory or module")
	}
	_node := &api.Node{
		Nodes: []*api.Node{},
	}
	filepath.Walk(node.NodeSelector.Path, func(node *api.Node, parentPath string) filepath.WalkFunc {
		return func(path string, info os.FileInfo, err error) error {
			if node.NodeSelector != nil {
				return nil
			}
			if path != parentPath {
				if len(strings.Split(path, "/"))-len(strings.Split(parentPath, "/")) != 1 {
					node = node.Parent()
					pathSegments := strings.Split(path, "/")
					if len(pathSegments) > 0 {
						pathSegments = pathSegments[:len(pathSegments)-1]
						parentPath = filepath.Join(pathSegments...)
					}
				}
			}
			n := &api.Node{
				Name: info.Name(),
			}
			n.SetParent(node)
			node.Nodes = append(node.Nodes, n)
			if info.IsDir() {
				node = n
				node.Nodes = []*api.Node{}
				parentPath = path
			} else {
				n.Source = path
			}
			return nil
		}
	}(_node, node.NodeSelector.Path))
	if len(_node.Nodes) > 0 && len(_node.Nodes[0].Nodes) > 0 {
		for _, node := range _node.Nodes[0].Nodes {
			node.SetParent(nil)
		}
		return _node.Nodes[0].Nodes, nil
	}
	return nil, nil
}

// Read implements readers.ContextResourceReader#Read
func (fs *fsHandler) Read(ctx context.Context, uri string) ([]byte, error) {
	return ioutil.ReadFile(uri)
}

func (fs *fsHandler) ResolveDocumentation(ctx context.Context, uri string) (*api.Documentation, error) {
	fileInfo, err := os.Stat(uri)
	if err != nil {
		return nil, err
	}
	if fileInfo.IsDir() {
		return nil, nil
	}
	blob, err := fs.Read(ctx, uri)
	if err != nil {
		return nil, err
	}

	return api.Parse(blob)
}

// ReadGitInfo implements resourcehandlers.ResourceHandler#ReadGitInfo
func (fs *fsHandler) ReadGitInfo(ctx context.Context, uri string) ([]byte, error) {
	var (
		log  []*gitLogEntry
		blob []byte
		err  error
	)
	if !checkGitExists() {
		return nil, fmt.Errorf("reading Git info for %s failed: git not found in PATH", uri)
	}

	if log, err = gitLog(uri); err != nil {
		return nil, err
	}

	if len(log) == 0 {
		return nil, nil
	}

	for _, logEntry := range log {
		logEntry.Name = strings.Split(logEntry.Name, "<")[0]
		logEntry.Name = strings.TrimSpace(logEntry.Name)
	}
	authorName := log[len(log)-1].Name
	authorEmail := log[len(log)-1].Email
	publishD := log[len(log)-1].Date
	lastModD := log[0].Date
	gitInfo := &git.GitInfo{
		PublishDate:      &publishD,
		LastModifiedDate: &lastModD,
		Author: &github.User{
			Name:  &authorName,
			Email: &authorEmail,
		},
		Contributors: []*github.User{},
	}

	for _, logEntry := range log {
		if logEntry.Email != *gitInfo.Author.Email {
			name := logEntry.Name
			email := logEntry.Email
			gitInfo.Contributors = append(gitInfo.Contributors, &github.User{
				Name:  &name,
				Email: &email,
			})
		}
	}

	if blob, err = json.MarshalIndent(gitInfo, "", "  "); err != nil {
		return nil, err
	}

	return blob, nil
}

// ResourceName implements resourcehandlers.LinkControl#ResourceName
func (fs *fsHandler) ResourceName(link string) (name string, extension string) {
	_, name = filepath.Split(link)
	if len(name) > 0 {
		if e := filepath.Ext(name); len(e) > 0 {
			extension = e[1:]
			name = strings.TrimSuffix(name, e)
		}
	}
	return
}

// BuildAbsLink implements resourcehandlers.LinkControl#BuildAbsLink
func (fs *fsHandler) BuildAbsLink(source, link string) (string, error) {
	if filepath.IsAbs(link) {
		return link, nil
	}
	dir, _ := filepath.Split(source)
	p := filepath.Join(dir, link)
	p = filepath.Clean(p)
	if filepath.IsAbs(p) {
		return p, nil
	}
	return filepath.Abs(p)
}

// GetRawFormatLink implements resourcehandlers.LinkControl#GetRawFormatLink
func (fs *fsHandler) GetRawFormatLink(absLink string) (string, error) {
	return absLink, nil
}

// SetVersion implements resourcehandlers.LinkControl#SetVersion
func (fs *fsHandler) SetVersion(absLink, version string) (string, error) {
	return absLink, nil
}

type gitLogEntry struct {
	Sha     string
	Author  string
	Date    string
	Message string
	Email   string
	Name    string
}

type gitLogEntryAuthor struct {
}

func gitLog(path string) ([]*gitLogEntry, error) {
	var (
		log            []byte
		err            error
		errStr         string
		stdout, stderr bytes.Buffer
	)

	if _, err := os.Stat(path); err != nil {
		return nil, err
	}

	git := exec.Command("git", "log", "--date=short", `--pretty=format:'{%n  "sha": "%H",%n  "author": "%aN <%aE>",%n  "date": "%ad",%n  "message": "%s",%n  "email": "%aE",%n  "name": "%aN"%n },'`, "--follow", path)
	git.Stdout = &stdout
	git.Stderr = &stderr
	if err = git.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, err
		}
	}
	log, errStr = stdout.Bytes(), string(stderr.Bytes())
	if len(errStr) > 0 {
		return nil, fmt.Errorf("failed to execute git log for %s:\n%s", path, errStr)
	}

	logS := string(log)
	logS = strings.ReplaceAll(logS, "'{", "{")
	logS = strings.ReplaceAll(logS, "},'", "},")
	if strings.HasSuffix(logS, ",") {
		logS = logS[:len(logS)-1]
	}
	logS = fmt.Sprintf("[%s]", logS)

	gitLog := []*gitLogEntry{}
	if err := json.Unmarshal([]byte(logS), &gitLog); err != nil {
		return nil, err
	}
	return gitLog, nil
}

func checkGitExists() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

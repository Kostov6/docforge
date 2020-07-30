package reactor

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/google/go-github/v32/github"
)

// GitHubWorker specializes in processing remote GitHub resources
type GitHubWorker struct {
	Client *github.Client
}

// GitHubTask is a unit of work specification that is processed by Worker
type GitHubTask struct {
	owner      string
	repository string
	entrySHA   string
	entryPath  string
	parentDir  string
}

// NewGitHubTask creates task for a Worker to execute
func NewGitHubTask(parentDir, owner, repository, entrySHA, entryPath string) *GitHubTask {
	return &GitHubTask{
		parentDir:  parentDir,
		owner:      owner,
		repository: repository,
		entrySHA:   entrySHA,
		entryPath:  entryPath,
	}
}

// Work implements Worker#Work function
func (b *GitHubWorker) Work(ctx context.Context, task interface{}) *WorkerError {
	if task, ok := task.(*GitHubTask); ok {
		if err := createBlobFromTask(ctx, b.Client, task); err != nil {
			return &WorkerError{
				error: err,
			}
		}
	}
	return nil
}

func createBlobFromTask(ctx context.Context, client *github.Client, t *GitHubTask) error {
	blob, response, err := client.Git.GetBlobRaw(ctx, t.owner, t.repository, t.entrySHA)
	if err != nil {
		return err
	}

	if response.StatusCode != 200 {
		return fmt.Errorf("not 200 status code returned, but %d. Failed to get file tree", response.StatusCode)
	}

	filePath := filepath.Join(t.parentDir, t.entryPath)
	parents := filepath.Dir(filePath)
	if _, err := os.Stat(parents); os.IsNotExist(err) {
		if err = os.MkdirAll(parents, os.ModePerm); err != nil {
			return err
		}
	}

	return ioutil.WriteFile(filePath, blob, 0644)
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

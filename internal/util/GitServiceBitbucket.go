package util

import (
	"fmt"
	"github.com/devtron-labs/devtron/internal/sql/repository"
	"github.com/go-pg/pg"
	"github.com/ktrysmt/go-bitbucket"
	"go.uber.org/zap"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"time"
)

const (
	BITBUCKET_CLONE_BASE_URL       = "https://bitbucket.org/"
	BITBUCKET_GITOPS_DIR           = "bitbucketGitOps"
	BITBUCKET_REPO_NOT_FOUND_ERROR = "404 Not Found"
	BITBUCKET_COMMIT_TIME_LAYOUT   = "2001-01-01T10:00:00+00:00"
)

type GitBitbucketClient struct {
	client                 *bitbucket.Client
	logger                 *zap.SugaredLogger
	gitService             GitService
	gitOpsConfigRepository repository.GitOpsConfigRepository
}

func NewGitBitbucketClient(username, token, host string, logger *zap.SugaredLogger, gitService GitService,
	gitOpsConfigRepository repository.GitOpsConfigRepository) GitBitbucketClient {
	coreClient := bitbucket.NewBasicAuth(username, token)
	logger.Infow("bitbucket client created", "clientDetails", coreClient)
	return GitBitbucketClient{
		client:                 coreClient,
		logger:                 logger,
		gitService:             gitService,
		gitOpsConfigRepository: gitOpsConfigRepository,
	}
}

func (impl GitBitbucketClient) DeleteRepository(name string) error {
	gitOpsConfigBitbucket, err := impl.gitOpsConfigRepository.GetGitOpsConfigByProvider(BITBUCKET_PROVIDER)
	if err != nil {
		if err == pg.ErrNoRows {
			gitOpsConfigBitbucket = &repository.GitOpsConfig{}
			gitOpsConfigBitbucket.BitBucketWorkspaceId = ""
			gitOpsConfigBitbucket.BitBucketProjectKey = ""
		} else {
			impl.logger.Errorw("error in fetching gitOps bitbucket config", "err", err)
			return err
		}
	}
	repoOptions := &bitbucket.RepositoryOptions{
		Owner:     gitOpsConfigBitbucket.BitBucketWorkspaceId,
		RepoSlug:  name,
		IsPrivate: "true",
		Project:   gitOpsConfigBitbucket.BitBucketProjectKey,
	}
	_, err = impl.client.Repositories.Repository.Delete(repoOptions)
	if err != nil {
		impl.logger.Errorw("error in deleting repo gitlab", "repoName", repoOptions.RepoSlug, "err", err)
	}
	return err
}

func (impl GitBitbucketClient) GetRepoUrl(repoName string) (repoUrl string, err error) {
	gitOpsConfigBitbucket, err := impl.gitOpsConfigRepository.GetGitOpsConfigByProvider(BITBUCKET_PROVIDER)
	if err != nil {
		if err == pg.ErrNoRows {
			gitOpsConfigBitbucket = &repository.GitOpsConfig{}
			gitOpsConfigBitbucket.BitBucketWorkspaceId = ""
			gitOpsConfigBitbucket.BitBucketProjectKey = ""
		} else {
			impl.logger.Errorw("error in fetching gitOps bitbucket config", "err", err)
			return "", err
		}
	}
	repoOptions := &bitbucket.RepositoryOptions{
		Owner:    gitOpsConfigBitbucket.BitBucketWorkspaceId,
		Project:  gitOpsConfigBitbucket.BitBucketProjectKey,
		RepoSlug: repoName,
	}
	_, exists, err := impl.repoExists(repoOptions)
	if err != nil {
		return "", err
	} else if !exists {
		return "", fmt.Errorf("%s :repo not found", repoOptions.RepoSlug)
	} else {
		repoUrl = fmt.Sprintf(BITBUCKET_CLONE_BASE_URL+"%s/%s.git", repoOptions.Owner, repoOptions.RepoSlug)
		return repoUrl, nil
	}
}
func (impl GitBitbucketClient) CreateRepository(name, description, userName, userEmailId string) (url string, isNew bool, detailedErrorGitOpsConfigActions DetailedErrorGitOpsConfigActions) {
	detailedErrorGitOpsConfigActions.StageErrorMap = make(map[string]error)

	gitOpsConfigBitbucket, err := impl.gitOpsConfigRepository.GetGitOpsConfigByProvider(BITBUCKET_PROVIDER)
	if err != nil {
		if err == pg.ErrNoRows {
			gitOpsConfigBitbucket = &repository.GitOpsConfig{}
			gitOpsConfigBitbucket.BitBucketWorkspaceId = ""
			gitOpsConfigBitbucket.BitBucketProjectKey = ""
		} else {
			impl.logger.Errorw("error in fetching gitOps bitbucket config", "err", err)
			return "", false, detailedErrorGitOpsConfigActions
		}
	}
	workSpaceId := gitOpsConfigBitbucket.BitBucketWorkspaceId
	projectKey := gitOpsConfigBitbucket.BitBucketProjectKey
	repoOptions := &bitbucket.RepositoryOptions{
		Owner:       workSpaceId,
		RepoSlug:    name,
		Scm:         "git",
		IsPrivate:   "true",
		Description: description,
		Project:     projectKey,
	}
	repoUrl, repoExists, err := impl.repoExists(repoOptions)
	if err != nil {
		impl.logger.Errorw("error in communication with bitbucket", "err", err)
		detailedErrorGitOpsConfigActions.StageErrorMap[GetRepoUrlStage] = err
		return "", false, detailedErrorGitOpsConfigActions
	}
	if repoExists {
		detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, GetRepoUrlStage)
		return repoUrl, false, detailedErrorGitOpsConfigActions
	}
	_, err = impl.client.Repositories.Repository.Create(repoOptions)
	if err != nil {
		impl.logger.Errorw("error in creating repo bitbucket", "project", name, "err", err)
		detailedErrorGitOpsConfigActions.StageErrorMap[CreateRepoStage] = err
		return "", true, detailedErrorGitOpsConfigActions
	}
	repoUrl = fmt.Sprintf(BITBUCKET_CLONE_BASE_URL+"%s/%s.git", repoOptions.Owner, repoOptions.RepoSlug)
	logger.Infow("repo created ", "repoUrl", repoUrl)
	detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, CreateRepoStage)

	validated, err := impl.ensureProjectAvailabilityOnHttp(repoOptions)
	if err != nil {
		impl.logger.Errorw("error in ensuring project availability bitbucket", "repoName", repoOptions.RepoSlug, "err", err)
		detailedErrorGitOpsConfigActions.StageErrorMap[CloneHttpStage] = err
		return "", true, detailedErrorGitOpsConfigActions
	}
	if !validated {
		detailedErrorGitOpsConfigActions.StageErrorMap[CloneHttpStage] = fmt.Errorf("unable to validate project:%s in given time", name)
		return "", true, detailedErrorGitOpsConfigActions
	}
	detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, CloneHttpStage)

	_, err = impl.CreateReadme(repoOptions.RepoSlug, userName, userEmailId)
	if err != nil {
		impl.logger.Errorw("error in creating readme bitbucket", "repoName", repoOptions.RepoSlug, "err", err)
		detailedErrorGitOpsConfigActions.StageErrorMap[CreateReadmeStage] = err
		return "", true, detailedErrorGitOpsConfigActions
	}
	detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, CreateReadmeStage)

	validated, err = impl.ensureProjectAvailabilityOnSsh(repoOptions)
	if err != nil {
		impl.logger.Errorw("error in ensuring project availability bitbucket", "project", name, "err", err)
		detailedErrorGitOpsConfigActions.StageErrorMap[CloneSshStage] = err
		return "", true, detailedErrorGitOpsConfigActions
	}
	if !validated {
		detailedErrorGitOpsConfigActions.StageErrorMap[CloneSshStage] = fmt.Errorf("unable to validate project:%s in given time", name)
		return "", true, detailedErrorGitOpsConfigActions
	}
	detailedErrorGitOpsConfigActions.SuccessfulStages = append(detailedErrorGitOpsConfigActions.SuccessfulStages, CloneSshStage)
	return repoUrl, true, detailedErrorGitOpsConfigActions
}

func (impl GitBitbucketClient) repoExists(repoOptions *bitbucket.RepositoryOptions) (repoUrl string, exists bool, err error) {
	repo, err := impl.client.Repositories.Repository.Get(repoOptions)
	if repo == nil && err.Error() == BITBUCKET_REPO_NOT_FOUND_ERROR {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	repoUrl = fmt.Sprintf(BITBUCKET_CLONE_BASE_URL+"%s/%s.git", repoOptions.Owner, repoOptions.RepoSlug)
	return repoUrl, true, nil
}
func (impl GitBitbucketClient) ensureProjectAvailabilityOnHttp(repoOptions *bitbucket.RepositoryOptions) (bool, error) {
	for count := 0; count < 5; count++ {
		_, exists, err := impl.repoExists(repoOptions)
		if err == nil && exists {
			impl.logger.Infow("repo validated successfully on https")
			return true, nil
		} else if err != nil {
			impl.logger.Errorw("error in validating repo bitbucket", "repoDetails", repoOptions, "err", err)
			return false, err
		} else {
			impl.logger.Errorw("repo not available on http", "repoDetails", repoOptions)
		}
		time.Sleep(10 * time.Second)
	}
	return false, nil
}
func (impl GitBitbucketClient) CreateReadme(repoName, userName, userEmailId string) (string, error) {
	cfg := &ChartConfig{
		ChartName:      repoName,
		ChartLocation:  "",
		FileName:       "README.md",
		FileContent:    "@devtron",
		ReleaseMessage: "pushing readme",
		ChartRepoName:  repoName,
		UserName:       userName,
		UserEmailId:    userEmailId,
	}
	hash, _, err := impl.CommitValues(cfg)
	if err != nil {
		impl.logger.Errorw("error in creating readme bitbucket", "repo", repoName, "err", err)
	}
	return hash, err
}
func (impl GitBitbucketClient) ensureProjectAvailabilityOnSsh(repoOptions *bitbucket.RepositoryOptions) (bool, error) {
	repoUrl := fmt.Sprintf(BITBUCKET_CLONE_BASE_URL+"%s/%s.git", repoOptions.Owner, repoOptions.RepoSlug)
	for count := 0; count < 5; count++ {
		_, err := impl.gitService.Clone(repoUrl, fmt.Sprintf("/ensure-clone/%s", repoOptions.RepoSlug))
		if err == nil {
			impl.logger.Infow("ensureProjectAvailability clone passed bitbucket", "try count", count, "repoUrl", repoUrl)
			return true, nil
		}
		impl.logger.Errorw("ensureProjectAvailability clone failed ssh bitbucket", "try count", count, "err", err)
		time.Sleep(10 * time.Second)
	}
	return false, nil
}

func (impl GitBitbucketClient) CommitValues(config *ChartConfig) (commitHash string, commitTime time.Time, err error) {
	gitOpsConfigBitbucket, err := impl.gitOpsConfigRepository.GetGitOpsConfigByProvider(BITBUCKET_PROVIDER)
	if err != nil {
		if err == pg.ErrNoRows {
			gitOpsConfigBitbucket = &repository.GitOpsConfig{}
			gitOpsConfigBitbucket.BitBucketWorkspaceId = ""
			gitOpsConfigBitbucket.BitBucketProjectKey = ""
		} else {
			impl.logger.Errorw("error in fetching gitOps bitbucket config", "err", err)
			return "", time.Time{}, err
		}
	}
	bitbucketWorkspaceId := gitOpsConfigBitbucket.BitBucketWorkspaceId
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", time.Time{}, err
	}
	bitbucketGitOpsDirPath := path.Join(homeDir, BITBUCKET_GITOPS_DIR)
	if _, err = os.Stat(bitbucketGitOpsDirPath); !os.IsExist(err) {
		os.Mkdir(bitbucketGitOpsDirPath, 0777)
	}

	bitbucketCommitFilePath := path.Join(bitbucketGitOpsDirPath, config.FileName)

	if _, err = os.Stat(bitbucketCommitFilePath); os.IsExist(err) {
		os.Remove(bitbucketCommitFilePath)
	}

	err = ioutil.WriteFile(bitbucketCommitFilePath, []byte(config.FileContent), 0666)
	if err != nil {
		return "", time.Time{}, err
	}
	fileName := filepath.Join(config.ChartLocation, config.FileName)

	//bitbucket needs author as - "Name <email-Id>"
	authorBitbucket := fmt.Sprintf("%s <%s>", config.UserName, config.UserEmailId)
	repoWriteOptions := &bitbucket.RepositoryBlobWriteOptions{
		Owner:    bitbucketWorkspaceId,
		RepoSlug: config.ChartRepoName,
		FilePath: bitbucketCommitFilePath,
		FileName: fileName,
		Message:  config.ReleaseMessage,
		Branch:   "master",
		Author:   authorBitbucket,
	}
	err = impl.client.Repositories.Repository.WriteFileBlob(repoWriteOptions)
	_ = os.Remove(bitbucketCommitFilePath)
	if err != nil {
		return "", time.Time{}, err
	}
	commitOptions := &bitbucket.CommitsOptions{
		RepoSlug:    config.ChartRepoName,
		Owner:       bitbucketWorkspaceId,
		Branchortag: "master",
	}
	commits, err := impl.client.Repositories.Commits.GetCommits(commitOptions)
	if err != nil {
		return "", time.Time{}, err
	}

	//extracting the latest commit hash from the paginated api response of above method, reference of api & response - https://developer.atlassian.com/bitbucket/api/2/reference/resource/repositories/%7Bworkspace%7D/%7Brepo_slug%7D/commits
	commitHash = commits.(map[string]interface{})["values"].([]interface{})[0].(map[string]interface{})["hash"].(string)
	commitTimeString := commits.(map[string]interface{})["values"].([]interface{})[0].(map[string]string)["date"]
	commitTime, err = time.Parse(BITBUCKET_COMMIT_TIME_LAYOUT, commitTimeString)
	if err != nil {
		impl.logger.Errorw("error in getting commitTime", "err", err)
		return "", time.Time{}, err
	}
	return commitHash, commitTime, nil
}

func (impl GitBitbucketClient) GetCommits(repoName, projectName string) ([]*GitCommitDto, error) {
	gitOpsConfigBitbucket, err := impl.gitOpsConfigRepository.GetGitOpsConfigByProvider(BITBUCKET_PROVIDER)
	if err != nil {
		if err == pg.ErrNoRows {
			gitOpsConfigBitbucket = &repository.GitOpsConfig{}
			gitOpsConfigBitbucket.BitBucketWorkspaceId = ""
			gitOpsConfigBitbucket.BitBucketProjectKey = ""
		} else {
			impl.logger.Errorw("error in fetching gitOps bitbucket config", "err", err)
			return nil, err
		}
	}
	bitbucketWorkspaceId := gitOpsConfigBitbucket.BitBucketWorkspaceId
	bitbucketClient := impl.client
	getCommitsOptions := &bitbucket.CommitsOptions{
		RepoSlug:    repoName,
		Owner:       bitbucketWorkspaceId,
		Branchortag: "master",
	}
	gitCommitsIf, err := bitbucketClient.Repositories.Commits.GetCommits(getCommitsOptions)
	if err != nil {
		impl.logger.Errorw("error in getting commits", "err", err, "repoName", repoName)
		return nil, err
	}

	gitCommits := gitCommitsIf.(map[string]interface{})["values"].([]interface{})
	var gitCommitsDto []*GitCommitDto
	for _, gitCommit := range gitCommits {

		commitHash := gitCommit.(map[string]string)["hash"]
		commitTime, err := time.Parse(BITBUCKET_COMMIT_TIME_LAYOUT, gitCommit.(map[string]string)["date"])
		if err != nil {
			impl.logger.Errorw("error in getting commitTime", "err", err, "gitCommit", gitCommit)
			return nil, err
		}
		gitCommitDto := &GitCommitDto{
			CommitHash: commitHash,
			CommitTime: commitTime,
		}
		gitCommitsDto = append(gitCommitsDto, gitCommitDto)
	}
	return gitCommitsDto, nil
}

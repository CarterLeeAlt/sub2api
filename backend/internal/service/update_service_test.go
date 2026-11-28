//go:build unit

package service

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpdateExtractsWindowsReleaseBinary(t *testing.T) {
	for _, name := range []string{"sub2api.exe", "./sub2api.exe", "README.md"} {
		dir := t.TempDir()
		archivePath := filepath.Join(dir, "sub2api_windows_amd64.zip")
		file, err := os.Create(archivePath)
		require.NoError(t, err)
		archive := zip.NewWriter(file)
		entry, err := archive.Create(name)
		require.NoError(t, err)
		_, err = entry.Write([]byte("fixture executable"))
		require.NoError(t, err)
		require.NoError(t, archive.Close())
		require.NoError(t, file.Close())
		dest := filepath.Join(dir, "new-binary")
		err = (&UpdateService{}).extractBinary(archivePath, dest)
		if name == "README.md" {
			require.ErrorContains(t, err, "not found")
			require.NoFileExists(t, dest)
			continue
		}
		require.NoError(t, err)
		body, err := os.ReadFile(dest)
		require.NoError(t, err)
		require.Equal(t, "fixture executable", string(body))
	}
}

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release        *GitHubRelease
	recentReleases []*GitHubRelease
	recentErr      error
	latestErr      error
	latestRepo     string
	latestCalls    int
	recentRepo     string
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.latestRepo = repo
	s.latestCalls++
	return s.release, s.latestErr
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(_ context.Context, repo string, _ int) ([]*GitHubRelease, error) {
	s.recentRepo = repo
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("DownloadFile should not be called when no update is available")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	panic("FetchChecksumFile should not be called when no update is available")
}

func TestUpdateServiceUpstreamWithoutReleaseIsExplicitAndCached(t *testing.T) {
	client := &updateServiceGitHubClientStub{latestErr: ErrNoPublishedRelease}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, "0.2.21", "release")
	info, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, "Wei-Shaw/sub2api", client.latestRepo)
	require.Equal(t, "no_release", info.WarningCode)
	require.Contains(t, info.Warning, "Wei-Shaw/sub2api")
	require.False(t, info.HasUpdate)
	require.Nil(t, info.ReleaseInfo)
	cached, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.True(t, cached.Cached)
	require.Equal(t, info.WarningCode, cached.WarningCode)
	require.Equal(t, info.Warning, cached.Warning)
	require.Equal(t, 1, client.latestCalls)
	require.ErrorIs(t, svc.PerformUpdate(context.Background()), ErrNoPublishedRelease)
	require.Equal(t, "CarterLeeAlt/sub2api", client.latestRepo)
	versions, err := svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.Empty(t, versions)
	require.Equal(t, "CarterLeeAlt/sub2api", client.recentRepo)
}

func TestUpdateServiceUpstreamNoticeDoesNotSelectForkInstallAssets(t *testing.T) {
	client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.2.22", HTMLURL: "https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.22"}}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, "0.2.21", "release")
	info, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, "Wei-Shaw/sub2api", client.latestRepo)
	require.True(t, info.HasUpdate)
	require.Equal(t, "0.2.22", info.LatestVersion)
	require.Equal(t, client.release.HTMLURL, info.ReleaseInfo.HTMLURL)

	// A cached upstream update must never become an install candidate.
	client.release = &GitHubRelease{TagName: "v0.2.21"}
	require.ErrorIs(t, svc.PerformUpdate(context.Background()), ErrNoUpdateAvailable)
	require.Equal(t, "CarterLeeAlt/sub2api", client.latestRepo)
	require.Equal(t, 2, client.latestCalls)
	_, err = svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.Equal(t, "CarterLeeAlt/sub2api", client.recentRepo)
}

func TestUpdateServiceUpstreamComparisonAndCache(t *testing.T) {
	for _, current := range []string{"0.2.20", "0.2.21", "0.2.22-custom"} {
		t.Run(current, func(t *testing.T) {
			client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.2.21"}}
			svc := NewUpdateService(&updateServiceCacheStub{}, client, current, "source")
			info, err := svc.CheckUpdate(context.Background(), false)
			require.NoError(t, err)
			require.Equal(t, current == "0.2.20", info.HasUpdate)
			require.Empty(t, info.Warning)
			cached, err := svc.CheckUpdate(context.Background(), false)
			require.NoError(t, err)
			require.True(t, cached.Cached)
			require.Equal(t, info.HasUpdate, cached.HasUpdate)
			require.Equal(t, 1, client.latestCalls)
		})
	}
}

func TestUpdateServiceCheckFailureAndRecovery(t *testing.T) {
	client := &updateServiceGitHubClientStub{latestErr: errors.New("network unavailable")}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, "0.2.21", "source")
	info, err := svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, "check_failed", info.WarningCode)
	require.False(t, info.Cached)
	require.False(t, info.HasUpdate)
	require.Nil(t, info.ReleaseInfo)

	client.latestErr = nil
	client.release = &GitHubRelease{TagName: "v0.2.22"}
	info, err = svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.True(t, info.HasUpdate)
	require.Empty(t, info.WarningCode)

	client.latestErr = errors.New("network unavailable")
	info, err = svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, "check_failed", info.WarningCode)
	require.Contains(t, info.Warning, "network unavailable")
	require.True(t, info.Cached)
	require.True(t, info.HasUpdate)
	require.Equal(t, "0.2.22", info.LatestVersion)

	client.latestErr = nil
	client.release = &GitHubRelease{TagName: "v0.2.23"}
	info, err = svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.Empty(t, info.Warning)
	require.Empty(t, info.WarningCode)
	require.False(t, info.Cached)
	require.Equal(t, "0.2.23", info.LatestVersion)
}

func TestUpdateServiceFailedRefreshDoesNotReportCachedNoRelease(t *testing.T) {
	client := &updateServiceGitHubClientStub{latestErr: ErrNoPublishedRelease}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, "0.2.21", "source")
	_, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	client.latestErr = errors.New("network unavailable")
	info, err := svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, "check_failed", info.WarningCode)
	require.True(t, info.Cached)
	require.False(t, info.HasUpdate)
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{
			release: &GitHubRelease{
				TagName: "v0.1.132",
				Name:    "v0.1.132",
			},
		},
		"0.1.132",
		"release",
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		current,
		"release",
	)
}

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148", PublishedAt: "2026-07-09T00:00:00Z"},                       // newer than current: excluded
		{TagName: "v0.1.147", PublishedAt: "2026-07-08T00:00:00Z"},                       // current: excluded
		{TagName: "v0.1.146-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		{TagName: "v0.1.146", PublishedAt: "2026-07-07T00:00:00Z"},
		{TagName: "v0.1.145", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"},
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"}, // duplicate: excluded
		{TagName: "v0.1.143", PublishedAt: "2026-07-04T00:00:00Z"},
		{TagName: "v0.1.142", PublishedAt: "2026-07-03T00:00:00Z"}, // beyond cap of 3: excluded
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.144", versions[1].Version)
	require.Equal(t, "0.1.143", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.144"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.145", versions[1].Version)
	require.Equal(t, "0.1.144", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.148"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		"0.1.147",
		"release",
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148"},
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
		{TagName: "v0.1.144"},
		{TagName: "v0.1.143"},
		{TagName: "v0.1.142"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	for _, target := range []string{
		"",         // empty
		"0.1.147",  // current version
		"v0.1.147", // current version with prefix
		"0.1.148",  // newer than current
		"0.1.142",  // older than the 3 most recent
		"9.9.9",    // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	// No platform asset in the release: the target passes the allowlist check
	// and fails later at asset lookup, proving the version itself was accepted.
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	err := svc.RollbackToVersion(context.Background(), "v0.1.146")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	require.Contains(t, err.Error(), "no compatible release found")
}
